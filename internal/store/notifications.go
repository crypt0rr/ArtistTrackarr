package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// destinationAdmissionPredicate is shared by event/digest fan-out and both
// due-delivery readers. A destination's historical failures are informational;
// only the explicit persisted paused state blocks new work. Transport policy
// is applied alongside it so legacy Gotify/SMTP/unknown advanced destinations
// remain visible but cannot initiate network activity.
const destinationAdmissionPredicate = `COALESCE(dh.status,'healthy')<>'paused'`

func supportedDestinationServicePredicate(alias string) string {
	return `COALESCE(` + alias + `.transport_status,'supported')='supported' AND LOWER(` + alias + `.service) IN ('discord','telegram','ntfy','generic')`
}

func destinationTransportStatus(service string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(service)) {
	case "ntfy", "discord", "telegram", "generic":
		return "supported", ""
	default:
		return "unsupported", "This destination uses a transport that is no longer supported; replace it."
	}
}

// destinationQueueStatus is used at event/digest creation time.  A paused or
// legacy-unsupported destination still receives a durable blocked row so the
// event is never silently lost; due readers only admit supported/presently
// healthy destinations.
func destinationQueueStatus(alias string) string {
	return `CASE WHEN COALESCE(` + alias + `.transport_status,'supported')<>'supported' OR LOWER(` + alias + `.service) NOT IN ('discord','telegram','ntfy','generic') OR COALESCE(dh.status,'healthy')='paused' THEN 'blocked' ELSE 'pending' END`
}

var (
	deliveryURLPattern        = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	deliveryCredentialPattern = regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key|key)=([^&\s]+)`)
)

func safeDeliveryError(message string) string {
	message = deliveryURLPattern.ReplaceAllString(message, "[redacted destination]")
	message = deliveryCredentialPattern.ReplaceAllString(message, "$1=[redacted]")
	return strings.TrimSpace(message)
}

func (s *Store) AddDestination(ctx context.Context, userID int64, name, service string, encrypted []byte) error {
	name, err := destinationName(name)
	if err != nil {
		return err
	}
	status, message := destinationTransportStatus(service)
	_, err = s.execWriteContext(ctx, `INSERT INTO destinations(user_id,name,service,encrypted_url,created_at,transport_status,transport_message)
		VALUES(?,?,?,?,?,?,?)`, userID, name, service, encrypted, nowText(), status, message)
	return err
}

// ValidateDestinationCiphertexts verifies that every persisted destination can
// be opened with the configured application key. It deliberately returns only
// the destination ID and a generic error; ciphertext and provider credentials
// never enter logs or user-facing responses. Startup uses this check during
// restore rehearsals so a wrong key cannot produce a seemingly healthy but
// unusable application.
func (s *Store) ValidateDestinationCiphertexts(ctx context.Context, decrypt func([]byte) (string, error)) error {
	if decrypt == nil {
		return errors.New("destination decryptor is required")
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,encrypted_url FROM destinations ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var encrypted []byte
		if err := rows.Scan(&id, &encrypted); err != nil {
			return err
		}
		if _, err := decrypt(encrypted); err != nil {
			return fmt.Errorf("destination %d cannot be decrypted", id)
		}
	}
	return rows.Err()
}

func (s *Store) RenameDestination(ctx context.Context, userID, destinationID int64, name string) error {
	name, err := destinationName(name)
	if err != nil {
		return err
	}
	result, err := s.execWriteContext(ctx, `UPDATE destinations SET name=? WHERE id=? AND user_id=?`,
		name, destinationID, userID)
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
}
func (s *Store) Destinations(ctx context.Context, userID int64) ([]Destination, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled,COALESCE(transport_status,'supported'),COALESCE(transport_message,'')
		FROM destinations WHERE user_id=? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Destination
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled, &d.TransportStatus, &d.TransportMessage); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// DestinationHealthByUser returns health for every destination owned by the
// user. Destinations created before the assurance migration are treated as
// healthy until their first delivery attempt.
func (s *Store) DestinationHealthByUser(ctx context.Context, userID int64) (map[int64]DestinationHealth, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,
		CASE WHEN `+supportedDestinationServicePredicate("d")+` THEN COALESCE(h.status,'healthy') ELSE 'unsupported' END,
		COALESCE(h.consecutive_failures,0),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='pending')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='pending'),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='blocked')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='blocked'),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='failed')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='failed'),
		h.last_success_at,h.last_failure_at,h.next_retry_at,COALESCE(h.last_error,''),
		COALESCE(h.updated_at,d.created_at)
		FROM destinations d LEFT JOIN destination_health h ON h.destination_id=d.id
		WHERE d.user_id=? ORDER BY d.id`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int64]DestinationHealth)
	for rows.Next() {
		var health DestinationHealth
		var status string
		var lastSuccess, lastFailure, nextRetry, lastError, updated sql.NullString
		if err := rows.Scan(&health.DestinationID, &status, &health.ConsecutiveFailures,
			&health.PendingCount, &health.BlockedCount, &health.FailedCount, &lastSuccess, &lastFailure, &nextRetry, &lastError, &updated); err != nil {
			return nil, err
		}
		health.Status = status
		health.LastError = safeDeliveryError(lastError.String)
		var parseErr error
		if health.LastSuccessAt, parseErr = parseStoredNullableTime(lastSuccess, "destination health last_success_at"); parseErr != nil {
			return nil, parseErr
		}
		if health.LastFailureAt, parseErr = parseStoredNullableTime(lastFailure, "destination health last_failure_at"); parseErr != nil {
			return nil, parseErr
		}
		if health.NextRetryAt, parseErr = parseStoredNullableTime(nextRetry, "destination health next_retry_at"); parseErr != nil {
			return nil, parseErr
		}
		if health.UpdatedAt, parseErr = parseStoredTime(updated.String, "destination health updated_at"); parseErr != nil {
			return nil, parseErr
		}
		result[health.DestinationID] = health
	}
	return result, rows.Err()
}

// AdminDestinationHealth is a compact household-wide view used by the admin
// dashboard. It never includes encrypted destination URLs or message bodies.
func (s *Store) AdminDestinationHealth(ctx context.Context) ([]AdminDestinationHealth, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,u.email,d.name,d.service,
		CASE WHEN `+supportedDestinationServicePredicate("d")+` THEN COALESCE(h.status,'healthy') ELSE 'unsupported' END,COALESCE(h.consecutive_failures,0),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='pending')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='pending'),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='blocked')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='blocked'),
		(SELECT COUNT(*) FROM deliveries WHERE destination_id=d.id AND status='failed')+
		(SELECT COUNT(*) FROM release_digest_deliveries WHERE destination_id=d.id AND status='failed'),
		h.last_success_at,h.last_failure_at,h.next_retry_at,COALESCE(h.last_error,''),
		COALESCE(h.updated_at,d.created_at)
		FROM destinations d JOIN users u ON u.id=d.user_id
		LEFT JOIN destination_health h ON h.destination_id=d.id
		ORDER BY CASE WHEN `+supportedDestinationServicePredicate("d")+` THEN CASE COALESCE(h.status,'healthy') WHEN 'paused' THEN 1 WHEN 'degraded' THEN 2 ELSE 3 END ELSE 0 END,
		d.name,u.email`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []AdminDestinationHealth
	for rows.Next() {
		var health AdminDestinationHealth
		var lastSuccess, lastFailure, nextRetry, lastError, updated sql.NullString
		if err := rows.Scan(&health.DestinationID, &health.UserEmail, &health.DestinationName,
			&health.Service, &health.Status, &health.ConsecutiveFailures, &health.PendingCount,
			&health.BlockedCount, &health.FailedCount, &lastSuccess, &lastFailure, &nextRetry, &lastError, &updated); err != nil {
			return nil, err
		}
		var parseErr error
		if health.LastSuccessAt, parseErr = parseStoredNullableTime(lastSuccess, "admin destination health last_success_at"); parseErr != nil {
			return nil, parseErr
		}
		if health.LastFailureAt, parseErr = parseStoredNullableTime(lastFailure, "admin destination health last_failure_at"); parseErr != nil {
			return nil, parseErr
		}
		if health.NextRetryAt, parseErr = parseStoredNullableTime(nextRetry, "admin destination health next_retry_at"); parseErr != nil {
			return nil, parseErr
		}
		health.LastError = safeDeliveryError(lastError.String)
		if health.UpdatedAt, parseErr = parseStoredTime(updated.String, "admin destination health updated_at"); parseErr != nil {
			return nil, parseErr
		}
		result = append(result, health)
	}
	return result, rows.Err()
}

// StartDeliveryAttempt records an attempt before decryption and network I/O.
// Keeping the destination name/service snapshot here makes the audit useful
// even if the destination is renamed or removed later.
func (s *Store) StartDeliveryAttempt(ctx context.Context, deliveryID, digestDeliveryID int64, destination Destination, attemptNumber int, started time.Time) (int64, error) {
	if (deliveryID == 0) == (digestDeliveryID == 0) {
		return 0, errors.New("exactly one delivery kind is required")
	}
	if attemptNumber < 1 {
		attemptNumber = 1
	}
	result, err := s.execWriteContext(ctx, `INSERT INTO delivery_attempts
		(delivery_id,digest_delivery_id,destination_id,destination_name,service,attempt_number,status,started_at)
		VALUES(?,?,?,?,?,?, 'started',?)`, nullableID(deliveryID), nullableID(digestDeliveryID),
		nullableID(destination.ID), destination.Name, destination.Service, attemptNumber, timeText(started))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// FinishDeliveryAttempt updates the attempt and destination circuit state in
// one writer transaction. Five consecutive failures pause the destination;
// a successful send clears the failure streak.
func (s *Store) FinishDeliveryAttempt(ctx context.Context, attemptID, destinationID int64, success bool, message string, nextRetry *time.Time, finished time.Time) error {
	if attemptID < 1 || destinationID < 1 {
		return errors.New("delivery attempt and destination are required")
	}
	message = safeDeliveryError(message)
	if len(message) > 500 {
		message = message[:500]
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	status := "failed"
	if success {
		status = "sent"
		message = ""
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET status=?,finished_at=?,last_error=? WHERE id=?`,
		status, timeText(finished), message, attemptID); err != nil {
		return err
	}
	if success {
		if _, err := tx.ExecContext(ctx, `INSERT INTO destination_health(destination_id,status,consecutive_failures,last_success_at,next_retry_at,last_error,updated_at)
			VALUES(?,'healthy',0,?,NULL,'',?)
			ON CONFLICT(destination_id) DO UPDATE SET
				status=CASE WHEN destination_health.status='paused' THEN 'paused' ELSE 'healthy' END,
				consecutive_failures=CASE WHEN destination_health.status='paused' THEN destination_health.consecutive_failures ELSE 0 END,
				last_success_at=excluded.last_success_at,
				next_retry_at=CASE WHEN destination_health.status='paused' THEN destination_health.next_retry_at ELSE NULL END,
				last_error=CASE WHEN destination_health.status='paused' THEN destination_health.last_error ELSE '' END,
				updated_at=excluded.updated_at`,
			destinationID, timeText(finished), timeText(finished)); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO destination_health(destination_id,status,consecutive_failures,last_failure_at,next_retry_at,last_error,updated_at)
			VALUES(?,CASE WHEN 1>=5 THEN 'paused' ELSE 'degraded' END,1,?,?,?,?)
			ON CONFLICT(destination_id) DO UPDATE SET
			consecutive_failures=destination_health.consecutive_failures+1,
			status=CASE WHEN destination_health.consecutive_failures+1>=5 THEN 'paused' ELSE 'degraded' END,
			last_failure_at=excluded.last_failure_at,next_retry_at=excluded.next_retry_at,last_error=excluded.last_error,updated_at=excluded.updated_at`,
			destinationID, timeText(finished), nullableTime(nextRetry), message, timeText(finished)); err != nil {
			return err
		}
		// Once the circuit pauses a destination, work that was already queued
		// must remain durable but cannot stay in the runnable queue. Convert all
		// currently pending rows to blocked atomically with the health update so
		// a maintenance tick cannot repeatedly claim work that is guaranteed to
		// fail. RetryFailedDeliveries explicitly moves these rows back to pending
		// after an operator recovers the destination.
		var healthStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM destination_health WHERE destination_id=?`, destinationID).Scan(&healthStatus); err != nil {
			return err
		}
		if healthStatus == "paused" {
			for _, query := range []string{
				`UPDATE deliveries
				 SET status='blocked',claim_owner=NULL,claim_expires_at=NULL,
				     last_error=CASE WHEN last_error='' THEN 'destination paused after repeated failures' ELSE last_error END
				 WHERE destination_id=? AND status='pending'`,
				`UPDATE release_digest_deliveries
				 SET status='blocked',claim_owner=NULL,claim_expires_at=NULL,
				     last_error=CASE WHEN last_error='' THEN 'destination paused after repeated failures' ELSE last_error END
				 WHERE destination_id=? AND status='pending'`,
			} {
				if _, err := tx.ExecContext(ctx, query, destinationID); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// RetryFailedDeliveries requeues all permanently failed deliveries for an
// owned destination. It is intentionally owner-scoped and resets the circuit
// only after the queue has been made runnable.
func (s *Store) RetryFailedDeliveries(ctx context.Context, userID, destinationID int64, now time.Time) (int, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM destinations WHERE id=? AND user_id=?`, destinationID, userID).Scan(&owned); err != nil {
		return 0, err
	}
	if owned == 0 {
		return 0, sql.ErrNoRows
	}
	count := 0
	for _, query := range []string{
		`UPDATE deliveries SET status=CASE WHEN COALESCE((SELECT transport_status FROM destinations WHERE id=deliveries.destination_id),'supported')='supported' AND LOWER(COALESCE((SELECT service FROM destinations WHERE id=deliveries.destination_id),'')) IN ('discord','telegram','ntfy','generic') THEN 'pending' ELSE 'blocked' END,
			attempts=0,next_attempt_at=?,last_error='',claim_owner=NULL,claim_expires_at=NULL WHERE destination_id=? AND status IN ('failed','blocked')`,
		`UPDATE release_digest_deliveries SET status=CASE WHEN COALESCE((SELECT transport_status FROM destinations WHERE id=release_digest_deliveries.destination_id),'supported')='supported' AND LOWER(COALESCE((SELECT service FROM destinations WHERE id=release_digest_deliveries.destination_id),'')) IN ('discord','telegram','ntfy','generic') THEN 'pending' ELSE 'blocked' END,
			attempts=0,next_attempt_at=?,last_error='',claim_owner=NULL,claim_expires_at=NULL WHERE destination_id=? AND status IN ('failed','blocked')`,
	} {
		result, updateErr := tx.ExecContext(ctx, query, timeText(now), destinationID)
		if updateErr != nil {
			return 0, updateErr
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return 0, rowsErr
		}
		count += int(changed)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO destination_health(destination_id,status,consecutive_failures,next_retry_at,last_error,updated_at)
		VALUES(?,'healthy',0,NULL,'',?)
		ON CONFLICT(destination_id) DO UPDATE SET status='healthy',consecutive_failures=0,next_retry_at=NULL,last_error='',updated_at=excluded.updated_at`, destinationID, timeText(now)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
func (s *Store) Destination(ctx context.Context, userID, id int64) (Destination, error) {
	var d Destination
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled,COALESCE(transport_status,'supported'),COALESCE(transport_message,'')
		FROM destinations WHERE user_id=? AND id=?`, userID, id).Scan(
		&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled, &d.TransportStatus, &d.TransportMessage)
	return d, err
}
func (s *Store) DeleteDestination(ctx context.Context, userID, id int64) error {
	result, err := s.execWriteContext(ctx, `DELETE FROM destinations WHERE user_id=? AND id=?`, userID, id)
	return changedOrNotFound(result, err)
}
func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at,
			dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,COALESCE(dst.transport_status,'supported'),COALESCE(dst.transport_message,''),
		e.title,e.body,e.event_type,rg.title
		FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id
		LEFT JOIN destination_health dh ON dh.destination_id=dst.id
		JOIN notification_events e ON e.id=d.event_id JOIN release_groups rg ON rg.id=e.release_group_id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND dst.enabled=1
		AND (d.claim_expires_at IS NULL OR d.claim_expires_at<=?)
		AND `+supportedDestinationServicePredicate("dst")+` AND `+destinationAdmissionPredicate+` ORDER BY d.next_attempt_at LIMIT ?`,
		timeText(now), timeText(now), limit)
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
		ORDER BY dd.next_attempt_at,dd.id LIMIT ?`, timeText(now), timeText(now), limit)
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

// ClaimDueDeliveries atomically leases runnable normal deliveries for one
// runner instance.  A second runner can see the row only after the lease
// expires, which gives us at-least-once processing without duplicate claims.
func (s *Store) ClaimDueDeliveries(ctx context.Context, now time.Time, limit int, owner string, lease time.Duration) ([]Delivery, error) {
	if limit < 1 {
		return nil, nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "legacy-worker"
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	expires := now.Add(lease)
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT d.id FROM deliveries d
		JOIN destinations dst ON dst.id=d.destination_id
		LEFT JOIN destination_health dh ON dh.destination_id=dst.id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND dst.enabled=1
		AND (d.claim_expires_at IS NULL OR d.claim_expires_at<=?)
		AND `+supportedDestinationServicePredicate("dst")+` AND `+destinationAdmissionPredicate+`
		ORDER BY d.next_attempt_at,d.id LIMIT ?`, timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET claim_owner=?,claim_expires_at=? WHERE id=? AND status='pending' AND (claim_expires_at IS NULL OR claim_expires_at<=?)`, owner, timeText(expires), id, timeText(now)); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, owner)
	rows, err = tx.QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at,
		dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,COALESCE(dst.transport_status,'supported'),COALESCE(dst.transport_message,''),
		e.title,e.body,e.event_type,rg.title
		FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id
		JOIN notification_events e ON e.id=d.event_id JOIN release_groups rg ON rg.id=e.release_group_id
		WHERE d.id IN (`+placeholders+`) AND d.claim_owner=? ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	var result []Delivery
	for rows.Next() {
		var d Delivery
		var next string
		if err := rows.Scan(&d.ID, &d.EventID, &d.Attempts, &next,
			&d.Destination.ID, &d.Destination.UserID, &d.Destination.Name, &d.Destination.Service,
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Destination.TransportStatus, &d.Destination.TransportMessage,
			&d.Title, &d.Body, &d.EventType, &d.ReleaseTitle); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.NextAttempt, err = parseStoredTime(next, "delivery next_attempt_at")
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.ClaimOwner = owner
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ClaimDueDigestDeliveries is the digest counterpart of ClaimDueDeliveries.
func (s *Store) ClaimDueDigestDeliveries(ctx context.Context, now time.Time, limit int, owner string, lease time.Duration) ([]DigestDelivery, error) {
	if limit < 1 {
		return nil, nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "legacy-worker"
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	expires := now.Add(lease)
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT dd.id FROM release_digest_deliveries dd
		JOIN destinations dst ON dst.id=dd.destination_id
		LEFT JOIN destination_health dh ON dh.destination_id=dst.id
		WHERE dd.status='pending' AND dd.next_attempt_at<=? AND dst.enabled=1
		AND (dd.claim_expires_at IS NULL OR dd.claim_expires_at<=?)
		AND `+supportedDestinationServicePredicate("dst")+` AND `+destinationAdmissionPredicate+`
		ORDER BY dd.next_attempt_at,dd.id LIMIT ?`, timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE release_digest_deliveries SET claim_owner=?,claim_expires_at=? WHERE id=? AND status='pending' AND (claim_expires_at IS NULL OR claim_expires_at<=?)`, owner, timeText(expires), id, timeText(now)); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, owner)
	rows, err = tx.QueryContext(ctx, `SELECT dd.id,dd.run_id,dd.attempts,dd.next_attempt_at,
		dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,COALESCE(dst.transport_status,'supported'),COALESCE(dst.transport_message,''),
		r.title,r.body
		FROM release_digest_deliveries dd JOIN release_digest_runs r ON r.id=dd.run_id
		JOIN destinations dst ON dst.id=dd.destination_id
		WHERE dd.id IN (`+placeholders+`) AND dd.claim_owner=? ORDER BY dd.id`, args...)
	if err != nil {
		return nil, err
	}
	var result []DigestDelivery
	for rows.Next() {
		var d DigestDelivery
		var next string
		if err := rows.Scan(&d.ID, &d.RunID, &d.Attempts, &next,
			&d.Destination.ID, &d.Destination.UserID, &d.Destination.Name, &d.Destination.Service,
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Destination.TransportStatus, &d.Destination.TransportMessage,
			&d.Title, &d.Body); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.NextAttempt, err = parseStoredTime(next, "digest delivery next_attempt_at")
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.ClaimOwner = owner
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) MarkDeliverySent(ctx context.Context, id int64, now time.Time) error {
	err := s.MarkDeliverySentOwned(ctx, id, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDeliverySentOwned(ctx context.Context, id int64, owner string, now time.Time) error {
	query := `UPDATE deliveries SET status='sent',attempts=attempts+1,sent_at=?,last_error='',claim_owner=NULL,claim_expires_at=NULL WHERE id=?`
	args := []any{timeText(now), id}
	if strings.TrimSpace(owner) != "" {
		query += ` AND claim_owner=?`
		args = append(args, owner)
	}
	result, err := s.execWriteContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && strings.TrimSpace(owner) != "" {
		return sql.ErrNoRows
	}
	if changed == 0 {
		return nil
	}
	return nil
}
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	err := s.MarkDeliveryFailedOwned(ctx, id, attempts, message, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDeliveryFailedOwned(ctx context.Context, id int64, attempts int, message, owner string, now time.Time) error {
	message = safeDeliveryError(message)
	status := "pending"
	if attempts >= 5 {
		status = "failed"
	}
	delay := time.Minute * time.Duration(1<<min(attempts, 6))
	if len(message) > 500 {
		message = message[:500]
	}
	query := `UPDATE deliveries SET status=?,attempts=?,next_attempt_at=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL WHERE id=?`
	args := []any{status, attempts, timeText(now.Add(delay)), message, id}
	if strings.TrimSpace(owner) != "" {
		query += ` AND claim_owner=?`
		args = append(args, owner)
	}
	result, err := s.execWriteContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && strings.TrimSpace(owner) != "" {
		return sql.ErrNoRows
	}
	if changed == 0 {
		return nil
	}
	return nil
}

func (s *Store) MarkDigestDeliverySent(ctx context.Context, id int64, now time.Time) error {
	err := s.MarkDigestDeliverySentOwned(ctx, id, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDigestDeliverySentOwned(ctx context.Context, id int64, owner string, now time.Time) error {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE release_digest_deliveries
		SET status='sent',attempts=attempts+1,sent_at=?,last_error='',claim_owner=NULL,claim_expires_at=NULL WHERE id=?`
	args := []any{timeText(now), id}
	if strings.TrimSpace(owner) != "" {
		query += ` AND claim_owner=?`
		args = append(args, owner)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && strings.TrimSpace(owner) != "" {
		return sql.ErrNoRows
	}
	if changed == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE release_digest_runs SET status=CASE WHEN EXISTS (
		SELECT 1 FROM release_digest_deliveries WHERE run_id=release_digest_runs.id AND status='failed'
	) THEN 'failed' ELSE 'sent' END
		WHERE id=(SELECT run_id FROM release_digest_deliveries WHERE id=?)
		AND NOT EXISTS (SELECT 1 FROM release_digest_deliveries WHERE run_id=release_digest_runs.id AND status IN ('pending','blocked'))`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkDigestDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	err := s.MarkDigestDeliveryFailedOwned(ctx, id, attempts, message, "", now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) MarkDigestDeliveryFailedOwned(ctx context.Context, id int64, attempts int, message, owner string, now time.Time) error {
	message = safeDeliveryError(message)
	status := "pending"
	if attempts >= 5 {
		status = "failed"
	}
	delay := time.Minute * time.Duration(1<<min(attempts, 6))
	if len(message) > 500 {
		message = message[:500]
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE release_digest_deliveries
		SET status=?,attempts=?,next_attempt_at=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL WHERE id=?`
	args := []any{status, attempts, timeText(now.Add(delay)), message, id}
	if strings.TrimSpace(owner) != "" {
		query += ` AND claim_owner=?`
		args = append(args, owner)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && strings.TrimSpace(owner) != "" {
		return sql.ErrNoRows
	}
	if changed == 0 {
		return nil
	}
	if status == "failed" {
		if _, err := tx.ExecContext(ctx, `UPDATE release_digest_runs SET status='failed'
			WHERE id=(SELECT run_id FROM release_digest_deliveries WHERE id=?)
			AND NOT EXISTS (SELECT 1 FROM release_digest_deliveries WHERE run_id=release_digest_runs.id AND status IN ('pending','blocked'))`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecoverExpiredWork makes abandoned claims runnable again. It is safe to
// call at startup and during maintenance: only expired ownership metadata is
// cleared, and sent/failed rows are never resurrected.
func (s *Store) RecoverExpiredWork(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	recovered := 0
	statements := []string{
		`UPDATE manual_sync_requests SET status='queued',started_at=NULL,lease_owner=NULL,lease_expires_at=NULL,last_error='worker lease expired' WHERE status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?`,
		`UPDATE deliveries SET claim_owner=NULL,claim_expires_at=NULL WHERE status='pending' AND claim_expires_at IS NOT NULL AND claim_expires_at<=?`,
		`UPDATE release_digest_deliveries SET claim_owner=NULL,claim_expires_at=NULL WHERE status='pending' AND claim_expires_at IS NOT NULL AND claim_expires_at<=?`,
	}
	for _, statement := range statements {
		result, err := tx.ExecContext(ctx, statement, timeText(now))
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		recovered += int(changed)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}

// ReconcileStaleDeliveryAttempts turns attempts that have no completion
// record after a bounded window into failed audit entries. The queue row is
// left pending and will be retried by its normal lease/attempt policy.
func (s *Store) ReconcileStaleDeliveryAttempts(ctx context.Context, now time.Time, staleAfter time.Duration) (int, error) {
	if staleAfter <= 0 {
		staleAfter = 10 * time.Minute
	}
	cutoff := now.Add(-staleAfter)
	result, err := s.execWriteContext(ctx, `UPDATE delivery_attempts
		SET status='failed',finished_at=?,abandoned_at=?,last_error='worker attempt expired'
		WHERE status='started' AND started_at<=?`, timeText(now), timeText(now), timeText(cutoff))
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	return int(changed), err
}
func (s *Store) DeliveryHistory(ctx context.Context, userID int64, limit int) ([]DeliveryHistory, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT title,event_type,destination,status,attempts,last_error,created_at,sent_at
		FROM (
			SELECT e.title,e.event_type,dst.name AS destination,d.status,d.attempts,d.last_error,e.created_at,d.sent_at,d.id AS sort_id
			FROM notification_events e LEFT JOIN deliveries d ON d.event_id=e.id
			LEFT JOIN destinations dst ON dst.id=d.destination_id WHERE e.user_id=?
			UNION ALL
			SELECT r.title,'digest',dst.name,dd.status,dd.attempts,dd.last_error,r.created_at,dd.sent_at,dd.id
			FROM release_digest_runs r JOIN release_digest_deliveries dd ON dd.run_id=r.id
			LEFT JOIN destinations dst ON dst.id=dd.destination_id WHERE r.user_id=?
		) ORDER BY created_at DESC,sort_id DESC LIMIT ?`, userID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []DeliveryHistory
	for rows.Next() {
		var h DeliveryHistory
		var dest, status sql.NullString
		var attempts sql.NullInt64
		var created string
		var sent sql.NullString
		var lastError sql.NullString
		if err := rows.Scan(&h.Title, &h.EventType, &dest, &status, &attempts, &lastError, &created, &sent); err != nil {
			return nil, err
		}
		h.Destination, h.Status, h.Attempts, h.LastError = dest.String, status.String, int(attempts.Int64), safeDeliveryError(lastError.String)
		if h.Destination == "" {
			h.Destination, h.Status = "No destination configured", "not sent"
		}
		h.CreatedAt, err = parseStoredTime(created, "delivery history created_at")
		if err != nil {
			return nil, err
		}
		if h.SentAt, err = parseStoredNullableTime(sent, "delivery history sent_at"); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}
func (s *Store) AdminDeliveryHistoryCount(ctx context.Context) (int, error) {
	var count int
	err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events e
		LEFT JOIN deliveries d ON d.event_id=e.id`).Scan(&count)
	return count, err
}
func (s *Store) AdminDeliveryHistory(ctx context.Context, limit, offset int) ([]AdminDeliveryHistory, error) {
	if limit < 1 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,u.email,e.title,e.body,e.event_type,
		dst.name,dst.service,d.status,d.attempts,d.last_error,e.created_at,d.next_attempt_at,d.sent_at
		FROM notification_events e
		JOIN users u ON u.id=e.user_id
		LEFT JOIN deliveries d ON d.event_id=e.id
		LEFT JOIN destinations dst ON dst.id=d.destination_id
		ORDER BY e.created_at DESC,d.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []AdminDeliveryHistory
	for rows.Next() {
		var h AdminDeliveryHistory
		var destination, service, status, lastError sql.NullString
		var attempts sql.NullInt64
		var deliveryID sql.NullInt64
		var created string
		var nextAttempt, sent sql.NullString
		if err := rows.Scan(
			&deliveryID, &h.UserEmail, &h.Title, &h.Body, &h.EventType,
			&destination, &service, &status, &attempts, &lastError,
			&created, &nextAttempt, &sent,
		); err != nil {
			return nil, err
		}
		if deliveryID.Valid {
			h.DeliveryID = deliveryID.Int64
		}
		h.Destination, h.Service = destination.String, service.String
		h.Status, h.Attempts, h.LastError = status.String, int(attempts.Int64), safeDeliveryError(lastError.String)
		if h.Destination == "" {
			h.Destination, h.Status = "No destination configured", "not sent"
		}
		h.CreatedAt, err = parseStoredTime(created, "admin delivery history created_at")
		if err != nil {
			return nil, err
		}
		if h.NextAttempt, err = parseStoredNullableTime(nextAttempt, "admin delivery history next_attempt_at"); err != nil {
			return nil, err
		}
		if h.SentAt, err = parseStoredNullableTime(sent, "admin delivery history sent_at"); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// AdminDeliveryHistorySummary deliberately omits notification bodies and
// provider error strings from the normal admin page. An administrator can
// request one specific record through AdminDeliveryDetail when investigation
// requires the full content, keeping bulk household data out of routine HTML.
func (s *Store) AdminDeliveryHistorySummary(ctx context.Context, limit, offset int) ([]AdminDeliveryHistory, error) {
	if limit < 1 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,u.email,e.title,e.event_type,
		dst.name,dst.service,d.status,d.attempts,e.created_at,d.next_attempt_at,d.sent_at
		FROM notification_events e
		JOIN users u ON u.id=e.user_id
		LEFT JOIN deliveries d ON d.event_id=e.id
		LEFT JOIN destinations dst ON dst.id=d.destination_id
		ORDER BY e.created_at DESC,d.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []AdminDeliveryHistory
	for rows.Next() {
		var h AdminDeliveryHistory
		var deliveryID, attempts sql.NullInt64
		var destination, service, status sql.NullString
		var created, nextAttempt, sent sql.NullString
		if err := rows.Scan(&deliveryID, &h.UserEmail, &h.Title, &h.EventType,
			&destination, &service, &status, &attempts, &created, &nextAttempt, &sent); err != nil {
			return nil, err
		}
		if deliveryID.Valid {
			h.DeliveryID = deliveryID.Int64
		}
		h.Destination, h.Service = destination.String, service.String
		h.Status, h.Attempts = status.String, int(attempts.Int64)
		if h.Destination == "" {
			h.Destination, h.Status = "No destination configured", "not sent"
		}
		var parseErr error
		h.CreatedAt, parseErr = parseStoredTime(created.String, "admin delivery history created_at")
		if parseErr != nil {
			return nil, parseErr
		}
		if h.NextAttempt, parseErr = parseStoredNullableTime(nextAttempt, "admin delivery history next_attempt_at"); parseErr != nil {
			return nil, parseErr
		}
		if h.SentAt, parseErr = parseStoredNullableTime(sent, "admin delivery history sent_at"); parseErr != nil {
			return nil, parseErr
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func (s *Store) AdminDeliveryDetail(ctx context.Context, deliveryID int64) (AdminDeliveryHistory, error) {
	var h AdminDeliveryHistory
	var destination, service, status, lastError sql.NullString
	var attempts sql.NullInt64
	var created, nextAttempt, sent sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT d.id,u.email,e.title,e.body,e.event_type,
		dst.name,dst.service,d.status,d.attempts,d.last_error,e.created_at,d.next_attempt_at,d.sent_at
		FROM deliveries d JOIN notification_events e ON e.id=d.event_id
		JOIN users u ON u.id=e.user_id LEFT JOIN destinations dst ON dst.id=d.destination_id
		WHERE d.id=?`, deliveryID).Scan(&h.DeliveryID, &h.UserEmail, &h.Title, &h.Body, &h.EventType,
		&destination, &service, &status, &attempts, &lastError, &created, &nextAttempt, &sent)
	if err != nil {
		return h, err
	}
	h.Destination, h.Service = destination.String, service.String
	h.Status, h.Attempts, h.LastError = status.String, int(attempts.Int64), safeDeliveryError(lastError.String)
	if h.Destination == "" {
		h.Destination, h.Status = "No destination configured", "not sent"
	}
	h.CreatedAt, err = parseStoredTime(created.String, "delivery detail created_at")
	if err != nil {
		return h, err
	}
	if h.NextAttempt, err = parseStoredNullableTime(nextAttempt, "delivery detail next_attempt_at"); err != nil {
		return h, err
	}
	if h.SentAt, err = parseStoredNullableTime(sent, "delivery detail sent_at"); err != nil {
		return h, err
	}
	return h, nil
}
func (s *Store) NotificationPreferences(ctx context.Context, userID int64) (NotificationPreferences, error) {
	var p NotificationPreferences
	p.UserID = userID
	var albums, eps, singles, announcements, releaseDay, digestEnabled, holdConflicts int
	var digestFrequency string
	err := s.readerDB().QueryRowContext(ctx, `SELECT albums,eps,singles,announcements,release_day,
		release_digest_enabled,release_digest_frequency,hold_conflicting_notifications
		FROM notification_preferences WHERE user_id=?`, userID).Scan(&albums, &eps, &singles, &announcements, &releaseDay, &digestEnabled, &digestFrequency, &holdConflicts)
	if err == sql.ErrNoRows {
		_, err = s.execWriteContext(ctx, `INSERT OR IGNORE INTO notification_preferences(user_id,updated_at) VALUES(?,?)`, userID, nowText())
		if err == nil {
			p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = true, true, true, true, true
			p.DigestFrequency = "weekly"
		}
		return p, err
	}
	p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = albums != 0, eps != 0, singles != 0, announcements != 0, releaseDay != 0
	p.DigestEnabled, p.DigestFrequency = digestEnabled != 0, normalizeDigestFrequency(digestFrequency)
	p.HoldConflictingNotifications = holdConflicts != 0
	return p, err
}
func (s *Store) UpdateNotificationPreferences(ctx context.Context, p NotificationPreferences) error {
	p.DigestFrequency = normalizeDigestFrequency(p.DigestFrequency)
	_, err := s.execWriteContext(ctx, `INSERT INTO notification_preferences(user_id,albums,eps,singles,announcements,release_day,
		release_digest_enabled,release_digest_frequency,hold_conflicting_notifications,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET albums=excluded.albums,eps=excluded.eps,singles=excluded.singles,
		announcements=excluded.announcements,release_day=excluded.release_day,
		release_digest_enabled=excluded.release_digest_enabled,release_digest_frequency=excluded.release_digest_frequency,
		hold_conflicting_notifications=excluded.hold_conflicting_notifications,updated_at=excluded.updated_at`,
		p.UserID, boolInt(p.Albums), boolInt(p.EPs), boolInt(p.Singles), boolInt(p.Announcements), boolInt(p.ReleaseDay),
		boolInt(p.DigestEnabled), p.DigestFrequency, boolInt(p.HoldConflictingNotifications), nowText())
	return err
}

func normalizeDigestFrequency(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "daily") {
		return "daily"
	}
	return "weekly"
}
