package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// holdConflictingNotificationTx stores one pending notification when the
// latest provider evidence contains a warning or critical issue. Informational
// issues (for example, a missing canonical observation) do not block delivery.
func holdConflictingNotificationTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM notification_events
		WHERE user_id=? AND release_group_id=? AND event_type=? LIMIT 1`, userID, releaseID, eventType).Scan(&exists); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var reason, fingerprint string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(group_concat(summary,'; '),''),
		COALESCE(group_concat(fingerprint,','),'')
		FROM release_evidence_issues
		WHERE release_group_id=? AND status='open' AND severity IN ('warning','critical')`, releaseID).Scan(&reason, &fingerprint); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return insertNotificationEventTx(ctx, tx, userID, releaseID, eventType, title, body, now)
	}
	var holdID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM notification_holds
		WHERE user_id=? AND release_group_id=? AND event_type=? AND status='held'`, userID, releaseID, eventType).Scan(&holdID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_holds
			(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at)
			VALUES(?,?,?,?,?,?,?,?, 'held',?)`, userID, releaseID, eventType, title, body,
			reason, fingerprint, timeText(now), timeText(now))
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE notification_holds SET title=?,body=?,reason=?,issue_fingerprint=?,planned_at=? WHERE id=?`,
			title, body, reason, fingerprint, timeText(now), holdID)
	}
	return err
}

// drainResolvedNotificationHoldsTx releases holds for a release once its
// warning/critical evidence issues have disappeared during synchronization.
func drainResolvedNotificationHoldsTx(ctx context.Context, tx *sql.Tx, releaseID int64, now time.Time) error {
	var blocking int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_evidence_issues
		WHERE release_group_id=? AND status='open' AND severity IN ('warning','critical')`, releaseID).Scan(&blocking); err != nil {
		return err
	}
	if blocking > 0 {
		return nil
	}
	return drainNotificationHoldsTx(ctx, tx, 0, releaseID, now)
}

// drainNotificationHoldsTx creates the normal event/delivery rows and marks
// matching holds released. userID=0 means all owners of the release.
func drainNotificationHoldsTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, now time.Time) error {
	query := `SELECT h.id,h.user_id,h.event_type,h.title,h.body
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		WHERE h.release_group_id=? AND ` + followedReleasePredicate("h.user_id") + ` AND h.status='held'`
	args := []any{releaseID}
	if userID > 0 {
		query += ` AND h.user_id=?`
		args = append(args, userID)
	}
	query += ` ORDER BY h.id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type hold struct {
		id, ownerID            int64
		eventType, title, body string
	}
	var holds []hold
	for rows.Next() {
		var item hold
		if err := rows.Scan(&item.id, &item.ownerID, &item.eventType, &item.title, &item.body); err != nil {
			_ = rows.Close()
			return err
		}
		holds = append(holds, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range holds {
		// A hold was created only after the normal rule and preference checks
		// passed. Releasing it is an explicit review action (or a resolved
		// conflict), so bypass the hold check and queue the retained event.
		if err := insertNotificationEventTx(ctx, tx, item.ownerID, releaseID, item.eventType, item.title, item.body, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_holds SET status='released',released_at=? WHERE id=? AND status='held'`, timeText(now), item.id); err != nil {
			return err
		}
	}
	return nil
}

// NotificationHolds returns pending holds for one household member.
func (s *Store) NotificationHolds(ctx context.Context, userID int64, limit int) ([]NotificationHold, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT h.id,h.user_id,h.release_group_id,a.name,rg.title,
		h.event_type,h.title,h.body,h.reason,h.issue_fingerprint,h.planned_at,h.status,h.created_at,h.released_at
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE h.user_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status='held'
		ORDER BY h.created_at DESC,h.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []NotificationHold
	for rows.Next() {
		item, err := scanNotificationHold(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// NotificationHoldsForRelease returns pending holds for a followed release.
func (s *Store) NotificationHoldsForRelease(ctx context.Context, userID, releaseID int64) ([]NotificationHold, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT h.id,h.user_id,h.release_group_id,a.name,rg.title,
		h.event_type,h.title,h.body,h.reason,h.issue_fingerprint,h.planned_at,h.status,h.created_at,h.released_at
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE h.user_id=? AND h.release_group_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status='held'
		ORDER BY h.created_at DESC,h.id DESC`, userID, releaseID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []NotificationHold
	for rows.Next() {
		item, err := scanNotificationHold(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ResolveNotificationHold either sends the held event through the normal
// delivery queue or discards it. Both actions are owner-scoped and atomic.
func (s *Store) ResolveNotificationHold(ctx context.Context, userID, holdID int64, action string) error {
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "notify" && action != "discard" {
		return ErrInvalidNotificationHoldAction
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var releaseID int64
	var eventType, title, body string
	if err := tx.QueryRowContext(ctx, `SELECT h.release_group_id,h.event_type,h.title,h.body
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		WHERE h.id=? AND h.user_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status='held'`, holdID, userID, userID).Scan(&releaseID, &eventType, &title, &body); err != nil {
		return err
	}
	now := time.Now().UTC()
	status := "discarded"
	if action == "notify" {
		if err := insertNotificationEventTx(ctx, tx, userID, releaseID, eventType, title, body, now); err != nil {
			return err
		}
		status = "released"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_holds SET status=?,released_at=? WHERE id=? AND status='held'`, status, timeText(now), holdID); err != nil {
		return err
	}
	return tx.Commit()
}

func scanNotificationHold(row interface{ Scan(...any) error }) (NotificationHold, error) {
	var item NotificationHold
	var planned, created, released sql.NullString
	if err := row.Scan(&item.ID, &item.UserID, &item.ReleaseGroupID, &item.ArtistName, &item.ReleaseTitle,
		&item.EventType, &item.Title, &item.Body, &item.Reason, &item.IssueFingerprint,
		&planned, &item.Status, &created, &released); err != nil {
		return item, err
	}
	item.PlannedAt, _ = parseTime(planned.String)
	item.CreatedAt, _ = parseTime(created.String)
	if released.Valid && released.String != "" {
		t, err := parseTime(released.String)
		if err == nil {
			item.ReleasedAt = &t
		}
	}
	return item, nil
}
