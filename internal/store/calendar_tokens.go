package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/security"
)

const calendarFeedTokenTTL = 365 * 24 * time.Hour

// CalendarFeedToken describes the owner-scoped calendar subscription
// credential without ever exposing its raw value after creation.
type CalendarFeedToken struct {
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
	Active    bool
}

// CreateCalendarFeedToken rotates the owner's calendar credential. The raw
// token is returned once; only its digest is persisted. Rotation invalidates
// the previous token atomically.
func (s *Store) CreateCalendarFeedToken(ctx context.Context, userID int64) (string, error) {
	raw, err := security.Token(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	expires := now.Add(calendarFeedTokenTTL)
	_, err = withWriteTxResult(s, ctx, func(tx *sql.Tx) (struct{}, error) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=?`, userID).Scan(&exists); err != nil {
			return struct{}{}, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO calendar_feed_tokens
			(user_id,token_hash,created_at,expires_at,revoked_at)
			VALUES(?,?,?,?,NULL)
			ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash,
			created_at=excluded.created_at,expires_at=excluded.expires_at,revoked_at=NULL`,
			userID, security.Digest(raw), timeText(now), timeText(expires))
		return struct{}{}, err
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// RevokeCalendarFeedToken invalidates the current credential while retaining
// its audit metadata. It is deliberately owner-scoped and idempotent for a
// missing token so account cleanup cannot leak whether another account has a
// token.
func (s *Store) RevokeCalendarFeedToken(ctx context.Context, userID int64) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE calendar_feed_tokens SET revoked_at=?
			WHERE user_id=? AND revoked_at IS NULL`, nowText(), userID)
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
	})
}

// CalendarFeedTokenStatus returns the current token lifecycle state. The raw
// credential is intentionally not recoverable from this projection.
func (s *Store) CalendarFeedTokenStatus(ctx context.Context, userID int64) (CalendarFeedToken, error) {
	var token CalendarFeedToken
	var created, expires string
	var revoked sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT created_at,expires_at,revoked_at
		FROM calendar_feed_tokens WHERE user_id=?`, userID).Scan(&created, &expires, &revoked)
	if err != nil {
		return token, err
	}
	token.CreatedAt, err = parseStoredTime(created, "calendar feed created_at")
	if err != nil {
		return token, err
	}
	token.ExpiresAt, err = parseStoredTime(expires, "calendar feed expires_at")
	if err != nil {
		return token, err
	}
	token.RevokedAt, err = parseStoredNullableTime(revoked, "calendar feed revoked_at")
	if err != nil {
		return token, err
	}
	token.Active = token.RevokedAt == nil && token.ExpiresAt.After(time.Now().UTC())
	return token, nil
}

// UserIDByCalendarFeedToken resolves an active opaque feed credential. Raw
// values are bounded before hashing so malformed requests cannot create
// unbounded work or accidentally match an empty digest.
func (s *Store) UserIDByCalendarFeedToken(ctx context.Context, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return 0, sql.ErrNoRows
	}
	var userID int64
	err := s.readerDB().QueryRowContext(ctx, `SELECT user_id FROM calendar_feed_tokens
		WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`, security.Digest(raw), nowText()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, sql.ErrNoRows
	}
	return userID, err
}
