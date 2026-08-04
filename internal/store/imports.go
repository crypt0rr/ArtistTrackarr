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
	result, err := s.DB.ExecContext(ctx, `INSERT INTO import_jobs(user_id,created_at) VALUES(?,?)`, userID, now)
	if err != nil {
		return ImportJob{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ImportJob{}, err
	}
	created, _ := parseTime(now)
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
	defer tx.Rollback()
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
	if err := s.readerDB().QueryRowContext(ctx, `SELECT id,user_id,created_at FROM import_jobs WHERE id=? AND user_id=?`, jobID, userID).
		Scan(&job.ID, &job.UserID, &created); err != nil {
		return ImportJob{}, err
	}
	job.CreatedAt, _ = parseTime(created)
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,job_id,source_value,display_name,status,artist_id,reason
		FROM import_rows WHERE job_id=? ORDER BY id`, jobID)
	if err != nil {
		return ImportJob{}, err
	}
	defer rows.Close()
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
	cutoff := now.Add(-24 * time.Hour)
	manualCutoff := now.Add(-30 * 24 * time.Hour)
	var stats MaintenanceStats
	statements := []struct {
		query string
		args  []any
		out   *int64
	}{
		{`DELETE FROM sessions WHERE datetime(expires_at) < datetime(?)`, []any{timeText(now)}, &stats.Sessions},
		{`DELETE FROM auth_tokens WHERE datetime(expires_at) < datetime(?) OR used_at IS NOT NULL`, []any{timeText(now)}, &stats.AuthTokens},
		{`DELETE FROM login_attempts WHERE datetime(first_at) < datetime(?)`, []any{timeText(cutoff)}, &stats.LoginAttempts},
		{`DELETE FROM manual_sync_requests WHERE status IN ('completed','failed') AND finished_at IS NOT NULL AND datetime(finished_at) < datetime(?)`, []any{timeText(manualCutoff)}, &stats.ManualSyncs},
		{`DELETE FROM import_jobs WHERE datetime(created_at) < datetime(?)`, []any{timeText(manualCutoff)}, &stats.ImportJobs},
	}
	for _, statement := range statements {
		result, err := s.DB.ExecContext(ctx, statement.query, statement.args...)
		if err != nil {
			return stats, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return stats, err
		}
		*statement.out = count
	}
	return stats, nil
}
