package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// MaxImportPayloadBytes is the upper bound for the original CSV payload kept
// with an import job. Keeping the bounded source payload makes interrupted
// jobs resumable without allowing durable state to grow beyond the upload
// limit enforced by the web layer.
const MaxImportPayloadBytes = 1 << 20

// ErrImportNotResumable indicates that an import has no retained source
// payload or is already complete. It is intentionally distinct from
// sql.ErrNoRows so callers can render a useful, non-sensitive response.
var ErrImportNotResumable = errors.New("import job is not resumable")

// ImportJob is the owner-scoped record for one CSV upload.
type ImportJob struct {
	ID              int64
	UserID          int64
	Status          string
	CreatedAt       time.Time
	FinishedAt      *time.Time
	PayloadSize     int
	CanResume       bool
	Rows            []ImportRow
	Added           int
	AlreadyFollowed int
	Invalid         int
}

// ImportRow records the independent result for one CSV data row.
type ImportRow struct {
	ID          int64
	JobID       int64
	SourceValue string
	DisplayName string
	Status      string
	ArtistID    *int64
	Reason      string
}

// ImportInput is a validated ArtistTrackarr export row. Validation belongs to
// the web package; the store deliberately accepts only the canonical fields
// needed to create a local artist and follow.
type ImportInput struct {
	SourceValue     string
	DisplayName     string
	SortName        string
	ArtistType      string
	Country         string
	Disambiguation  string
	SpotifyID       string
	SpotifyURL      string
	SpotifyImageURL string
	MBID            string
	MBURL           string
	Reason          string
}

func (s *Store) CreateImportJob(ctx context.Context, userID int64) (ImportJob, error) {
	return s.CreateImportJobWithPayload(ctx, userID, nil)
}

// CreateImportJobWithPayload creates an owner-scoped import and retains the
// bounded original upload so a process interruption can be resumed without
// asking the user to locate and upload the backup again.
func (s *Store) CreateImportJobWithPayload(ctx context.Context, userID int64, payload []byte) (ImportJob, error) {
	if len(payload) > MaxImportPayloadBytes {
		return ImportJob{}, errors.New("import payload exceeds the maximum size")
	}
	if payload == nil {
		payload = []byte{}
	}
	now := nowText()
	result, err := s.execWriteContext(ctx, `INSERT INTO import_jobs(user_id,created_at,status,payload) VALUES(?,?,?,?)`, userID, now, "processing", payload)
	if err != nil {
		return ImportJob{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ImportJob{}, err
	}
	created, err := parseStoredTime(now, "import job created_at")
	if err != nil {
		return ImportJob{}, err
	}
	return ImportJob{ID: id, UserID: userID, Status: "processing", CreatedAt: created,
		PayloadSize: len(payload)}, nil
}

// FinishImportJob records the terminal state of an upload. It is deliberately
// owner-scoped and idempotent: a retry after a completed request cannot change
// the result, while a failed request can still be marked explicitly.
func (s *Store) FinishImportJob(ctx context.Context, userID, jobID int64, status string) error {
	// "interrupted" is a terminal status too: it is what the schema and
	// CanResume use for an upload that stopped part-way and can be resumed.
	// Rejecting it here left such a job stuck in "processing", which is not
	// resumable, so the member had no route forward until an hourly sweep.
	if status != "complete" && status != "failed" && status != "interrupted" {
		return errors.New("invalid import job status")
	}
	// The source upload is needed only to recover an incomplete import. Clear
	// it as part of the same terminal transition for successful jobs so public
	// artist metadata is not retained as an unnecessary raw payload and the
	// database does not grow by the upload limit for every completed import.
	query := `UPDATE import_jobs SET status=?,finished_at=?`
	if status == "complete" {
		query += `,payload=X''`
	}
	query += ` WHERE id=? AND user_id=? AND status='processing'`
	args := []any{status, nowText(), jobID, userID}
	result, err := s.execWriteContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return changedOrNotFound(result, nil)
}

// RecoverInterruptedImportJobs marks uploads that were left processing after
// a request or process interruption. A generous stale window avoids racing a
// large, legitimate upload while ensuring an interrupted job is visible.
func (s *Store) RecoverInterruptedImportJobs(ctx context.Context, now time.Time, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}
	result, err := s.execWriteContext(ctx, `UPDATE import_jobs SET status='interrupted',finished_at=?
		WHERE status='processing' AND created_at<=?`, timeText(now), timeText(now.Add(-staleAfter)))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// SaveImportRow persists one row and, for valid input, creates or reuses the
// canonical artist and owner-scoped follow in the same transaction. This makes
// partial uploads durable without coupling them to provider availability.
func (s *Store) SaveImportRow(ctx context.Context, userID, jobID int64, input ImportInput) (ImportRow, error) {
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (ImportRow, error) {
		var owner int64
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM import_jobs WHERE id=?`, jobID).Scan(&owner); err != nil {
			return ImportRow{}, err
		}
		if owner != userID {
			return ImportRow{}, sql.ErrNoRows
		}

		row := ImportRow{JobID: jobID, SourceValue: strings.TrimSpace(input.SourceValue), DisplayName: strings.TrimSpace(input.DisplayName)}
		if input.Reason != "" {
			row.Status, row.Reason = "invalid", input.Reason
			if err := insertImportRowTx(ctx, tx, row); err != nil {
				return ImportRow{}, err
			}
			return row, nil
		}

		now := nowText()
		sortName := strings.TrimSpace(input.SortName)
		if sortName == "" {
			sortName = strings.TrimSpace(input.DisplayName)
		}
		artistResult, err := tx.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO NOTHING`,
			input.MBID, input.DisplayName, sortName, input.ArtistType, input.Country, input.Disambiguation,
			nullString(input.SpotifyID), nullString(input.SpotifyURL), nullString(input.SpotifyImageURL), now, now)
		if err != nil {
			return ImportRow{}, err
		}
		artistInserted, err := artistResult.RowsAffected()
		if err != nil {
			return ImportRow{}, err
		}
		var artistID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM artists WHERE mbid=?`, input.MBID).Scan(&artistID); err != nil {
			return ImportRow{}, err
		}
		identityStatus := "verified"
		if artistInserted > 0 {
			identityStatus = "pending"
		}
		if err := insertArtistIdentityStatusTx(ctx, tx, artistID, identityStatus); err != nil {
			return ImportRow{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`, userID, artistID, now)
		if err != nil {
			return ImportRow{}, err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return ImportRow{}, err
		}
		// Keep imported follows equivalent to follows created through the normal
		// and resolution paths. INSERT OR IGNORE repairs a legacy follow without
		// resetting an existing customized policy, and remains part of this
		// transaction so the first release sync can enqueue notifications.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follow_notification_rules
		(user_id,artist_id,delivery_mode,include_primary,include_featured,albums,eps,singles,compilations,announcements,release_day,updated_at)
		VALUES(?,?, 'inherit',1,1,1,1,1,1,1,1,?)`, userID, artistID, now); err != nil {
			return ImportRow{}, err
		}
		row.ArtistID = &artistID
		if added > 0 {
			row.Status = "added"
			// The next normal runner tick performs the regular baseline sync. It is
			// intentionally not run inline with the upload and therefore cannot
			// flood this request with provider calls.
			if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
				CASE WHEN spotify_id IS NOT NULL AND (spotify_next_check_at IS NULL OR spotify_next_check_at>?)
				THEN ? ELSE spotify_next_check_at END WHERE id=?`, now, now, now, artistID); err != nil {
				return ImportRow{}, err
			}
		} else {
			row.Status = "already_followed"
		}
		if err := insertImportRowTx(ctx, tx, row); err != nil {
			return ImportRow{}, err
		}
		return row, nil
	})
}

func insertImportRowTx(ctx context.Context, tx *sql.Tx, row ImportRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO import_rows(job_id,source_value,display_name,status,artist_id,reason)
		VALUES(?,?,?,?,?,?)`, row.JobID, row.SourceValue, row.DisplayName, row.Status, row.ArtistID, row.Reason)
	return err
}

func (s *Store) ImportJob(ctx context.Context, userID, jobID int64) (ImportJob, error) {
	var job ImportJob
	var created string
	var finished sql.NullString
	var payloadSize int64
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,user_id,status,created_at,finished_at,LENGTH(payload) FROM import_jobs WHERE id=? AND user_id=?`, jobID, userID).
		Scan(&job.ID, &job.UserID, &job.Status, &created, &finished, &payloadSize)
	if err != nil {
		return ImportJob{}, err
	}
	job.CreatedAt, err = parseStoredTime(created, "import job created_at")
	if err != nil {
		return ImportJob{}, err
	}
	if job.FinishedAt, err = parseStoredNullableTime(finished, "import job finished_at"); err != nil {
		return ImportJob{}, err
	}
	if payloadSize > 0 {
		job.PayloadSize = int(payloadSize)
		job.CanResume = job.Status == "interrupted" || job.Status == "failed"
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,job_id,source_value,display_name,status,artist_id,reason
		FROM import_rows WHERE job_id=? ORDER BY id`, jobID)
	if err != nil {
		return ImportJob{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row ImportRow
		var artistID sql.NullInt64
		if err := rows.Scan(&row.ID, &row.JobID, &row.SourceValue, &row.DisplayName, &row.Status, &artistID, &row.Reason); err != nil {
			return ImportJob{}, err
		}
		if artistID.Valid {
			id := artistID.Int64
			row.ArtistID = &id
		}
		switch row.Status {
		case "added":
			job.Added++
		case "already_followed":
			job.AlreadyFollowed++
		case "invalid":
			job.Invalid++
		}
		job.Rows = append(job.Rows, row)
	}
	return job, rows.Err()
}

// ImportJobPayload returns the retained source upload for an interrupted or
// failed owner-scoped job. The payload is never exposed cross-user and is
// copied before returning so callers cannot mutate store-owned memory.
func (s *Store) ImportJobPayload(ctx context.Context, userID, jobID int64) ([]byte, error) {
	var status string
	var payload []byte
	if err := s.readerDB().QueryRowContext(ctx, `SELECT status,payload FROM import_jobs WHERE id=? AND user_id=?`, jobID, userID).
		Scan(&status, &payload); err != nil {
		return nil, err
	}
	if status != "interrupted" && status != "failed" {
		return nil, ErrImportNotResumable
	}
	if len(payload) == 0 {
		return nil, ErrImportNotResumable
	}
	return append([]byte(nil), payload...), nil
}

// resumableImportJob matches an import job whose retained payload is the only
// thing that makes the documented Resume action work. Migration 034 exists to
// keep it, so every sweep that deletes by age must exclude it.
//
// One definition on purpose: this predicate previously existed as separate
// hand-written copies in the unattended sweep, the manual cleanup, and the
// prunable-count query. They drifted - the guard was added to two of them and
// not the third - so the scheduler deleted resumable imports while the admin
// dry-run reported nothing to delete.
const resumableImportJob = `(status IN ('interrupted','failed') AND length(payload) > 0)`

// PruneExpiredState removes only expired or completed transient state. It
// deliberately leaves active sessions, current login blocks, queued work, and
// notification/delivery history untouched.
type MaintenanceStats struct {
	Sessions      int64
	AuthTokens    int64
	LoginAttempts int64
	ManualSyncs   int64
	ImportJobs    int64
}

func (s *Store) PruneExpiredState(ctx context.Context, now time.Time) (MaintenanceStats, error) {
	policy := s.retention()
	cutoff := now.Add(-24 * time.Hour)
	manualCutoff := now.Add(-time.Duration(policy.TransientStateDays) * 24 * time.Hour)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM sessions WHERE expires_at < ?`, []any{timeText(now)}},
		{`DELETE FROM auth_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, []any{timeText(now)}},
		{`DELETE FROM login_attempts WHERE first_at < ?`, []any{timeText(cutoff)}},
		{`DELETE FROM manual_sync_requests WHERE status IN ('completed','failed') AND finished_at IS NOT NULL AND finished_at < ?`, []any{timeText(manualCutoff)}},
		// Keep interrupted and failed jobs that still carry a payload. That
		// payload is why migration 034 retains it, and deleting it removes the
		// Resume import action. CleanupRetention carries the same guard; this
		// is the path that runs unattended on every tick, so the two must agree
		// or the admin dry-run reports nothing while this sweep deletes.
		{`DELETE FROM import_jobs WHERE created_at < ?
			AND NOT ` + resumableImportJob, []any{timeText(manualCutoff)}},
	}
	result, err := withWriteTxResult(s, ctx, func(tx *sql.Tx) (MaintenanceStats, error) {
		var stats MaintenanceStats
		counts := []*int64{&stats.Sessions, &stats.AuthTokens, &stats.LoginAttempts, &stats.ManualSyncs, &stats.ImportJobs}
		for i, statement := range statements {
			res, err := tx.ExecContext(ctx, statement.query, statement.args...)
			if err != nil {
				return stats, err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return stats, err
			}
			*counts[i] = count
		}
		return stats, nil
	})
	return result, err
}
