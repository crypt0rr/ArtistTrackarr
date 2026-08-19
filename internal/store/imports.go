package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ImportJob is the owner-scoped record for one CSV upload.
type ImportJob struct {
	ID              int64
	UserID          int64
	CreatedAt       time.Time
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
	SourceValue string
	DisplayName string
	MBID        string
	MBURL       string
	SpotifyID   string
	SpotifyURL  string
	Reason      string
}

func (s *Store) CreateImportJob(ctx context.Context, userID int64) (ImportJob, error) {
	now := nowText()
	result, err := s.execWriteContext(ctx, `INSERT INTO import_jobs(user_id,created_at) VALUES(?,?)`, userID, now)
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
	return ImportJob{ID: id, UserID: userID, CreatedAt: created}, nil
}

// SaveImportRow persists one row and, for valid input, creates or reuses the
// canonical artist and owner-scoped follow in the same transaction. This makes
// partial uploads durable without coupling them to provider availability.
func (s *Store) SaveImportRow(ctx context.Context, userID, jobID int64, input ImportInput) (ImportRow, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return ImportRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
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
		if err := tx.Commit(); err != nil {
			return ImportRow{}, err
		}
		return row, nil
	}

	now := nowText()
	_, err = tx.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		spotify_id=COALESCE(excluded.spotify_id,artists.spotify_id),
		spotify_url=COALESCE(excluded.spotify_url,artists.spotify_url),updated_at=excluded.updated_at`,
		input.MBID, input.DisplayName, input.DisplayName, "", "", "",
		nullString(input.SpotifyID), nullString(input.SpotifyURL), nil, now, now)
	if err != nil {
		return ImportRow{}, err
	}
	var artistID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM artists WHERE mbid=?`, input.MBID).Scan(&artistID); err != nil {
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
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, now, artistID); err != nil {
			return ImportRow{}, err
		}
	} else {
		row.Status = "already_followed"
	}
	if err := insertImportRowTx(ctx, tx, row); err != nil {
		return ImportRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportRow{}, err
	}
	return row, nil
}

func insertImportRowTx(ctx context.Context, tx *sql.Tx, row ImportRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO import_rows(job_id,source_value,display_name,status,artist_id,reason)
		VALUES(?,?,?,?,?,?)`, row.JobID, row.SourceValue, row.DisplayName, row.Status, row.ArtistID, row.Reason)
	return err
}

func (s *Store) ImportJob(ctx context.Context, userID, jobID int64) (ImportJob, error) {
	var job ImportJob
	var created string
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,user_id,created_at FROM import_jobs WHERE id=? AND user_id=?`, jobID, userID).
		Scan(&job.ID, &job.UserID, &created)
	if err != nil {
		return ImportJob{}, err
	}
	job.CreatedAt, err = parseStoredTime(created, "import job created_at")
	if err != nil {
		return ImportJob{}, err
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
	var stats MaintenanceStats
	statements := []struct {
		query string
		args  []any
		out   *int64
	}{
		{`DELETE FROM sessions WHERE expires_at < ?`, []any{timeText(now)}, &stats.Sessions},
		{`DELETE FROM auth_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, []any{timeText(now)}, &stats.AuthTokens},
		{`DELETE FROM login_attempts WHERE first_at < ?`, []any{timeText(cutoff)}, &stats.LoginAttempts},
		{`DELETE FROM manual_sync_requests WHERE status IN ('completed','failed') AND finished_at IS NOT NULL AND finished_at < ?`, []any{timeText(manualCutoff)}, &stats.ManualSyncs},
		{`DELETE FROM import_jobs WHERE created_at < ?`, []any{timeText(manualCutoff)}, &stats.ImportJobs},
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		result, err := tx.ExecContext(ctx, statement.query, statement.args...)
		if err != nil {
			return stats, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return stats, err
		}
		*statement.out = count
	}
	if err := tx.Commit(); err != nil {
		return stats, err
	}
	return stats, nil
}
