package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const releaseInboxFrom = `
	FROM release_groups rg
	JOIN artists a ON a.id=rg.artist_id
	JOIN follows f ON f.artist_id=rg.artist_id
	JOIN notification_events e ON e.user_id=f.user_id AND e.release_group_id=rg.id
	LEFT JOIN user_release_states s ON s.user_id=f.user_id AND s.release_group_id=rg.id
	WHERE f.user_id=? AND e.id=(
		SELECT latest.id FROM notification_events latest
		WHERE latest.user_id=? AND latest.release_group_id=rg.id
		ORDER BY latest.created_at DESC,latest.id DESC LIMIT 1
	)`

// releaseInboxFilters keeps all filter values constrained to known enum-like
// values before they reach the query. The caller supplies the current UTC time
// for determining whether a snooze has expired.
func releaseInboxFilters(state, source, primaryType string, now time.Time) (string, []any) {
	where := ` AND (s.state IS NULL OR s.state<>'dismissed')`
	args := make([]any, 0, 3)
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "unread":
		where += ` AND (s.state IS NULL OR s.state='unread' OR
			(s.state='snoozed' AND (s.snoozed_until IS NULL OR s.snoozed_until<=?)))`
		args = append(args, timeText(now))
	case "read":
		where += ` AND s.state='read'`
	case "snoozed":
		where += ` AND s.state='snoozed' AND s.snoozed_until>?`
		args = append(args, timeText(now))
	case "dismissed":
		where = ` AND s.state='dismissed'`
	}
	switch source = strings.ToLower(strings.TrimSpace(source)); source {
	case "musicbrainz", "spotify", "itunes", "both":
		where += ` AND rg.source=?`
		args = append(args, source)
	}
	switch primaryType = strings.ToLower(strings.TrimSpace(primaryType)); primaryType {
	case "album", "ep", "single":
		where += ` AND lower(rg.primary_type)=?`
		args = append(args, primaryType)
	}
	return where, args
}

// ReleaseInbox returns the latest alertable event for each followed release.
// Historical releases that were silently baselined never have a notification
// event and therefore do not appear here.
func (s *Store) ReleaseInbox(ctx context.Context, userID int64, state, source, primaryType string, limit, offset int, now time.Time) ([]ReleaseInboxItem, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, filterArgs := releaseInboxFilters(state, source, primaryType, now.UTC())
	query := `SELECT ` + releaseSelectColumns + `,e.event_type,e.title,e.created_at,COALESCE(s.state,'unread'),s.snoozed_until` +
		releaseInboxFrom + where + ` ORDER BY e.created_at DESC,e.id DESC LIMIT ? OFFSET ?`
	args := []any{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.readerDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ReleaseInboxItem
	for rows.Next() {
		var item ReleaseInboxItem
		var eventCreated, stateValue string
		var snoozed sql.NullString
		item.Release, err = scanReleaseWithExtra(rows, &item.EventType, &item.EventTitle, &eventCreated, &stateValue, &snoozed)
		if err != nil {
			return nil, err
		}
		item.EventCreatedAt, _ = parseTime(eventCreated)
		item.State = stateValue
		if snoozed.Valid && snoozed.String != "" {
			when, parseErr := parseTime(snoozed.String)
			if parseErr == nil {
				item.SnoozedUntil = &when
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ReleaseInboxCount(ctx context.Context, userID int64, state, source, primaryType string, now time.Time) (int, error) {
	where, filterArgs := releaseInboxFilters(state, source, primaryType, now.UTC())
	query := `SELECT COUNT(*)` + releaseInboxFrom + where
	args := []any{userID, userID}
	args = append(args, filterArgs...)
	var count int
	err := s.readerDB().QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *Store) ReleaseInboxUnreadCount(ctx context.Context, userID int64, now time.Time) (int, error) {
	return s.ReleaseInboxCount(ctx, userID, "unread", "", "", now)
}

// SetReleaseInboxState changes only the requesting member's review state. A
// release must still have an alertable event for that member, preventing
// cross-user access and accidental state rows for arbitrary release IDs.
func (s *Store) SetReleaseInboxState(ctx context.Context, userID, releaseID int64, state string, snoozedUntil *time.Time) error {
	state = strings.ToLower(strings.TrimSpace(state))
	if state != "read" && state != "unread" && state != "snoozed" && state != "dismissed" {
		return errors.New("invalid release inbox state")
	}
	if state == "snoozed" {
		if snoozedUntil == nil || !snoozedUntil.After(time.Now().UTC()) {
			return errors.New("snooze time must be in the future")
		}
	} else {
		snoozedUntil = nil
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM follows f
		JOIN notification_events e ON e.user_id=f.user_id AND e.release_group_id=?
		WHERE f.user_id=? AND f.artist_id=(SELECT artist_id FROM release_groups WHERE id=?) LIMIT 1`,
		releaseID, userID, releaseID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	var until any
	if snoozedUntil != nil {
		until = timeText(snoozedUntil.UTC())
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_release_states(user_id,release_group_id,state,snoozed_until,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(user_id,release_group_id) DO UPDATE SET state=excluded.state,
			snoozed_until=excluded.snoozed_until,updated_at=excluded.updated_at`,
		userID, releaseID, state, until, nowText())
	if err != nil {
		return err
	}
	return tx.Commit()
}
