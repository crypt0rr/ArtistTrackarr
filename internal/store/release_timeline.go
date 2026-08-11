package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ReleaseTimeline returns a redacted explanation of the provider observations,
// credits, reviews, notification decisions, and delivery state for one release
// visible to the requesting household member. It is derived from existing
// projections so it cannot alter release or notification semantics.
func (s *Store) ReleaseTimeline(ctx context.Context, userID, releaseID int64) ([]ReleaseTimelineEntry, error) {
	var exists int
	err := s.readerDB().QueryRowContext(ctx, `SELECT 1
		FROM release_groups rg
		JOIN follows f ON f.artist_id=rg.artist_id
		WHERE rg.id=? AND f.user_id=? LIMIT 1`, releaseID, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	entries := make([]ReleaseTimelineEntry, 0, 16)
	appendEntry := func(entry ReleaseTimelineEntry) {
		if entry.OccurredAt.IsZero() {
			return
		}
		entries = append(entries, entry)
	}

	observations, err := s.readerDB().QueryContext(ctx, `SELECT provider,observed_at
		FROM provider_observations WHERE release_group_id=?
		ORDER BY observed_at DESC,provider`, releaseID)
	if err != nil {
		return nil, err
	}
	for observations.Next() {
		var provider, observedAt string
		if err := observations.Scan(&provider, &observedAt); err != nil {
			_ = observations.Close()
			return nil, err
		}
		when, parseErr := parseTime(observedAt)
		if parseErr != nil {
			continue
		}
		appendEntry(ReleaseTimelineEntry{
			Kind:       "observation",
			Provider:   provider,
			Status:     "observed",
			Summary:    "Release metadata observed",
			OccurredAt: when,
		})
	}
	if err := observations.Err(); err != nil {
		_ = observations.Close()
		return nil, err
	}
	_ = observations.Close()

	credits, err := s.readerDB().QueryContext(ctx, `SELECT rc.provider,rc.role,rc.track_title,
		rc.credit_name,rc.last_seen_at
		FROM release_credits rc
		JOIN follows f ON f.artist_id=rc.artist_id
		WHERE f.user_id=? AND rc.release_group_id=?
		ORDER BY rc.last_seen_at DESC,rc.provider,rc.role,rc.track_title`, userID, releaseID)
	if err != nil {
		return nil, err
	}
	for credits.Next() {
		var provider, role, trackTitle, creditName, lastSeen string
		if err := credits.Scan(&provider, &role, &trackTitle, &creditName, &lastSeen); err != nil {
			_ = credits.Close()
			return nil, err
		}
		when, parseErr := parseTime(lastSeen)
		if parseErr != nil {
			continue
		}
		summary := creditTimelineSummary(role, trackTitle, creditName)
		appendEntry(ReleaseTimelineEntry{
			Kind:       "credit",
			Provider:   provider,
			Role:       role,
			Status:     "active",
			Summary:    summary,
			OccurredAt: when,
		})
	}
	if err := credits.Err(); err != nil {
		_ = credits.Close()
		return nil, err
	}
	_ = credits.Close()

	issues, err := s.readerDB().QueryContext(ctx, `SELECT i.issue_type,i.severity,i.summary,
		i.status,i.last_seen_at,COALESCE(r.state,'')
		FROM release_evidence_issues i
		LEFT JOIN release_evidence_reviews r ON r.issue_id=i.id AND r.user_id=?
		WHERE i.release_group_id=?
		ORDER BY i.last_seen_at DESC,i.id DESC`, userID, releaseID)
	if err != nil {
		return nil, err
	}
	for issues.Next() {
		var issueType, severity, summary, status, lastSeen, reviewState string
		if err := issues.Scan(&issueType, &severity, &summary, &status, &lastSeen, &reviewState); err != nil {
			_ = issues.Close()
			return nil, err
		}
		when, parseErr := parseTime(lastSeen)
		if parseErr != nil {
			continue
		}
		if reviewState != "" {
			status = reviewState
		}
		appendEntry(ReleaseTimelineEntry{
			Kind:       "evidence",
			Status:     strings.TrimSpace(severity + " · " + status),
			Summary:    strings.TrimSpace(issueTypeLabel(issueType) + ": " + summary),
			OccurredAt: when,
		})
	}
	if err := issues.Err(); err != nil {
		_ = issues.Close()
		return nil, err
	}
	_ = issues.Close()

	var provider, reason, updatedAt string
	var decisionExists int
	err = s.readerDB().QueryRowContext(ctx, `SELECT selected_provider,reason,updated_at
		FROM release_truth_decisions WHERE release_group_id=?`, releaseID).Scan(&provider, &reason, &updatedAt)
	if err == nil {
		decisionExists = 1
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if decisionExists == 1 {
		when, parseErr := parseTime(updatedAt)
		if parseErr == nil {
			summary := "Household confirmed " + providerLabel(provider) + " as the preferred source"
			if strings.TrimSpace(reason) != "" {
				summary += ": " + strings.TrimSpace(reason)
			}
			appendEntry(ReleaseTimelineEntry{
				Kind:       "decision",
				Provider:   provider,
				Status:     "confirmed",
				Summary:    summary,
				OccurredAt: when,
			})
		}
	}

	notifications, err := s.readerDB().QueryContext(ctx, `SELECT e.event_type,e.title,e.created_at,
		COUNT(d.id),COALESCE(SUM(CASE WHEN d.status='sent' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.status='pending' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.status='failed' THEN 1 ELSE 0 END),0)
		FROM notification_events e
		LEFT JOIN deliveries d ON d.event_id=e.id
		WHERE e.user_id=? AND e.release_group_id=?
		GROUP BY e.id,e.event_type,e.title,e.created_at
		ORDER BY e.created_at DESC,e.id DESC`, userID, releaseID)
	if err != nil {
		return nil, err
	}
	for notifications.Next() {
		var eventType, title, createdAt string
		var destinations, sent, pending, failed int
		if err := notifications.Scan(&eventType, &title, &createdAt, &destinations, &sent, &pending, &failed); err != nil {
			_ = notifications.Close()
			return nil, err
		}
		when, parseErr := parseTime(createdAt)
		if parseErr != nil {
			continue
		}
		status := "queued"
		switch {
		case destinations == 0:
			status = "recorded"
		case sent == destinations:
			status = "sent"
		case failed > 0 && pending == 0:
			status = "failed"
		case pending > 0:
			status = "pending"
		}
		summary := notificationEventSummary(eventType, title, destinations, sent, pending, failed)
		appendEntry(ReleaseTimelineEntry{
			Kind:       "notification",
			Status:     status,
			Summary:    summary,
			OccurredAt: when,
		})
	}
	if err := notifications.Err(); err != nil {
		_ = notifications.Close()
		return nil, err
	}
	_ = notifications.Close()

	holds, err := s.readerDB().QueryContext(ctx, `SELECT event_type,reason,status,created_at,
		COALESCE(released_at,'')
		FROM notification_holds WHERE user_id=? AND release_group_id=?
		ORDER BY created_at DESC,id DESC`, userID, releaseID)
	if err != nil {
		return nil, err
	}
	for holds.Next() {
		var eventType, reason, status, createdAt, releasedAt string
		if err := holds.Scan(&eventType, &reason, &status, &createdAt, &releasedAt); err != nil {
			_ = holds.Close()
			return nil, err
		}
		when, parseErr := parseTime(createdAt)
		if parseErr != nil {
			continue
		}
		summary := notificationEventTypeLabel(eventType) + " held: " + strings.TrimSpace(reason)
		if status == "released" {
			summary = notificationEventTypeLabel(eventType) + " hold released"
			if released, releaseErr := parseTime(releasedAt); releaseErr == nil {
				when = released
			}
		} else if status == "discarded" {
			summary = notificationEventTypeLabel(eventType) + " hold discarded"
		}
		appendEntry(ReleaseTimelineEntry{
			Kind:       "hold",
			Status:     status,
			Summary:    summary,
			OccurredAt: when,
		})
	}
	if err := holds.Err(); err != nil {
		_ = holds.Close()
		return nil, err
	}
	_ = holds.Close()

	var ruleMode string
	var includePrimary, includeFeatured, albums, eps, singles, compilations, announcements, releaseDay int
	var pausedUntil, ruleUpdated string
	err = s.readerDB().QueryRowContext(ctx, `SELECT delivery_mode,include_primary,include_featured,
		albums,eps,singles,compilations,announcements,release_day,COALESCE(paused_until,''),updated_at
		FROM follow_notification_rules WHERE user_id=? AND artist_id=(SELECT artist_id FROM release_groups WHERE id=?)`, userID, releaseID).
		Scan(&ruleMode, &includePrimary, &includeFeatured, &albums, &eps, &singles, &compilations, &announcements, &releaseDay, &pausedUntil, &ruleUpdated)
	if err == nil {
		when, parseErr := parseTime(ruleUpdated)
		if parseErr == nil {
			if summary := notificationRuleSummary(ruleMode, includePrimary, includeFeatured, albums, eps, singles, compilations, announcements, releaseDay, pausedUntil); summary != "" {
				appendEntry(ReleaseTimelineEntry{Kind: "rule", Status: "active", Summary: summary, OccurredAt: when})
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var inboxState, inboxUpdated string
	err = s.readerDB().QueryRowContext(ctx, `SELECT state,updated_at FROM user_release_states
		WHERE user_id=? AND release_group_id=?`, userID, releaseID).Scan(&inboxState, &inboxUpdated)
	if err == nil {
		when, parseErr := parseTime(inboxUpdated)
		if parseErr == nil {
			appendEntry(ReleaseTimelineEntry{Kind: "inbox", Status: inboxState, Summary: "Inbox marked " + inboxState, OccurredAt: when})
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].OccurredAt.Equal(entries[j].OccurredAt) {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].OccurredAt.After(entries[j].OccurredAt)
	})
	return entries, nil
}

func creditTimelineSummary(role, trackTitle, creditName string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	label := "Artist credit recorded"
	switch role {
	case "featured":
		label = "Featured appearance recorded"
	case "guest":
		label = "Guest credit recorded"
	case "primary":
		label = "Primary artist credit recorded"
	}
	trackTitle = strings.TrimSpace(trackTitle)
	creditName = strings.TrimSpace(creditName)
	if trackTitle != "" {
		return label + " on “" + trackTitle + "”"
	}
	if creditName != "" {
		return label + " as “" + creditName + "”"
	}
	return label
}

func issueTypeLabel(issueType string) string {
	switch strings.ToLower(strings.TrimSpace(issueType)) {
	case "date_conflict":
		return "Date conflict"
	case "title_conflict":
		return "Title conflict"
	case "type_conflict":
		return "Type conflict"
	case "missing_canonical":
		return "Canonical source missing"
	default:
		return "Provider evidence"
	}
}

func notificationEventTypeLabel(eventType string) string {
	if strings.EqualFold(strings.TrimSpace(eventType), "release_day") {
		return "Release-day reminder"
	}
	return "Release announcement"
}

func notificationEventSummary(eventType, title string, destinations, sent, pending, failed int) string {
	label := notificationEventTypeLabel(eventType)
	if strings.TrimSpace(title) == "" {
		return fmt.Sprintf("%s recorded (%d destinations)", label, destinations)
	}
	status := fmt.Sprintf("%d destinations", destinations)
	switch {
	case destinations == 0:
		status = "no active destinations"
	case sent == destinations:
		status = fmt.Sprintf("sent to %d destinations", sent)
	case failed > 0 && pending == 0:
		status = fmt.Sprintf("%d failed, %d sent", failed, sent)
	case pending > 0:
		status = fmt.Sprintf("%d pending, %d sent", pending, sent)
	}
	return label + ": “" + strings.TrimSpace(title) + "” · " + status
}

func notificationRuleSummary(mode string, includePrimary, includeFeatured, albums, eps, singles, compilations, announcements, releaseDay int, pausedUntil string) string {
	parts := make([]string, 0, 3)
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off":
		parts = append(parts, "Notifications turned off for this follow")
	case "digest":
		parts = append(parts, "Notifications delivered through the digest")
	case "immediate":
		parts = append(parts, "Notifications delivered immediately")
	}
	if includePrimary == 0 {
		parts = append(parts, "Primary credits excluded")
	}
	if includeFeatured == 0 {
		parts = append(parts, "Featured and guest credits excluded")
	}
	if albums == 0 || eps == 0 || singles == 0 || compilations == 0 || announcements == 0 || releaseDay == 0 {
		parts = append(parts, "Some release types or notification moments are filtered")
	}
	if strings.TrimSpace(pausedUntil) != "" {
		parts = append(parts, "Follow is temporarily paused")
	}
	return strings.Join(parts, " · ")
}
