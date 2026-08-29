package store

// This file holds store methods that exist only for tests. They are compiled
// into the test binary alone, so they are not part of the package's production
// surface.
//
// They were exported methods with no production callers: the runner uses the
// claiming and owner-scoped variants exclusively. Keeping them reachable from
// non-test code invited new callers onto a path production does not use - and
// the Due* pair in particular differs from what the runner runs, since it
// returns rows without leasing them and omits the per-user fairness ceiling the
// claim queries apply, so a test written against it does not cover the runner's
// actual selection shape.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/security"
)

func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at,
			dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,COALESCE(dst.transport_status,'supported'),COALESCE(dst.transport_message,''),
		e.title,e.body,e.event_type,rg.title
		FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id
		LEFT JOIN destination_health dh ON dh.destination_id=dst.id
		JOIN notification_events e ON e.id=d.event_id JOIN release_groups rg ON rg.id=e.release_group_id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND dst.enabled=1
		AND (d.claim_expires_at IS NULL OR d.claim_expires_at<=?)
		AND `+supportedDestinationServicePredicate("dst")+` AND `+destinationAdmissionPredicate+`
		AND `+destinationRetryPredicate+` ORDER BY d.next_attempt_at LIMIT ?`,
		timeText(now), timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Delivery
	for rows.Next() {
		var d Delivery
		var next string
		if err := rows.Scan(&d.ID, &d.EventID, &d.Attempts, &next,
			&d.Destination.ID, &d.Destination.UserID, &d.Destination.Name, &d.Destination.Service,
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Destination.TransportStatus, &d.Destination.TransportMessage,
			&d.Title, &d.Body, &d.EventType, &d.ReleaseTitle); err != nil {
			return nil, err
		}
		d.NextAttempt, err = parseStoredTime(next, "delivery next_attempt_at")
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// DueDigestDeliveries returns aggregate release-digest deliveries ready for
// the same notification worker used by normal release events.
func (s *Store) DueDigestDeliveries(ctx context.Context, now time.Time, limit int) ([]DigestDelivery, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT dd.id,dd.run_id,dd.attempts,dd.next_attempt_at,
		dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,COALESCE(dst.transport_status,'supported'),COALESCE(dst.transport_message,''),
		r.title,r.body
		FROM release_digest_deliveries dd
		JOIN release_digest_runs r ON r.id=dd.run_id
		JOIN destinations dst ON dst.id=dd.destination_id
		LEFT JOIN destination_health dh ON dh.destination_id=dst.id
		WHERE dd.status='pending' AND dd.next_attempt_at<=? AND dst.enabled=1
		AND (dd.claim_expires_at IS NULL OR dd.claim_expires_at<=?)
		AND `+supportedDestinationServicePredicate("dst")+` AND `+destinationAdmissionPredicate+`
		AND `+destinationRetryPredicate+`
		ORDER BY dd.next_attempt_at,dd.id LIMIT ?`, timeText(now), timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []DigestDelivery
	for rows.Next() {
		var d DigestDelivery
		var next string
		if err := rows.Scan(&d.ID, &d.RunID, &d.Attempts, &next,
			&d.Destination.ID, &d.Destination.UserID, &d.Destination.Name, &d.Destination.Service,
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Destination.TransportStatus, &d.Destination.TransportMessage,
			&d.Title, &d.Body); err != nil {
			return nil, err
		}
		d.NextAttempt, err = parseStoredTime(next, "digest delivery next_attempt_at")
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) MarkDeliverySent(ctx context.Context, id int64, now time.Time) error {
	err := s.MarkDeliverySentOwned(ctx, id, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	err := s.MarkDeliveryFailedOwned(ctx, id, attempts, message, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDigestDeliverySent(ctx context.Context, id int64, now time.Time) error {
	err := s.MarkDigestDeliverySentOwned(ctx, id, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDigestDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	err := s.MarkDigestDeliveryFailedOwned(ctx, id, attempts, message, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) ClaimManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	return s.ClaimManualSyncRequestsWithLease(ctx, limit, "legacy-worker", 5*time.Minute)
}

func (s *Store) CompleteManualSyncRequest(ctx context.Context, id int64, syncErr error) error {
	return s.CompleteManualSyncRequestOwned(ctx, id, "", syncErr)
}

func (s *Store) ConsumeAuthToken(ctx context.Context, raw, kind string) (email string, userID *int64, err error) {
	result, err := withWriteTxResult(s, ctx, func(tx *sql.Tx) (struct {
		email string
		id    sql.NullInt64
	}, error) {
		var result struct {
			email string
			id    sql.NullInt64
		}
		if err := tx.QueryRowContext(ctx, `SELECT email,user_id FROM auth_tokens
			WHERE token_hash=? AND kind=? AND used_at IS NULL AND expires_at>?`,
			security.Digest(raw), kind, nowText()).Scan(&result.email, &result.id); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE token_hash=?`, nowText(), security.Digest(raw)); err != nil {
			return result, err
		}
		return result, nil
	})
	if err != nil {
		return "", nil, err
	}
	if result.id.Valid {
		userID = &result.id.Int64
	}
	return result.email, userID, nil
}

// WatchlistAssurance returns the complete owner-scoped assurance counts and a
// small severity-ranked list for dashboard use. The underlying projections
// are batched, matching the Trust Center's query behavior.
func (s *Store) WatchlistAssurance(ctx context.Context, userID int64, limit int) (AssuranceSummary, error) {
	_, summary, err := s.CoverageOverview(ctx, userID, limit)
	return summary, err
}
