package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/crypt0rr/artist-tracker/internal/logging"
)

// maxQueuedManualSyncRequests prevents repeated manual-sync actions from
// starving normal scheduled work. Requests for the same artist are still
// coalesced by CreateManualSyncRequest before this global cap is checked.
const maxQueuedManualSyncRequests = 100

func (s *Store) InsertApplicationLog(ctx context.Context, entry logging.Entry) error {
	attrs, err := json.Marshal(entry.Attributes)
	if err != nil {
		return err
	}
	level := strings.ToUpper(entry.Level)
	if level == "WARNING" {
		level = "WARN"
	}
	if level != "INFO" && level != "WARN" && level != "ERROR" {
		return nil
	}
	_, err = s.execWriteContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, entry.Time.UTC().Format(time.RFC3339Nano), level, entry.Message, string(attrs))
	return err
}

const (
	backupMarkerFile  = ".artist-trackarr-last-backup"
	restoreMarkerFile = ".artist-trackarr-last-restore"
)

// operationalMarker reads a deliberately small, operator-written marker. The
// marker is not a source of truth for application data; it only lets the
// diagnostics page report whether an external backup or restore rehearsal has
// recently completed. Missing markers are normal for fresh installations.
func (s *Store) operationalMarker(name string) (*time.Time, string) {
	if strings.TrimSpace(s.dataDir) == "" {
		return nil, ""
	}
	contents, err := os.ReadFile(filepath.Join(s.dataDir, name))
	if err != nil {
		return nil, ""
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, ""
	}
	parsed, err := parseTime(strings.TrimSpace(lines[0]))
	if err != nil {
		return nil, ""
	}
	status := ""
	if len(lines) > 1 {
		status = strings.TrimSpace(lines[1])
	}
	return &parsed, status
}
func (s *Store) ApplicationLogs(ctx context.Context, limit int) ([]logging.Entry, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT created_at,level,message,attributes_json FROM application_logs ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []logging.Entry
	for rows.Next() {
		var ts, level, msg, attrs string
		if err := rows.Scan(&ts, &level, &msg, &attrs); err != nil {
			return nil, err
		}
		t, err := parseStoredTime(ts, "application log created_at")
		if err != nil {
			return nil, err
		}
		// Rows are stored canonically in UTC. Convert them to the current process
		// timezone for the web view so they match the container's local display.
		if strings.HasSuffix(ts, "Z") || strings.HasSuffix(ts, "+00:00") {
			t = t.In(time.Local)
		}
		var fields []logging.Field
		if err := json.Unmarshal([]byte(attrs), &fields); err != nil {
			return nil, fmt.Errorf("invalid persisted application log attributes: %w", err)
		}
		out = append(out, logging.Entry{Time: t, Level: level, Message: msg, Attributes: fields})
	}
	return out, rows.Err()
}
func (s *Store) PruneApplicationLogs(ctx context.Context, before time.Time) error {
	_, err := s.execWriteContext(ctx, `DELETE FROM application_logs WHERE created_at < ?`, timeText(before))
	return err
}

// Optimize refreshes SQLite's lightweight query-planner statistics. It is
// deliberately run by hourly maintenance on the serialized writer so it
// cannot contend with read-only dashboard connections or race a schema write.
func (s *Store) Optimize(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `PRAGMA optimize`)
	return err
}
func (s *Store) CreateManualSyncRequest(ctx context.Context, userID int64, scope string, artistID *int64) (ManualSyncRequest, error) {
	if scope != "artist" && scope != "retry" {
		return ManualSyncRequest{}, errors.New("invalid sync scope")
	}
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (ManualSyncRequest, error) {
		var q string
		var args []any
		if scope == "artist" {
			if artistID == nil {
				return ManualSyncRequest{}, errors.New("artist is required")
			}
			q = `SELECT id FROM manual_sync_requests WHERE scope='artist' AND artist_id=? AND status IN ('queued','running') LIMIT 1`
			args = []any{*artistID}
		} else {
			q = `SELECT id FROM manual_sync_requests WHERE scope='retry' AND status IN ('queued','running') LIMIT 1`
		}
		var existing int64
		if err := tx.QueryRowContext(ctx, q, args...).Scan(&existing); err == nil {
			return ManualSyncRequest{ID: existing, Scope: scope, Status: "queued"}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ManualSyncRequest{}, err
		}
		var queued int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM manual_sync_requests WHERE status='queued'`).Scan(&queued); err != nil {
			return ManualSyncRequest{}, err
		}
		if queued >= maxQueuedManualSyncRequests {
			return ManualSyncRequest{}, ErrManualSyncQueueFull
		}
		now := nowText()
		res, err := tx.ExecContext(ctx, `INSERT INTO manual_sync_requests(requested_by,scope,artist_id,status,created_at) VALUES(?,?,?,?,?)`, userID, scope, artistID, "queued", now)
		if err != nil {
			return ManualSyncRequest{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return ManualSyncRequest{}, err
		}
		created, err := parseStoredTime(now, "manual sync created_at")
		if err != nil {
			return ManualSyncRequest{}, err
		}
		return ManualSyncRequest{ID: id, RequestedBy: userID, Scope: scope, ArtistID: artistID, Status: "queued", CreatedAt: created}, nil
	})
}
func (s *Store) ClaimManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	return s.ClaimManualSyncRequestsWithLease(ctx, limit, "legacy-worker", 5*time.Minute)
}

// ClaimManualSyncRequestsWithLease atomically claims queued work and recovers
// running rows whose lease expired. The owner token prevents two runner
// instances from completing the same durable request concurrently.
func (s *Store) ClaimManualSyncRequestsWithLease(ctx context.Context, limit int, owner string, lease time.Duration) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 1
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "legacy-worker"
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) ([]ManualSyncRequest, error) {
		now := time.Now().UTC()
		expires := now.Add(lease)
		rows, err := tx.QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,created_at FROM manual_sync_requests
		WHERE status='queued' OR (status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
		ORDER BY id LIMIT ?`, timeText(now), limit)
		if err != nil {
			return nil, err
		}
		var ids []int64
		var out []ManualSyncRequest
		if err := func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var r ManualSyncRequest
				var aid sql.NullInt64
				var ts string
				if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &ts); err != nil {
					return err
				}
				if aid.Valid {
					v := aid.Int64
					r.ArtistID = &v
				}
				r.Status = "running"
				created, parseErr := parseStoredTime(ts, "manual sync created_at")
				if parseErr != nil {
					return parseErr
				}
				r.CreatedAt = created
				out = append(out, r)
				ids = append(ids, r.ID)
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
		nowTextValue := timeText(now)
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE manual_sync_requests SET status='running',started_at=?,lease_owner=?,lease_expires_at=?,attempt_count=attempt_count+1 WHERE id=? AND (status='queued' OR (status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))`, nowTextValue, owner, timeText(expires), id, nowTextValue); err != nil {
				return nil, err
			}
		}
		started, err := parseStoredTime(nowTextValue, "manual sync started_at")
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].StartedAt = &started
			out[i].LeaseOwner = owner
			expiresCopy := expires
			out[i].LeaseExpiresAt = &expiresCopy
		}
		return out, nil
	})
}
func (s *Store) CompleteManualSyncRequest(ctx context.Context, id int64, syncErr error) error {
	return s.CompleteManualSyncRequestOwned(ctx, id, "", syncErr)
}

func (s *Store) CompleteManualSyncRequestOwned(ctx context.Context, id int64, owner string, syncErr error) error {
	status, msg := "completed", ""
	if syncErr != nil {
		status = "failed"
		msg = syncErr.Error()
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	query := `UPDATE manual_sync_requests SET status=?,finished_at=?,last_error=?,lease_owner=NULL,lease_expires_at=NULL WHERE id=?`
	args := []any{status, nowText(), msg, id}
	if strings.TrimSpace(owner) != "" {
		query += ` AND lease_owner=?`
		args = append(args, owner)
	}
	result, err := s.execWriteContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) ManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,status,created_at,started_at,finished_at,last_error,lease_owner,lease_expires_at,attempt_count FROM manual_sync_requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ManualSyncRequest
	for rows.Next() {
		var r ManualSyncRequest
		var aid sql.NullInt64
		var c string
		var st, ft, leaseExpires, leaseOwner sql.NullString
		if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &r.Status, &c, &st, &ft, &r.LastError, &leaseOwner, &leaseExpires, &r.AttemptCount); err != nil {
			return nil, err
		}
		r.LeaseOwner = leaseOwner.String
		created, parseErr := parseStoredTime(c, "manual sync created_at")
		if parseErr != nil {
			return nil, parseErr
		}
		r.CreatedAt = created
		if aid.Valid {
			v := aid.Int64
			r.ArtistID = &v
		}
		if r.StartedAt, parseErr = parseStoredNullableTime(st, "manual sync started_at"); parseErr != nil {
			return nil, parseErr
		}
		if r.FinishedAt, parseErr = parseStoredNullableTime(ft, "manual sync finished_at"); parseErr != nil {
			return nil, parseErr
		}
		if r.LeaseExpiresAt, parseErr = parseStoredNullableTime(leaseExpires, "manual sync lease_expires_at"); parseErr != nil {
			return nil, parseErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) UpsertProviderHealth(ctx context.Context, provider string, success bool, next *time.Time, rateLimited, quota bool, lastError string) error {
	now := nowText()
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	if success {
		_, err := s.execWriteContext(ctx, `INSERT INTO provider_health(provider,last_success_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_success_at=excluded.last_success_at,last_error='',next_check_at=excluded.next_check_at,rate_limited=0,quota_exceeded=0,updated_at=excluded.updated_at`, provider, now, "", nullableTime(next), 0, 0, now)
		return err
	}
	_, err := s.execWriteContext(ctx, `INSERT INTO provider_health(provider,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_failure_at=excluded.last_failure_at,last_error=excluded.last_error,next_check_at=excluded.next_check_at,rate_limited=excluded.rate_limited,quota_exceeded=excluded.quota_exceeded,updated_at=excluded.updated_at`, provider, now, lastError, nullableTime(next), boolInt(rateLimited), boolInt(quota), now)
	return err
}

// ProviderHealthByName returns the persisted state for one provider. The
// scheduler uses the next check time to carry a provider-wide cooldown across
// process restarts, so a quota response cannot cause a fresh burst of calls
// after the container is recreated.
func (s *Store) ProviderHealthByName(ctx context.Context, provider string) (ProviderHealth, error) {
	var p ProviderHealth
	var ls, lf, n, u sql.NullString
	var rl, qe int
	err := s.readerDB().QueryRowContext(ctx, `SELECT provider,last_success_at,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at
		FROM provider_health WHERE provider=?`, provider).
		Scan(&p.Provider, &ls, &lf, &p.LastError, &n, &rl, &qe, &u)
	if err != nil {
		return p, err
	}
	if p.LastSuccessAt, err = parseStoredNullableTime(ls, "provider health last_success_at"); err != nil {
		return p, err
	}
	if p.LastFailureAt, err = parseStoredNullableTime(lf, "provider health last_failure_at"); err != nil {
		return p, err
	}
	if p.NextCheckAt, err = parseStoredNullableTime(n, "provider health next_check_at"); err != nil {
		return p, err
	}
	p.RateLimited = rl != 0
	p.QuotaExceeded = qe != 0
	p.UpdatedAt, err = parseStoredTime(u.String, "provider health updated_at")
	if err != nil {
		return p, err
	}
	return p, nil
}
func (s *Store) ProviderHealth(ctx context.Context) ([]ProviderHealth, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT provider,last_success_at,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at FROM provider_health ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProviderHealth
	for rows.Next() {
		var p ProviderHealth
		var ls, lf, n, u sql.NullString
		var rl, qe int
		if err := rows.Scan(&p.Provider, &ls, &lf, &p.LastError, &n, &rl, &qe, &u); err != nil {
			return nil, err
		}
		if p.LastSuccessAt, err = parseStoredNullableTime(ls, "provider health last_success_at"); err != nil {
			return nil, err
		}
		if p.LastFailureAt, err = parseStoredNullableTime(lf, "provider health last_failure_at"); err != nil {
			return nil, err
		}
		if p.NextCheckAt, err = parseStoredNullableTime(n, "provider health next_check_at"); err != nil {
			return nil, err
		}
		p.RateLimited = rl != 0
		p.QuotaExceeded = qe != 0
		if p.UpdatedAt, err = parseStoredTime(u.String, "provider health updated_at"); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Diagnostics returns bounded operational counters for the administrator
// support view. It deliberately omits error text and all destination/provider
// credentials so the resulting report is safe to copy into an issue.
func (s *Store) Diagnostics(ctx context.Context) (DiagnosticsSnapshot, error) {
	var snapshot DiagnosticsSnapshot
	snapshot.CheckedAt = time.Now().UTC()
	err := s.readerDB().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(version) FROM schema_migrations),0),
		(SELECT COUNT(*) FROM follows),
		(SELECT COUNT(*) FROM release_groups),
		(SELECT COUNT(*) FROM manual_sync_requests WHERE status='queued'),
		(SELECT COUNT(*) FROM manual_sync_requests WHERE status='running'),
		(SELECT COUNT(*) FROM deliveries WHERE status='pending'),
		(SELECT COUNT(*) FROM deliveries WHERE status='failed'),
		(SELECT COUNT(*) FROM application_logs WHERE created_at>=?)`,
		timeText(snapshot.CheckedAt.Add(-24*time.Hour))).
		Scan(&snapshot.SchemaVersion, &snapshot.FollowedArtists, &snapshot.Releases,
			&snapshot.QueuedSyncs, &snapshot.RunningSyncs, &snapshot.PendingDeliveries,
			&snapshot.FailedDeliveries, &snapshot.RecentLogEntries)
	if err != nil {
		return DiagnosticsSnapshot{}, err
	}
	snapshot.DatabaseHealthy = true
	var oldestDue sql.NullString
	if err := s.readerDB().QueryRowContext(ctx, `WITH due AS (
		SELECT a.id,a.next_check_at AS due_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE a.next_check_at IS NULL OR a.next_check_at<=?
		UNION ALL
		SELECT a.id,a.spotify_next_check_at AS due_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE a.spotify_id IS NOT NULL AND (a.spotify_next_check_at IS NULL OR a.spotify_next_check_at<=?)
	)
	SELECT COUNT(DISTINCT id),MIN(due_at) FROM due`, timeText(snapshot.CheckedAt), timeText(snapshot.CheckedAt)).
		Scan(&snapshot.DueSyncArtists, &oldestDue); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if snapshot.OldestDueSyncAt, err = parseStoredNullableTime(oldestDue, "oldest due artist sync"); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	var oldest sql.NullString
	if err := s.readerDB().QueryRowContext(ctx, `SELECT MIN(value) FROM (
		SELECT next_attempt_at AS value FROM deliveries WHERE status IN ('pending','blocked')
		UNION ALL SELECT next_attempt_at FROM release_digest_deliveries WHERE status IN ('pending','blocked'))`).Scan(&oldest); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if snapshot.OldestQueueAt, err = parseStoredNullableTime(oldest, "oldest queued delivery"); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	var earliestFuture sql.NullString
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*),MIN(value) FROM (
		SELECT next_attempt_at AS value FROM deliveries
		WHERE status IN ('pending','blocked') AND next_attempt_at>?
		UNION ALL
		SELECT next_attempt_at FROM release_digest_deliveries
		WHERE status IN ('pending','blocked') AND next_attempt_at>?)`,
		timeText(snapshot.CheckedAt.Add(24*time.Hour)), timeText(snapshot.CheckedAt.Add(24*time.Hour))).
		Scan(&snapshot.FutureDeliveries, &earliestFuture); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if snapshot.EarliestFutureDelivery, err = parseStoredNullableTime(earliestFuture, "earliest future delivery"); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM manual_sync_requests WHERE status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)+
		(SELECT COUNT(*) FROM deliveries WHERE status='pending' AND claim_expires_at IS NOT NULL AND claim_expires_at<=?)+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE status='pending' AND claim_expires_at IS NOT NULL AND claim_expires_at<=?),
		(SELECT COUNT(*) FROM destination_health WHERE status='paused'),
		(SELECT COUNT(*) FROM provider_health WHERE last_failure_at IS NOT NULL AND (last_success_at IS NULL OR last_failure_at>last_success_at)),
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE status IN ('pending','blocked'))`, timeText(snapshot.CheckedAt), timeText(snapshot.CheckedAt), timeText(snapshot.CheckedAt)).
		Scan(&snapshot.StaleClaims, &snapshot.PausedDestinations, &snapshot.ProviderFailures, &snapshot.DigestBacklog); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	var oldestProviderFailure, oldestDigestBacklog sql.NullString
	if err := s.readerDB().QueryRowContext(ctx, `SELECT
		(SELECT MIN(last_failure_at) FROM provider_health
		 WHERE last_failure_at IS NOT NULL AND (last_success_at IS NULL OR last_failure_at>last_success_at)),
		(SELECT MIN(r.created_at) FROM release_digest_deliveries dd
		 JOIN release_digest_runs r ON r.id=dd.run_id
		 WHERE dd.status IN ('pending','blocked'))`).Scan(&oldestProviderFailure, &oldestDigestBacklog); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if snapshot.OldestProviderFailureAt, err = parseStoredNullableTime(oldestProviderFailure, "oldest provider failure"); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if snapshot.OldestDigestBacklogAt, err = parseStoredNullableTime(oldestDigestBacklog, "oldest digest backlog"); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	var pageCount, pageSize, freePages int64
	if err := s.readerDB().QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return DiagnosticsSnapshot{}, err
	}
	snapshot.DatabaseBytes = pageCount * pageSize
	snapshot.DatabaseFreeBytes = freePages * pageSize
	snapshot.LastBackupAt, _ = s.operationalMarker(backupMarkerFile)
	snapshot.LastRestoreAt, snapshot.LastRestoreResult = s.operationalMarker(restoreMarkerFile)
	health, err := s.ProviderHealth(ctx)
	if err != nil {
		return DiagnosticsSnapshot{}, err
	}
	snapshot.Providers = make([]DiagnosticsProvider, 0, len(health))
	for _, provider := range health {
		snapshot.Providers = append(snapshot.Providers, DiagnosticsProvider{
			Provider:    provider.Provider,
			Status:      ProviderHealthStatus(provider, snapshot.CheckedAt, s.providerHealthStaleAfter(provider.Provider)),
			NextCheckAt: provider.NextCheckAt,
		})
	}
	return snapshot, nil
}
func (s *Store) MarkAllArtistsDue(ctx context.Context) error {
	_, err := s.execWriteContext(ctx, `UPDATE artists SET next_check_at=? WHERE id IN (SELECT DISTINCT artist_id FROM follows)`, nowText())
	return err
}
func (s *Store) ArtistByID(ctx context.Context, id int64) (Artist, error) {
	var a Artist
	var sid, surl, simg, checked, next sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,spotify_id,spotify_url,spotify_image_url,last_checked_at,spotify_next_check_at FROM artists WHERE id=?`, id).Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation, &sid, &surl, &simg, &checked, &next)
	if err != nil {
		return a, err
	}
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, simg.String
	if a.LastCheckedAt, err = parseStoredNullableTime(checked, "artist last_checked_at"); err != nil {
		return a, err
	}
	if a.SpotifyNextCheckAt, err = parseStoredNullableTime(next, "artist spotify_next_check_at"); err != nil {
		return a, err
	}
	return a, err
}
func (s *Store) AdminArtists(ctx context.Context) ([]AdminArtist, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.name,a.mbid FROM artists a JOIN follows f ON f.artist_id=a.id ORDER BY a.sort_name,a.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminArtist
	for rows.Next() {
		var a AdminArtist
		if err := rows.Scan(&a.ID, &a.Name, &a.MBID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
