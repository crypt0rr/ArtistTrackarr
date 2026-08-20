package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const artistIdentityStatusColumns = `artist_id,status,attempts,next_check_at,last_error,updated_at`

func scanArtistIdentityStatus(row interface{ Scan(...any) error }) (ArtistIdentityStatus, error) {
	var status ArtistIdentityStatus
	var next, updated sql.NullString
	if err := row.Scan(&status.ArtistID, &status.Status, &status.Attempts, &next, &status.LastError, &updated); err != nil {
		return ArtistIdentityStatus{}, err
	}
	var err error
	if status.NextCheckAt, err = parseStoredNullableTime(next, "artist identity next_check_at"); err != nil {
		return ArtistIdentityStatus{}, err
	}
	if updated.Valid && strings.TrimSpace(updated.String) != "" {
		status.UpdatedAt, err = parseStoredTime(updated.String, "artist identity updated_at")
		if err != nil {
			return ArtistIdentityStatus{}, err
		}
	}
	return status, nil
}

// ArtistIdentityStatus returns the durable verification state. The boolean is
// false only for a legacy database row that predates the verification table;
// such artists were admitted through a canonical MusicBrainz result and are
// therefore treated as verified by callers.
func (s *Store) ArtistIdentityStatus(ctx context.Context, artistID int64) (ArtistIdentityStatus, bool, error) {
	status, err := scanArtistIdentityStatus(s.readerDB().QueryRowContext(ctx,
		`SELECT `+artistIdentityStatusColumns+` FROM artist_identity_status WHERE artist_id=?`, artistID))
	if errors.Is(err, sql.ErrNoRows) {
		return ArtistIdentityStatus{ArtistID: artistID, Status: "verified"}, false, nil
	}
	return status, err == nil, err
}

// VerifyArtistIdentity records canonical metadata and transitions the import
// state atomically. Provider metadata such as Spotify identity is deliberately
// preserved because MusicBrainz is only validating the canonical identity.
func (s *Store) VerifyArtistIdentity(ctx context.Context, artistID int64, artist Artist) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		now := nowText()
		result, err := tx.ExecContext(ctx, `UPDATE artists SET mbid=?,name=?,sort_name=?,artist_type=?,country=?,disambiguation=?,updated_at=? WHERE id=?`,
			artist.MBID, artist.Name, artist.SortName, artist.Type, artist.Country, artist.Disambiguation, now, artistID)
		if err != nil {
			return err
		}
		if err := changedOrNotFound(result, nil); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO artist_identity_status
			(artist_id,status,attempts,next_check_at,last_error,updated_at)
			VALUES(?, 'verified',0,NULL,'',?)
			ON CONFLICT(artist_id) DO UPDATE SET status='verified',attempts=0,next_check_at=NULL,last_error='',updated_at=excluded.updated_at`,
			artistID, now)
		return err
	})
}

// ScheduleArtistIdentityFailure persists a bounded retry or terminal state
// and advances both artist schedules so a failed import cannot pin the due
// queue (including when it carries a Spotify identity).
func (s *Store) ScheduleArtistIdentityFailure(ctx context.Context, artistID int64, attempts int, next time.Time, message string, terminal bool) error {
	if attempts < 1 {
		attempts = 1
	}
	if len(message) > 500 {
		message = message[:500]
	}
	status := "pending"
	if terminal {
		status = "unresolvable"
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		now := nowText()
		if _, err := tx.ExecContext(ctx, `INSERT INTO artist_identity_status
			(artist_id,status,attempts,next_check_at,last_error,updated_at)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(artist_id) DO UPDATE SET status=excluded.status,attempts=excluded.attempts,
				next_check_at=excluded.next_check_at,last_error=excluded.last_error,updated_at=excluded.updated_at`,
			artistID, status, attempts, timeText(next), message, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
			CASE WHEN spotify_id IS NOT NULL AND (spotify_next_check_at IS NULL OR spotify_next_check_at<?)
			THEN ? ELSE spotify_next_check_at END WHERE id=?`,
			timeText(next), timeText(next), timeText(next), artistID)
		return err
	})
}

// ResetArtistIdentity makes an explicitly requested manual sync retry a
// terminal or pending import immediately.
func (s *Store) ResetArtistIdentity(ctx context.Context, artistID int64, now time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		nowTextValue := timeText(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO artist_identity_status
			(artist_id,status,attempts,next_check_at,last_error,updated_at)
			VALUES(?, 'verified',0,NULL,'',?)
			ON CONFLICT(artist_id) DO UPDATE SET
				status=CASE WHEN artist_identity_status.status<>'verified' THEN 'pending' ELSE artist_identity_status.status END,
				attempts=CASE WHEN artist_identity_status.status<>'verified' THEN 0 ELSE artist_identity_status.attempts END,
				next_check_at=CASE WHEN artist_identity_status.status<>'verified' THEN excluded.updated_at ELSE artist_identity_status.next_check_at END,
				last_error=CASE WHEN artist_identity_status.status<>'verified' THEN '' ELSE artist_identity_status.last_error END,
				updated_at=excluded.updated_at`, artistID, nowTextValue); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
			CASE WHEN spotify_id IS NOT NULL THEN ? ELSE spotify_next_check_at END WHERE id=?`,
			nowTextValue, nowTextValue, artistID)
		return err
	})
}

func insertArtistIdentityStatusTx(ctx context.Context, tx *sql.Tx, artistID int64, status string) error {
	if status != "pending" && status != "verified" {
		return fmt.Errorf("invalid artist identity status %q", status)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO artist_identity_status
		(artist_id,status,attempts,next_check_at,last_error,updated_at) VALUES(?,?,0,NULL,'',?)`,
		artistID, status, nowText())
	return err
}
