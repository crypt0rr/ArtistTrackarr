package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so the recompute can run
// on a reader for diagnostics and inside the writer transaction that is about to
// act on its answer.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// followCandidate is one follow that could govern a release's delivery.
type followCandidate struct {
	rule FollowNotificationRule
	role string
}

// governingFollow answers "which follow decides this delivery's schedule".
//
// Found is false when the member currently has no follow that admission would
// select: metadata drift, an unfollow, a tightened account preference, or a
// changed type filter. Callers MUST treat that as "not deferred" and MUST NEVER
// treat it as grounds to discard a delivery. Whether the member may still be
// sent this release at all is a different question, answered by the coarse
// followedReleasePredicate; only that predicate may drive a delete.
type governingFollow struct {
	Found    bool
	Rule     FollowNotificationRule
	Role     string
	Deferred bool
	DeferTo  time.Time
}

// selectGoverningCandidate is the ranking admission has always used, lifted so
// there is exactly one implementation of it.
//
// candidates MUST arrive in admission's ORDER BY order - canonical artist
// first, then artist_id - because equal priorities keep the first entry, and
// that tie-break is itself a prior fix: a paused immediate follow must beat a
// disabled follow on the canonical artist, or the deferral is evaluated against
// the wrong rule and dropped while the event row is still written and has
// permanently consumed its uniqueness slot.
func selectGoverningCandidate(candidates []followCandidate, p NotificationPreferences,
	primaryType string, secondaryTypes []string, eventType string, now time.Time) *followCandidate {
	var selected *followCandidate
	selectedPriority := -1
	for index := range candidates {
		item := &candidates[index]
		if !item.rule.AllowsRelease(primaryType, secondaryTypes, item.role, eventType, now) ||
			!item.rule.allowsAccountNotificationMoment(p, eventType) {
			continue
		}
		// Rank on the configured mode, not the paused projection of it. A
		// paused follow reports "off", which used to tie it with a genuinely
		// disabled follow.
		priority := 1
		switch normalizeFollowDeliveryMode(item.rule.DeliveryMode) {
		case FollowDeliveryInherit, FollowDeliveryImmediate:
			priority = 3
		case FollowDeliveryDigest:
			priority = 2
		}
		if selected == nil || priority > selectedPriority {
			selected = item
			selectedPriority = priority
		}
	}
	return selected
}

// governingFollowBatchSize bounds the placeholder count of one recompute query.
const governingFollowBatchSize = 200

// governingFollows recomputes the governing follow for a set of events.
//
// Every input to the selection is persisted state and none of it depends on the
// instant admission ran: AllowsRelease ignores its now argument, and the ranking
// reads the configured delivery mode rather than its paused projection. So this
// is not a reconstruction of a past decision - it is the same function applied
// to the state that holds now, which is what the callers actually need.
//
// It queries in batches and drains each cursor completely before running the
// next statement: the store has one writer and four readers, and issuing a
// query while a cursor from the same pool is open is what exhausts that pool.
func governingFollows(ctx context.Context, q rowQuerier, eventIDs []int64, at time.Time) (map[int64]governingFollow, error) {
	result := make(map[int64]governingFollow, len(eventIDs))
	for start := 0; start < len(eventIDs); start += governingFollowBatchSize {
		end := start + governingFollowBatchSize
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		chunk := eventIDs[start:end]
		// Build placeholders and args in one loop. modernc.org/sqlite errors on
		// missing bind arguments but silently ignores surplus ones, so a count
		// mismatch is not a failure - it is a silently wrong query.
		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		// followedReleaseAssociationRole binds no arguments of its own; args is
		// exactly the event ids.
		rows, err := q.QueryContext(ctx, `SELECT ne.id,ne.user_id,ne.event_type,
			COALESCE(p.albums,1),COALESCE(p.eps,1),COALESCE(p.singles,1),
			COALESCE(p.announcements,1),COALESCE(p.release_day,1),COALESCE(p.hold_conflicting_notifications,0),
			f.artist_id,rg.primary_type,COALESCE(rg.secondary_types,'[]'),
			COALESCE(NULLIF(`+followedReleaseAssociationRole+`,''),'primary'),
			COALESCE(nr.delivery_mode,'inherit'),COALESCE(nr.include_primary,1),COALESCE(nr.include_featured,1),
			COALESCE(nr.albums,1),COALESCE(nr.eps,1),COALESCE(nr.singles,1),COALESCE(nr.compilations,1),
			COALESCE(nr.announcements,1),COALESCE(nr.release_day,1),nr.paused_until,COALESCE(nr.updated_at,'')
			FROM notification_events ne
			JOIN release_groups rg ON rg.id=ne.release_group_id
			JOIN follows f ON f.user_id=ne.user_id AND (f.artist_id=rg.artist_id OR EXISTS (
				SELECT 1 FROM release_credits owner_credit
				WHERE owner_credit.release_group_id=rg.id AND owner_credit.artist_id=f.artist_id
			))
			LEFT JOIN notification_preferences p ON p.user_id=ne.user_id
			LEFT JOIN follow_notification_rules nr ON nr.user_id=ne.user_id AND nr.artist_id=f.artist_id
			WHERE ne.id IN (`+string(placeholders)+`)
			ORDER BY ne.id,CASE WHEN f.artist_id=rg.artist_id THEN 0 ELSE 1 END,f.artist_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("load governing follow candidates: %w", err)
		}
		// Group by event, then resolve after the cursor is closed.
		type eventRows struct {
			eventType  string
			prefs      NotificationPreferences
			primary    string
			secondary  string
			candidates []followCandidate
			userID     int64
		}
		grouped := make(map[int64]*eventRows)
		order := make([]int64, 0, len(chunk))
		if err := func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var eventID, ownerID int64
				var eventType string
				var albums, eps, singles, announcements, releaseDay, holdConflicts int
				var artistID int64
				var primary, secondary, role, mode string
				var includePrimary, includeFeatured int
				var ruleAlbums, ruleEPs, ruleSingles, compilations, ruleAnnouncements, ruleReleaseDay int
				var paused, updated sql.NullString
				if err := rows.Scan(&eventID, &ownerID, &eventType, &albums, &eps, &singles, &announcements, &releaseDay,
					&holdConflicts, &artistID, &primary, &secondary, &role, &mode, &includePrimary, &includeFeatured,
					&ruleAlbums, &ruleEPs, &ruleSingles, &compilations, &ruleAnnouncements, &ruleReleaseDay,
					&paused, &updated); err != nil {
					return fmt.Errorf("scan governing follow candidate: %w", err)
				}
				entry, ok := grouped[eventID]
				if !ok {
					entry = &eventRows{eventType: eventType, primary: primary, secondary: secondary, userID: ownerID}
					entry.prefs.Albums, entry.prefs.EPs, entry.prefs.Singles = albums != 0, eps != 0, singles != 0
					entry.prefs.Announcements, entry.prefs.ReleaseDay = announcements != 0, releaseDay != 0
					entry.prefs.HoldConflictingNotifications = holdConflicts != 0
					grouped[eventID] = entry
					order = append(order, eventID)
				}
				rule, err := followRuleFromColumns(mode, includePrimary, includeFeatured, ruleAlbums, ruleEPs,
					ruleSingles, compilations, ruleAnnouncements, ruleReleaseDay, paused.String, updated.String,
					entry.userID, artistID)
				if err != nil {
					return err
				}
				entry.candidates = append(entry.candidates, followCandidate{rule: rule, role: role})
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
		for _, eventID := range order {
			entry := grouped[eventID]
			var secondaryTypes []string
			if err := json.Unmarshal([]byte(entry.secondary), &secondaryTypes); err != nil {
				return nil, fmt.Errorf("parse release secondary types: %w", err)
			}
			if !releaseTypeEnabled(entry.prefs, entry.primary, secondaryTypes) {
				continue
			}
			selected := selectGoverningCandidate(entry.candidates, entry.prefs, entry.primary, secondaryTypes, entry.eventType, at)
			if selected == nil {
				continue
			}
			resolved := governingFollow{Found: true, Rule: selected.rule, Role: selected.role}
			if resumesAt, deferred := selected.rule.pausedDeliveryResumesAt(at); deferred {
				resolved.Deferred, resolved.DeferTo = true, resumesAt
			}
			result[eventID] = resolved
		}
	}
	return result, nil
}
