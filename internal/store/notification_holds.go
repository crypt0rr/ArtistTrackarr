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
	// A member's explicit discard is terminal until they restore the hold.
	// Without this guard, each release-day queue pass would recreate the same
	// discarded review row while the evidence issue remained open.
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM notification_holds
		WHERE user_id=? AND release_group_id=? AND event_type=? AND status='discarded' LIMIT 1`, userID, releaseID, eventType).Scan(&exists); err == nil {
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
	return drainResolvedNotificationHoldsForUserTx(ctx, tx, 0, releaseID, now)
}

// drainResolvedNotificationHoldsForUserTx is the owner-scoped variant used by
// private evidence review actions. Synchronization-driven resolution calls the
// household-wide helper above, but one member confirming an issue must never
// release another member's held notification.
func drainResolvedNotificationHoldsForUserTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, now time.Time) error {
	var blocking int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_evidence_issues
		WHERE release_group_id=? AND status='open' AND severity IN ('warning','critical')`, releaseID).Scan(&blocking); err != nil {
		return err
	}
	if blocking > 0 {
		return nil
	}
	return drainNotificationHoldsTxMode(ctx, tx, userID, releaseID, now, false)
}

// drainNotificationHoldsTx creates the normal event/delivery rows and marks
// matching holds released. userID=0 means all owners of the release.
func drainNotificationHoldsTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, now time.Time) error {
	return drainNotificationHoldsTxMode(ctx, tx, userID, releaseID, now, true)
}

// drainNotificationHoldsTxMode re-admits held events through the current
// notification rules before releasing their review rows. A hold created under
// an immediate rule may later be paused, switched to digest-only, or disabled;
// those changes must be honored when the hold is resolved. Explicit provider
// confirmation and a user choosing "Notify anyway" bypass only the conflict
// gate, not the owner's current content, timing, or delivery-mode rules.
func drainNotificationHoldsTxMode(ctx context.Context, tx *sql.Tx, userID, releaseID int64, now time.Time, bypassConflictHold bool) error {
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
	if err := func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item hold
			if err := rows.Scan(&item.id, &item.ownerID, &item.eventType, &item.title, &item.body); err != nil {
				return err
			}
			holds = append(holds, item)
		}
		return rows.Err()
	}(); err != nil {
		return err
	}
	for _, item := range holds {
		if err := enqueueHeldEventTxMode(ctx, tx, item.ownerID, releaseID, item.eventType, item.title, item.body, now, bypassConflictHold); err != nil {
			return err
		}
		var eventID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM notification_events
			WHERE user_id=? AND release_group_id=? AND event_type=?`, item.ownerID, releaseID, item.eventType).Scan(&eventID)
		if errors.Is(err, sql.ErrNoRows) {
			// Current rules intentionally suppressed this event (for example,
			// the owner disabled the follow or account-level moment). Keep the
			// hold available so a later rule change or explicit restore can
			// re-admit it instead of silently losing the alert.
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_holds SET status='released',released_at=? WHERE id=? AND status='held'`, timeText(now), item.id); err != nil {
			return err
		}
	}
	return nil
}

// ensureApprovedReleaseNotificationTx makes an explicit review actionable
// even when the provider conflict was discovered after the original event
// would have been queued. Existing events and explicit discard decisions win;
// otherwise the event is rebuilt through the normal preference and follow-rule
// admission path, bypassing only the conflict hold itself.
func ensureApprovedReleaseNotificationTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, now time.Time, bypassBlocking bool) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM notification_events
		WHERE user_id=? AND release_group_id=? LIMIT 1`, userID, releaseID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// A member who explicitly discarded a held notification must not receive a
	// later approval-created copy of that same release.
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM notification_holds
		WHERE user_id=? AND release_group_id=? AND status='discarded' LIMIT 1`, userID, releaseID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM release_groups rg
		WHERE rg.id=? AND `+followedReleasePredicate("?")+` LIMIT 1`, releaseID, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !bypassBlocking {
		var blocking int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_evidence_issues
			WHERE release_group_id=? AND status='open' AND severity IN ('warning','critical')`, releaseID).Scan(&blocking); err != nil {
			return err
		}
		if blocking > 0 {
			return nil
		}
	}

	release, err := releaseByIDTx(ctx, tx, releaseID)
	if err != nil {
		return err
	}
	var timezone string
	if err := tx.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id=?`, userID).Scan(&timezone); err != nil {
		return err
	}
	location := userLocation(timezone)
	localNow := now.In(location)
	eventType := "announcement"
	releaseDateValue := strings.TrimSpace(release.FirstReleaseDate)
	precision := release.DatePrecision
	if precision < 1 || precision > 3 {
		switch len(releaseDateValue) {
		case 4:
			precision = 1
		case 7:
			precision = 2
		case 10:
			precision = 3
		default:
			return nil
		}
	}
	switch precision {
	case 1:
		if _, valid := comparableReleaseDate(releaseDateValue); !valid || len(releaseDateValue) != 4 {
			return nil
		}
	case 2:
		if _, valid := comparableReleaseDate(releaseDateValue); !valid || len(releaseDateValue) != 7 {
			return nil
		}
	case 3:
		date, valid := releaseDate(releaseDateValue)
		if !valid {
			return nil
		}
		if date.Format("2006-01-02") == localNow.Format("2006-01-02") {
			eventType = "release_day"
		}
	default:
		return nil
	}
	title, body := initialReleaseMessageInLocation(Artist{ID: release.ArtistID, Name: release.ArtistName}, release, eventType, now.UTC(), location)
	return enqueueApprovedEventTx(ctx, tx, userID, releaseID, eventType, title, body, now.UTC())
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

// NotificationHoldsForReleaseIncludingDiscarded returns pending and explicitly
// discarded holds for a release. The broader projection is used only on the
// release details page so a member can intentionally restore a discarded hold;
// dashboard hold counts continue to represent pending work only.
func (s *Store) NotificationHoldsForReleaseIncludingDiscarded(ctx context.Context, userID, releaseID int64) ([]NotificationHold, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT h.id,h.user_id,h.release_group_id,a.name,rg.title,
		h.event_type,h.title,h.body,h.reason,h.issue_fingerprint,h.planned_at,h.status,h.created_at,h.released_at
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE h.user_id=? AND h.release_group_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status IN ('held','discarded')
		ORDER BY h.created_at DESC,h.id DESC`, userID, releaseID)
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
// delivery queue, discards it, or restores a discarded hold. All actions are
// owner-scoped and atomic. Restoring re-enters the normal admission flow: a
// still-blocking evidence issue leaves the hold pending, while a resolved
// issue releases it immediately.
func (s *Store) ResolveNotificationHold(ctx context.Context, userID, holdID int64, action string) error {
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "notify" && action != "discard" && action != "restore" {
		return ErrInvalidNotificationHoldAction
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var releaseID int64
		var eventType, title, body string
		status := "held"
		if action == "restore" {
			status = "discarded"
		}
		if err := tx.QueryRowContext(ctx, `SELECT h.release_group_id,h.event_type,h.title,h.body
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		WHERE h.id=? AND h.user_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status=?`, holdID, userID, status).Scan(&releaseID, &eventType, &title, &body); err != nil {
			return err
		}
		now := time.Now().UTC()
		if action == "restore" {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_holds
			SET status='held',released_at=NULL WHERE id=? AND user_id=? AND status='discarded'`, holdID, userID); err != nil {
				return err
			}
			return drainResolvedNotificationHoldsForUserTx(ctx, tx, userID, releaseID, now)
		}
		status = "discarded"
		if action == "notify" {
			if err := enqueueHeldEventTxMode(ctx, tx, userID, releaseID, eventType, title, body, now, true); err != nil {
				return err
			}
			var eventID int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM notification_events
			WHERE user_id=? AND release_group_id=? AND event_type=?`, userID, releaseID, eventType).Scan(&eventID)
			if errors.Is(err, sql.ErrNoRows) {
				// The current rule intentionally blocks admission. Keep the hold
				// pending so the owner can change the rule and retry it later.
				return nil
			}
			if err != nil {
				return err
			}
			status = "released"
		}
		_, err := tx.ExecContext(ctx, `UPDATE notification_holds SET status=?,released_at=? WHERE id=? AND status='held'`, status, timeText(now), holdID)
		return err
	})
}

func scanNotificationHold(row interface{ Scan(...any) error }) (NotificationHold, error) {
	var item NotificationHold
	var planned, created, released sql.NullString
	if err := row.Scan(&item.ID, &item.UserID, &item.ReleaseGroupID, &item.ArtistName, &item.ReleaseTitle,
		&item.EventType, &item.Title, &item.Body, &item.Reason, &item.IssueFingerprint,
		&planned, &item.Status, &created, &released); err != nil {
		return item, err
	}
	var err error
	item.PlannedAt, err = parseStoredTime(planned.String, "notification hold planned_at")
	if err != nil {
		return item, err
	}
	item.CreatedAt, err = parseStoredTime(created.String, "notification hold created_at")
	if err != nil {
		return item, err
	}
	if item.ReleasedAt, err = parseStoredNullableTime(released, "notification hold released_at"); err != nil {
		return item, err
	}
	return item, nil
}
