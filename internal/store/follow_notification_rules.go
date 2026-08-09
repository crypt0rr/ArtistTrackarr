package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const followNotificationRuleColumns = `r.user_id,r.artist_id,r.delivery_mode,
	r.include_primary,r.include_featured,r.albums,r.eps,r.singles,r.compilations,
	r.announcements,r.release_day,r.paused_until,r.updated_at`

func normalizeFollowDeliveryMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case FollowDeliveryImmediate:
		return FollowDeliveryImmediate
	case FollowDeliveryDigest:
		return FollowDeliveryDigest
	case FollowDeliveryOff:
		return FollowDeliveryOff
	default:
		return FollowDeliveryInherit
	}
}

func validFollowDeliveryMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case FollowDeliveryInherit, FollowDeliveryImmediate, FollowDeliveryDigest, FollowDeliveryOff:
		return true
	default:
		return false
	}
}

func (r FollowNotificationRule) effectiveDeliveryMode(now time.Time) string {
	if r.PausedUntil != nil && now.Before(*r.PausedUntil) {
		return FollowDeliveryOff
	}
	return normalizeFollowDeliveryMode(r.DeliveryMode)
}

// AllowsContent determines whether an event for this follow should exist in
// the member's notification/inbox history. DeliveryMode is intentionally not
// considered here: digest and paused rules retain the event for visibility,
// while deciding whether a destination receives it is handled separately.
func (r FollowNotificationRule) AllowsContent(primaryType, creditRole, eventType string, now time.Time) bool {
	if strings.EqualFold(strings.TrimSpace(creditRole), "featured") {
		if !r.IncludeFeatured {
			return false
		}
	} else if !r.IncludePrimary {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(primaryType)) {
	case "album":
		if !r.Albums {
			return false
		}
	case "ep":
		if !r.EPs {
			return false
		}
	case "single":
		if !r.Singles {
			return false
		}
	case "compilation":
		if !r.Compilations {
			return false
		}
	}
	switch eventType {
	case "announcement":
		return r.Announcements
	case "release_day":
		return r.ReleaseDay
	default:
		_ = now
		return true
	}
}

func (r FollowNotificationRule) queuesImmediate(now time.Time) bool {
	mode := r.effectiveDeliveryMode(now)
	return mode == FollowDeliveryInherit || mode == FollowDeliveryImmediate
}

func (r FollowNotificationRule) belongsInDigest(now time.Time) bool {
	mode := r.effectiveDeliveryMode(now)
	return mode == FollowDeliveryInherit || mode == FollowDeliveryDigest
}

func scanFollowNotificationRule(row interface{ Scan(...any) error }) (FollowNotificationRule, error) {
	var rule FollowNotificationRule
	var mode string
	var primary, featured, albums, eps, singles, compilations, announcements, releaseDay int
	var paused, updated sql.NullString
	if err := row.Scan(&rule.UserID, &rule.ArtistID, &mode, &primary, &featured,
		&albums, &eps, &singles, &compilations, &announcements, &releaseDay, &paused, &updated); err != nil {
		return rule, err
	}
	rule.DeliveryMode = normalizeFollowDeliveryMode(mode)
	rule.IncludePrimary, rule.IncludeFeatured = primary != 0, featured != 0
	rule.Albums, rule.EPs, rule.Singles, rule.Compilations = albums != 0, eps != 0, singles != 0, compilations != 0
	rule.Announcements, rule.ReleaseDay = announcements != 0, releaseDay != 0
	if paused.Valid && strings.TrimSpace(paused.String) != "" {
		if value, err := parseTime(paused.String); err == nil {
			rule.PausedUntil = &value
		}
	}
	rule.UpdatedAt, _ = parseTime(updated.String)
	return rule, nil
}

// FollowNotificationRules returns rules for the requested follows. A nil or
// empty artist list loads every follow for the owner, which is useful for the
// digest builder; requested IDs that predate the migration receive the same
// safe inherit defaults as a newly-created follow.
func (s *Store) FollowNotificationRules(ctx context.Context, userID int64, artistIDs []int64) (map[int64]FollowNotificationRule, error) {
	now := time.Now().UTC()
	result := make(map[int64]FollowNotificationRule)
	followQuery := `SELECT artist_id FROM follows WHERE user_id=?`
	followArgs := []any{userID}
	query := `SELECT ` + followNotificationRuleColumns + `
		FROM follow_notification_rules r JOIN follows f ON f.user_id=r.user_id AND f.artist_id=r.artist_id
		WHERE r.user_id=?`
	args := []any{userID}
	if len(artistIDs) > 0 {
		placeholders := make([]string, 0, len(artistIDs))
		seen := make(map[int64]struct{}, len(artistIDs))
		for _, artistID := range artistIDs {
			if artistID < 1 {
				continue
			}
			if _, exists := seen[artistID]; exists {
				continue
			}
			seen[artistID] = struct{}{}
			placeholders = append(placeholders, "?")
			args = append(args, artistID)
			followArgs = append(followArgs, artistID)
		}
		if len(placeholders) == 0 {
			return result, nil
		}
		followQuery += ` AND artist_id IN (` + strings.Join(placeholders, ",") + ")"
		query += ` AND r.artist_id IN (` + strings.Join(placeholders, ",") + ")"
	}
	followRows, err := s.readerDB().QueryContext(ctx, followQuery, followArgs...)
	if err != nil {
		return nil, err
	}
	for followRows.Next() {
		var artistID int64
		if err := followRows.Scan(&artistID); err != nil {
			_ = followRows.Close()
			return nil, err
		}
		result[artistID] = defaultFollowNotificationRule(userID, artistID, now)
	}
	if err := followRows.Err(); err != nil {
		_ = followRows.Close()
		return nil, err
	}
	if err := followRows.Close(); err != nil {
		return nil, err
	}
	rows, err := s.readerDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		rule, err := scanFollowNotificationRule(rows)
		if err != nil {
			return nil, err
		}
		result[rule.ArtistID] = rule
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) FollowNotificationRule(ctx context.Context, userID, artistID int64) (FollowNotificationRule, error) {
	rules, err := s.FollowNotificationRules(ctx, userID, []int64{artistID})
	if err != nil {
		return FollowNotificationRule{}, err
	}
	rule, ok := rules[artistID]
	if !ok {
		return FollowNotificationRule{}, sql.ErrNoRows
	}
	return rule, nil
}

// UpdateFollowNotificationRules applies one complete policy to a set of
// owner-scoped follows. It validates all IDs before changing any row, so a
// mixed or cross-user submission cannot partially update a batch.
func (s *Store) UpdateFollowNotificationRules(ctx context.Context, userID int64, artistIDs []int64, rule FollowNotificationRule) (int, error) {
	if userID < 1 {
		return 0, errors.New("user is required")
	}
	ids := make([]int64, 0, len(artistIDs))
	seen := make(map[int64]struct{}, len(artistIDs))
	for _, artistID := range artistIDs {
		if artistID < 1 {
			continue
		}
		if _, exists := seen[artistID]; exists {
			continue
		}
		seen[artistID] = struct{}{}
		ids = append(ids, artistID)
	}
	if len(ids) == 0 {
		return 0, errors.New("select at least one followed artist")
	}
	if !validFollowDeliveryMode(rule.DeliveryMode) {
		return 0, fmt.Errorf("invalid follow notification delivery mode")
	}
	rule.DeliveryMode = normalizeFollowDeliveryMode(rule.DeliveryMode)
	now := time.Now().UTC()
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, artistID := range ids {
		args = append(args, artistID)
	}
	var followed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=? AND artist_id IN (`+placeholders+`)`, args...).Scan(&followed); err != nil {
		return 0, err
	}
	if followed != len(ids) {
		return 0, sql.ErrNoRows
	}
	for _, artistID := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO follow_notification_rules
			(user_id,artist_id,delivery_mode,include_primary,include_featured,albums,eps,singles,compilations,announcements,release_day,paused_until,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id,artist_id) DO UPDATE SET
			delivery_mode=excluded.delivery_mode,include_primary=excluded.include_primary,
			include_featured=excluded.include_featured,albums=excluded.albums,eps=excluded.eps,
			singles=excluded.singles,compilations=excluded.compilations,
			announcements=excluded.announcements,release_day=excluded.release_day,
			paused_until=excluded.paused_until,updated_at=excluded.updated_at`,
			userID, artistID, rule.DeliveryMode, boolInt(rule.IncludePrimary), boolInt(rule.IncludeFeatured),
			boolInt(rule.Albums), boolInt(rule.EPs), boolInt(rule.Singles), boolInt(rule.Compilations),
			boolInt(rule.Announcements), boolInt(rule.ReleaseDay), nullableTime(rule.PausedUntil), timeText(now)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (s *Store) UpdateFollowNotificationRule(ctx context.Context, userID, artistID int64, rule FollowNotificationRule) error {
	_, err := s.UpdateFollowNotificationRules(ctx, userID, []int64{artistID}, rule)
	return err
}

// SetFollowNotificationDeliveryMode updates only the mode for a selected
// group, preserving each artist's release-type and credit filters.
func (s *Store) SetFollowNotificationDeliveryMode(ctx context.Context, userID int64, artistIDs []int64, mode string) (int, error) {
	if !validFollowDeliveryMode(mode) {
		return 0, fmt.Errorf("invalid follow notification delivery mode")
	}
	mode = normalizeFollowDeliveryMode(mode)
	ids := make([]int64, 0, len(artistIDs))
	seen := make(map[int64]struct{}, len(artistIDs))
	for _, artistID := range artistIDs {
		if artistID < 1 {
			continue
		}
		if _, exists := seen[artistID]; exists {
			continue
		}
		seen[artistID] = struct{}{}
		ids = append(ids, artistID)
	}
	if len(ids) == 0 {
		return 0, errors.New("select at least one followed artist")
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, artistID := range ids {
		args = append(args, artistID)
	}
	var followed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=? AND artist_id IN (`+placeholders+`)`, args...).Scan(&followed); err != nil {
		return 0, err
	}
	if followed != len(ids) {
		return 0, sql.ErrNoRows
	}
	args = []any{mode, timeText(time.Now().UTC()), userID}
	for _, artistID := range ids {
		args = append(args, artistID)
	}
	result, err := tx.ExecContext(ctx, `UPDATE follow_notification_rules SET delivery_mode=?,updated_at=?
		WHERE user_id=? AND artist_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(changed), nil
}

func (s *Store) PauseFollowNotificationRule(ctx context.Context, userID, artistID int64, until *time.Time) error {
	rule, err := s.FollowNotificationRule(ctx, userID, artistID)
	if err != nil {
		return err
	}
	rule.PausedUntil = until
	_, err = s.UpdateFollowNotificationRules(ctx, userID, []int64{artistID}, rule)
	return err
}

func followRuleFromColumns(mode string, primary, featured, albums, eps, singles, compilations, announcements, releaseDay int, paused string, updated string, userID, artistID int64) FollowNotificationRule {
	rule := defaultFollowNotificationRule(userID, artistID, time.Now().UTC())
	rule.DeliveryMode = normalizeFollowDeliveryMode(mode)
	rule.IncludePrimary, rule.IncludeFeatured = primary != 0, featured != 0
	rule.Albums, rule.EPs, rule.Singles, rule.Compilations = albums != 0, eps != 0, singles != 0, compilations != 0
	rule.Announcements, rule.ReleaseDay = announcements != 0, releaseDay != 0
	if value, err := parseTime(strings.TrimSpace(paused)); err == nil && strings.TrimSpace(paused) != "" {
		rule.PausedUntil = &value
	}
	rule.UpdatedAt, _ = parseTime(updated)
	return rule
}
