package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	_, err = s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, entry.Time.UTC().Format(time.RFC3339Nano), level, entry.Message, string(attrs))
	return err
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
		t, _ := parseTime(ts)
		// Rows are stored canonically in UTC. Convert them to the current process
		// timezone for the web view so they match the container's local display.
		if strings.HasSuffix(ts, "Z") || strings.HasSuffix(ts, "+00:00") {
			t = t.In(time.Local)
		}
		var fields []logging.Field
		_ = json.Unmarshal([]byte(attrs), &fields)
		out = append(out, logging.Entry{Time: t, Level: level, Message: msg, Attributes: fields})
	}
	return out, rows.Err()
}
func (s *Store) PruneApplicationLogs(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM application_logs WHERE created_at < ?`, timeText(before))
	return err
}
func (s *Store) CreateManualSyncRequest(ctx context.Context, userID int64, scope string, artistID *int64) (ManualSyncRequest, error) {
	if scope != "artist" && scope != "retry" {
		return ManualSyncRequest{}, errors.New("invalid sync scope")
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return ManualSyncRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
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
	} else if err != sql.ErrNoRows {
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
	if err := tx.Commit(); err != nil {
		return ManualSyncRequest{}, err
	}
	created, _ := parseTime(now)
	return ManualSyncRequest{ID: id, RequestedBy: userID, Scope: scope, ArtistID: artistID, Status: "queued", CreatedAt: created}, nil
}
func (s *Store) ClaimManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,created_at FROM manual_sync_requests WHERE status='queued' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	var out []ManualSyncRequest
	for rows.Next() {
		var r ManualSyncRequest
		var aid sql.NullInt64
		var ts string
		if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &ts); err != nil {
			return nil, err
		}
		if aid.Valid {
			v := aid.Int64
			r.ArtistID = &v
		}
		r.Status = "running"
		r.CreatedAt, _ = parseTime(ts)
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := nowText()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE manual_sync_requests SET status='running',started_at=? WHERE id=?`, now, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i := range out {
		t, _ := parseTime(now)
		out[i].StartedAt = &t
	}
	return out, nil
}
func (s *Store) CompleteManualSyncRequest(ctx context.Context, id int64, syncErr error) error {
	status, msg := "completed", ""
	if syncErr != nil {
		status = "failed"
		msg = syncErr.Error()
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE manual_sync_requests SET status=?,finished_at=?,last_error=? WHERE id=?`, status, nowText(), msg, id)
	return err
}
func (s *Store) ManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,status,created_at,started_at,finished_at,last_error FROM manual_sync_requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ManualSyncRequest
	for rows.Next() {
		var r ManualSyncRequest
		var aid sql.NullInt64
		var c string
		var st, ft sql.NullString
		if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &r.Status, &c, &st, &ft, &r.LastError); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = parseTime(c)
		if aid.Valid {
			v := aid.Int64
			r.ArtistID = &v
		}
		if st.Valid {
			v, _ := parseTime(st.String)
			r.StartedAt = &v
		}
		if ft.Valid {
			v, _ := parseTime(ft.String)
			r.FinishedAt = &v
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
		_, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,last_success_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_success_at=excluded.last_success_at,last_error='',next_check_at=excluded.next_check_at,rate_limited=0,quota_exceeded=0,updated_at=excluded.updated_at`, provider, now, "", nullableTime(next), 0, 0, now)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_failure_at=excluded.last_failure_at,last_error=excluded.last_error,next_check_at=excluded.next_check_at,rate_limited=excluded.rate_limited,quota_exceeded=excluded.quota_exceeded,updated_at=excluded.updated_at`, provider, now, lastError, nullableTime(next), boolInt(rateLimited), boolInt(quota), now)
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
	if ls.Valid {
		v, parseErr := parseTime(ls.String)
		if parseErr == nil {
			p.LastSuccessAt = &v
		}
	}
	if lf.Valid {
		v, parseErr := parseTime(lf.String)
		if parseErr == nil {
			p.LastFailureAt = &v
		}
	}
	if n.Valid {
		v, parseErr := parseTime(n.String)
		if parseErr == nil {
			p.NextCheckAt = &v
		}
	}
	p.RateLimited = rl != 0
	p.QuotaExceeded = qe != 0
	p.UpdatedAt, _ = parseTime(u.String)
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
		if ls.Valid {
			v, _ := parseTime(ls.String)
			p.LastSuccessAt = &v
		}
		if lf.Valid {
			v, _ := parseTime(lf.String)
			p.LastFailureAt = &v
		}
		if n.Valid {
			v, _ := parseTime(n.String)
			p.NextCheckAt = &v
		}
		p.RateLimited = rl != 0
		p.QuotaExceeded = qe != 0
		p.UpdatedAt, _ = parseTime(u.String)
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
	health, err := s.ProviderHealth(ctx)
	if err != nil {
		return DiagnosticsSnapshot{}, err
	}
	snapshot.Providers = make([]DiagnosticsProvider, 0, len(health))
	for _, provider := range health {
		snapshot.Providers = append(snapshot.Providers, DiagnosticsProvider{
			Provider:    provider.Provider,
			Status:      ProviderHealthStatus(provider, snapshot.CheckedAt, ProviderHealthStaleAfter(provider.Provider)),
			NextCheckAt: provider.NextCheckAt,
		})
	}
	return snapshot, nil
}
func (s *Store) MarkAllArtistsDue(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id IN (SELECT DISTINCT artist_id FROM follows)`, nowText())
	return err
}
func (s *Store) ArtistByID(ctx context.Context, id int64) (Artist, error) {
	var a Artist
	var sid, surl, simg, checked, next sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,spotify_id,spotify_url,spotify_image_url,last_checked_at,spotify_next_check_at FROM artists WHERE id=?`, id).Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation, &sid, &surl, &simg, &checked, &next)
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, simg.String
	if checked.Valid {
		t, _ := parseTime(checked.String)
		a.LastCheckedAt = &t
	}
	if next.Valid {
		t, _ := parseTime(next.String)
		a.SpotifyNextCheckAt = &t
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
