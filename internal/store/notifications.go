package store

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

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
	_, err = s.DB.ExecContext(ctx, `INSERT INTO destinations(user_id,name,service,encrypted_url,created_at)
		VALUES(?,?,?,?,?)`, userID, name, service, encrypted, nowText())
	return err
}
func (s *Store) RenameDestination(ctx context.Context, userID, destinationID int64, name string) error {
	name, err := destinationName(name)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE destinations SET name=? WHERE id=? AND user_id=?`,
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
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled
		FROM destinations WHERE user_id=? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Destination
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (s *Store) Destination(ctx context.Context, userID, id int64) (Destination, error) {
	var d Destination
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled
		FROM destinations WHERE user_id=? AND id=?`, userID, id).Scan(
		&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled)
	return d, err
}
func (s *Store) DeleteDestination(ctx context.Context, userID, id int64) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM destinations WHERE user_id=? AND id=?`, userID, id)
	return changedOrNotFound(result, err)
}
func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at,
		dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,
		e.title,e.body,e.event_type,rg.title
		FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id
		JOIN notification_events e ON e.id=d.event_id JOIN release_groups rg ON rg.id=e.release_group_id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND dst.enabled=1 ORDER BY d.next_attempt_at LIMIT ?`,
		timeText(now), limit)
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
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Title, &d.Body, &d.EventType, &d.ReleaseTitle); err != nil {
			return nil, err
		}
		d.NextAttempt, _ = parseTime(next)
		result = append(result, d)
	}
	return result, rows.Err()
}
func (s *Store) MarkDeliverySent(ctx context.Context, id int64, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status='sent',attempts=attempts+1,sent_at=?,last_error='' WHERE id=?`,
		timeText(now), id)
	return err
}
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	message = safeDeliveryError(message)
	status := "pending"
	if attempts >= 5 {
		status = "failed"
	}
	delay := time.Minute * time.Duration(1<<min(attempts, 6))
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status=?,attempts=?,next_attempt_at=?,last_error=? WHERE id=?`,
		status, attempts, timeText(now.Add(delay)), message, id)
	return err
}
func (s *Store) DeliveryHistory(ctx context.Context, userID int64, limit int) ([]DeliveryHistory, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT e.title,e.event_type,dst.name,d.status,d.attempts,d.last_error,e.created_at,d.sent_at
		FROM notification_events e LEFT JOIN deliveries d ON d.event_id=e.id
		LEFT JOIN destinations dst ON dst.id=d.destination_id WHERE e.user_id=?
		ORDER BY e.created_at DESC,d.id DESC LIMIT ?`, userID, limit)
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
		h.CreatedAt, _ = parseTime(created)
		if sent.Valid {
			t, _ := parseTime(sent.String)
			h.SentAt = &t
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
		h.CreatedAt, _ = parseTime(created)
		if nextAttempt.Valid {
			t, _ := parseTime(nextAttempt.String)
			h.NextAttempt = &t
		}
		if sent.Valid {
			t, _ := parseTime(sent.String)
			h.SentAt = &t
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
		h.CreatedAt, _ = parseTime(created.String)
		if nextAttempt.Valid {
			t, _ := parseTime(nextAttempt.String)
			h.NextAttempt = &t
		}
		if sent.Valid {
			t, _ := parseTime(sent.String)
			h.SentAt = &t
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
	h.CreatedAt, _ = parseTime(created.String)
	if nextAttempt.Valid {
		t, _ := parseTime(nextAttempt.String)
		h.NextAttempt = &t
	}
	if sent.Valid {
		t, _ := parseTime(sent.String)
		h.SentAt = &t
	}
	return h, nil
}
func (s *Store) NotificationPreferences(ctx context.Context, userID int64) (NotificationPreferences, error) {
	var p NotificationPreferences
	p.UserID = userID
	var albums, eps, singles, announcements, releaseDay int
	err := s.DB.QueryRowContext(ctx, `SELECT albums,eps,singles,announcements,release_day FROM notification_preferences WHERE user_id=?`, userID).Scan(&albums, &eps, &singles, &announcements, &releaseDay)
	if err == sql.ErrNoRows {
		_, err = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO notification_preferences(user_id,updated_at) VALUES(?,?)`, userID, nowText())
		if err == nil {
			p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = true, true, true, true, true
		}
		return p, err
	}
	p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = albums != 0, eps != 0, singles != 0, announcements != 0, releaseDay != 0
	return p, err
}
func (s *Store) UpdateNotificationPreferences(ctx context.Context, p NotificationPreferences) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO notification_preferences(user_id,albums,eps,singles,announcements,release_day,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET albums=excluded.albums,eps=excluded.eps,singles=excluded.singles,announcements=excluded.announcements,release_day=excluded.release_day,updated_at=excluded.updated_at`,
		p.UserID, boolInt(p.Albums), boolInt(p.EPs), boolInt(p.Singles), boolInt(p.Announcements), boolInt(p.ReleaseDay), nowText())
	return err
}
