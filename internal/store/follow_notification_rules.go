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

// AllowsRelease determines whether an event for this follow should exist in
// the member's notification/inbox history. Spotify, iTunes, and MusicBrainz
// normalize compilations as primary type Album with Compilation in the
// secondary types, so compilations have their own filter and must not be
// accidentally controlled by the Albums checkbox.
func (r FollowNotificationRule) AllowsRelease(primaryType string, secondaryTypes []string, creditRole, eventType string, now time.Time) bool {
	// "featured" and "guest" are both appearance credits: release_credits.role
	// is CHECK(role IN ('primary','featured','guest')) and both iTunes and
	// MusicBrainz emit "guest". The README states that follow rules including
	// featured appearances also include guest credits, so both must be gated by
	// IncludeFeatured. Gating "guest" by IncludePrimary inverts that.
	switch strings.ToLower(strings.TrimSpace(creditRole)) {
	case "featured", "guest":
		if !r.IncludeFeatured {
			return false
		}
	default:
		if !r.IncludePrimary {
			return false
		}
	}
	compilation := strings.EqualFold(strings.TrimSpace(primaryType), "compilation")
	if !compilation {
		for _, secondary := range secondaryTypes {
			if strings.EqualFold(strings.TrimSpace(secondary), "compilation") {
				compilation = true
				break
			}
		}
	}
	if compilation {
		if !r.Compilations {
			return false
		}
	} else {
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

// pausedDeliveryResumesAt reports when a paused follow's notifications become
// deliverable. Pausing is a deferral, not a discard: suppressing the delivery
// outright lost the alert permanently, because notification_events is unique
// per (user, release, event type) and the release is no longer newly observed
// once the pause expires. Only immediate delivery is deferred; a follow that is
// digest-only or switched off keeps that behaviour while paused.
func (r FollowNotificationRule) pausedDeliveryResumesAt(now time.Time) (time.Time, bool) {
	if r.PausedUntil == nil || !now.Before(*r.PausedUntil) {
		return time.Time{}, false
	}
	switch normalizeFollowDeliveryMode(r.DeliveryMode) {
	case FollowDeliveryInherit, FollowDeliveryImmediate:
		return *r.PausedUntil, true
	default:
		return time.Time{}, false
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

// allowsAccountNotificationMoment applies the account-wide event-moment
// preferences to follows that still inherit account defaults. A follow with
// an explicit delivery mode is intentionally treated as an override: its
// content filters continue to decide whether the event is eligible, while the
// mode controls whether it is delivered immediately, in a digest, or retained
// only in history. This keeps the account settings useful without making an
// explicit per-follow rule impossible to use.
func (r FollowNotificationRule) allowsAccountNotificationMoment(p NotificationPreferences, eventType string) bool {
	if normalizeFollowDeliveryMode(r.DeliveryMode) != FollowDeliveryInherit {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "announcement":
		return p.Announcements
	case "release_day":
		return p.ReleaseDay
	default:
		return true
	}
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
	var parseErr error
	if rule.PausedUntil, parseErr = parseStoredNullableTime(paused, "follow notification rule paused_until"); parseErr != nil {
		return rule, parseErr
	}
	if !updated.Valid || strings.TrimSpace(updated.String) == "" {
		rule.UpdatedAt = time.Now().UTC()
		return rule, nil
	}
	rule.UpdatedAt, parseErr = parseStoredTime(updated.String, "follow notification rule updated_at")
	return rule, parseErr
}

// FollowNotificationRules returns rules for the requested follows. A nil or
// empty artist list loads every follow for the owner, which is useful for the
// digest builder; requested IDs that predate the migration receive the same
// safe inherit defaults as a newly-created follow.
func (s *Store) FollowNotificationRules(ctx context.Context, userID int64, artistIDs []int64) (map[int64]FollowNotificationRule, error) {
	return followNotificationRulesQ(ctx, s.readerDB(), userID, artistIDs)
}

// followNotificationRulesTx reads rules inside an open write transaction, so a
// caller can read, decide and write atomically.
func followNotificationRulesTx(ctx context.Context, tx *sql.Tx, userID int64, artistIDs []int64) (map[int64]FollowNotificationRule, error) {
	return followNotificationRulesQ(ctx, tx, userID, artistIDs)
}

// followNotificationRulesQ returns one rule per followed artist, defaulting for
// follows that have no explicit row yet.
//
// Each cursor is drained and closed before the next statement runs. The reader
// pool is four connections, and holding one cursor open across a second query
// is how that pool gets exhausted.
func followNotificationRulesQ(ctx context.Context, q rowQuerier, userID int64, artistIDs []int64) (map[int64]FollowNotificationRule, error) {
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
	if err := func() error {
		followRows, err := q.QueryContext(ctx, followQuery, followArgs...)
		if err != nil {
			return err
		}
		defer func() { _ = followRows.Close() }()
		for followRows.Next() {
			var artistID int64
			if err := followRows.Scan(&artistID); err != nil {
				return err
			}
			result[artistID] = defaultFollowNotificationRule(userID, artistID, now)
		}
		return followRows.Err()
	}(); err != nil {
		return nil, err
	}
	if err := func() error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			rule, err := scanFollowNotificationRule(rows)
			if err != nil {
				return err
			}
			result[rule.ArtistID] = rule
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}
	return result, nil
}

// upsertFollowNotificationRulesTx is the write half of
// UpdateFollowNotificationRules, extracted so a caller can perform the rule
// write and the delivery realignment in ONE transaction against ONE clock.
// Pausing previously wrote the rule in one transaction and realigned in a
// second, with a second time.Now(): the recompute must see the pause it is
// reacting to, which is only guaranteed inside the same transaction.
func upsertFollowNotificationRulesTx(ctx context.Context, tx *sql.Tx, userID int64, ids []int64, rule FollowNotificationRule, now time.Time) error {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, artistID := range ids {
		args = append(args, artistID)
	}
	var followed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=? AND artist_id IN (`+placeholders+`)`, args...).Scan(&followed); err != nil {
		return err
	}
	if followed != len(ids) {
		return sql.ErrNoRows
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
			return err
		}
	}
	return nil
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
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (int, error) {
		if err := upsertFollowNotificationRulesTx(ctx, tx, userID, ids, rule, now); err != nil {
			return 0, err
		}
		// A rule change can hand governance of a queued delivery from one follow
		// to another - a pause, or a mode change that outranks the incumbent -
		// so the queue has to be re-derived here too, not only on pause.
		if err := realignQueuedDeliveriesTx(ctx, tx, userID, ids, now); err != nil {
			return 0, err
		}
		return len(ids), nil
	})
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
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (int, error) {
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
		// A mode change can outrank the follow that currently governs a queued
		// delivery, so the schedule has to be re-derived here as well.
		if err := realignQueuedDeliveriesTx(ctx, tx, userID, ids, time.Now().UTC()); err != nil {
			return 0, err
		}
		return int(changed), nil
	})
}

// PauseFollowNotificationRule sets or clears a follow's pause and moves the
// deliveries that pause governs to match, in one transaction against one clock.
//
// The realignment must run after the upsert so the recompute observes the new
// pause, and both must be atomic: a crash between them left deliveries parked
// against a pause that no longer existed.
func (s *Store) PauseFollowNotificationRule(ctx context.Context, userID, artistID int64, until *time.Time) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		rules, err := followNotificationRulesTx(ctx, tx, userID, []int64{artistID})
		if err != nil {
			return err
		}
		rule, ok := rules[artistID]
		if !ok {
			return sql.ErrNoRows
		}
		rule.PausedUntil = until
		if err := upsertFollowNotificationRulesTx(ctx, tx, userID, []int64{artistID}, rule, now); err != nil {
			return err
		}
		return realignQueuedDeliveriesTx(ctx, tx, userID, []int64{artistID}, now)
	})
}

// realignQueuedDeliveriesTx re-derives the governing follow for every queued
// delivery of userID that a change to any of artistIDs could affect, and
// rewrites next_attempt_at to match whatever follow now governs it.
//
// It is keyed on the release correlation, not on the follow that changed, on
// purpose: a pause or a mode change can hand governance from one follow to
// another, and the row must end up on the NEW governor's schedule. A delivery
// governed by a different, still-active follow is therefore visited and left
// exactly where that follow put it - which is precisely what pausing or
// resuming one follow used to break for every other follow on the same release.
//
// The predecessor bounded its update with next_attempt_at > target, which admits
// only the earlier direction; extending a pause therefore left its deliveries
// due at the old, earlier time. There is no such bound here.
func realignQueuedDeliveriesTx(ctx context.Context, tx *sql.Tx, userID int64, artistIDs []int64, now time.Time) error {
	if userID < 1 || len(artistIDs) == 0 {
		return nil
	}
	type queued struct {
		id        int64
		eventID   int64
		attempts  int
		nextAt    time.Time
		hasNextAt bool
	}
	var candidates []queued
	seen := make(map[int64]struct{})
	for _, artistID := range artistIDs {
		if artistID < 1 {
			continue
		}
		// ne.user_id=? is load-bearing: two members can hold deliveries deferred
		// by their own follows on the same artist id.
		if err := func() error {
			rows, err := tx.QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at
				FROM deliveries d
				WHERE d.status IN ('pending','blocked')
				  AND EXISTS (
					SELECT 1 FROM notification_events ne
					JOIN release_groups rg ON rg.id=ne.release_group_id
					WHERE ne.id=d.event_id AND ne.user_id=?
					  AND (rg.artist_id=? OR EXISTS (
						SELECT 1 FROM release_credits rc
						WHERE rc.release_group_id=rg.id AND rc.artist_id=?
					  ))
				  )`, userID, artistID, artistID)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var item queued
				var nextAt sql.NullString
				if err := rows.Scan(&item.id, &item.eventID, &item.attempts, &nextAt); err != nil {
					return err
				}
				if nextAt.Valid && strings.TrimSpace(nextAt.String) != "" {
					parsed, err := parseStoredTime(nextAt.String, "delivery next attempt")
					if err != nil {
						return err
					}
					item.nextAt, item.hasNextAt = parsed, true
				}
				if _, dup := seen[item.id]; dup {
					continue
				}
				seen[item.id] = struct{}{}
				candidates = append(candidates, item)
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	eventIDs := make([]int64, 0, len(candidates))
	for _, item := range candidates {
		eventIDs = append(eventIDs, item.eventID)
	}
	governing, err := governingFollows(ctx, tx, eventIDs, now)
	if err != nil {
		return err
	}
	// Bucket by target instant so each distinct value is one statement.
	buckets := make(map[string][]int64)
	for _, item := range candidates {
		g, ok := governing[item.eventID]
		if !ok {
			// No follow governs this delivery any more - metadata drift, a
			// tightened preference, or an unfollow. Leave it exactly where it
			// is: whether the member may still be sent it at all is a different
			// question, and only the coarse visibility predicate may answer it.
			continue
		}
		switch {
		case g.Deferred && item.attempts == 0:
			// The pause decides, in both directions. This is the extend case.
			buckets[timeText(g.DeferTo)] = append(buckets[timeText(g.DeferTo)], item.id)
		case g.Deferred && item.attempts > 0:
			// Honour the pause without ever pulling a retry backoff earlier.
			target := g.DeferTo
			if item.hasNextAt && item.nextAt.After(target) {
				target = item.nextAt
			}
			buckets[timeText(target)] = append(buckets[timeText(target)], item.id)
		case !g.Deferred && item.attempts == 0:
			// No pause governs it; make it due, but never push it later.
			if item.hasNextAt && item.nextAt.After(now) {
				buckets[timeText(now)] = append(buckets[timeText(now)], item.id)
			}
		default:
			// An ordinary retry backoff that no pause created. Resuming an
			// unrelated follow used to reset these to now, forcing an immediate
			// re-attempt against a destination that was already failing.
		}
	}
	for target, ids := range buckets {
		for start := 0; start < len(ids); start += governingFollowBatchSize {
			end := start + governingFollowBatchSize
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			placeholders := make([]byte, 0, len(chunk)*2)
			args := make([]any, 0, len(chunk)+1)
			args = append(args, target)
			for i, id := range chunk {
				if i > 0 {
					placeholders = append(placeholders, ',')
				}
				placeholders = append(placeholders, '?')
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET next_attempt_at=?
				WHERE id IN (`+string(placeholders)+`)`, args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func followRuleFromColumns(mode string, primary, featured, albums, eps, singles, compilations, announcements, releaseDay int, paused string, updated string, userID, artistID int64) (FollowNotificationRule, error) {
	rule := defaultFollowNotificationRule(userID, artistID, time.Now().UTC())
	rule.DeliveryMode = normalizeFollowDeliveryMode(mode)
	rule.IncludePrimary, rule.IncludeFeatured = primary != 0, featured != 0
	rule.Albums, rule.EPs, rule.Singles, rule.Compilations = albums != 0, eps != 0, singles != 0, compilations != 0
	rule.Announcements, rule.ReleaseDay = announcements != 0, releaseDay != 0
	var err error
	if strings.TrimSpace(paused) != "" {
		rule.PausedUntil, err = parseStoredNullableTime(sql.NullString{String: paused, Valid: true}, "follow notification rule paused_until")
		if err != nil {
			return rule, err
		}
	}
	if strings.TrimSpace(updated) == "" {
		return rule, nil
	}
	rule.UpdatedAt, err = parseStoredTime(updated, "follow notification rule updated_at")
	return rule, err
}
