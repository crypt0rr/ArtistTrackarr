package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReleaseTimelineIsOwnerScopedAndExplainsDecisions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "timeline-owner@example.com", "hash", "member", "UTC", "timeline-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "timeline-other@example.com", "hash", "member", "UTC", "timeline-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "timeline-artist", Name: "Timeline Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, owner, artist.ID); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.August, 11, 8, 0, 0, 0, time.UTC)
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_id,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "timeline-release", artist.ID, "Timeline Release", "Album", "[]",
		"2026-08-12", 3, "https://musicbrainz.org/release-group/timeline-release", "timeline-spotify",
		"https://open.spotify.com/album/timeline-release", "spotify", timeText(base), timeText(base))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES
		('spotify','timeline-spotify',?,'hash',?)`, releaseID, timeText(base)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_credits
		(release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,provider_url,confidence,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, releaseID, artist.ID, "spotify", "timeline-track", "featured",
		"Timeline Track", "Timeline Artist", "https://open.spotify.com/track/timeline-track", "confirmed",
		timeText(base), timeText(base.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,evidence_json,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, releaseID, "date_conflict", "warning", "timeline-fingerprint",
		"Providers disagree on the date", "[]", "open", timeText(base), timeText(base.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, owner, releaseID,
		"announcement", "Timeline Release announced", "secret notification body", timeText(base.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, owner, "Timeline destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, owner)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destinationID := destinations[0].ID
	var eventID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM notification_events WHERE user_id=? AND release_group_id=?`, owner, releaseID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries
		(event_id,destination_id,status,attempts,next_attempt_at,last_error,sent_at) VALUES(?,?,?,?,?,?,?)`, eventID,
		destinationID, "sent", 1, timeText(base.Add(time.Hour)), "", timeText(base.Add(4*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, owner, releaseID, "announcement", "Timeline Release announced", "secret hold body",
		"Providers disagree on the date", "timeline-fingerprint", timeText(base), "held", timeText(base.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE follow_notification_rules SET delivery_mode='off',updated_at=? WHERE user_id=? AND artist_id=?`, timeText(base.Add(5*time.Minute)), owner, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO user_release_states(user_id,release_group_id,state,updated_at) VALUES(?,?,?,?)`, owner, releaseID, "read", timeText(base.Add(6*time.Minute))); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ReleaseTimeline(ctx, owner, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 7 {
		t.Fatalf("timeline entries=%d, want at least 7: %#v", len(entries), entries)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		seen[entry.Kind] = true
		if strings.Contains(entry.Summary, "secret") {
			t.Fatalf("timeline leaked sensitive message: %#v", entry)
		}
	}
	for _, kind := range []string{"observation", "credit", "evidence", "notification", "hold", "rule", "inbox"} {
		if !seen[kind] {
			t.Fatalf("timeline missing %q: %#v", kind, entries)
		}
	}
	if entries[0].Kind != "inbox" || entries[0].Status != "read" {
		t.Fatalf("timeline is not newest first: %#v", entries[:2])
	}
	if !strings.Contains(entries[1].Summary, "Notifications turned off") {
		t.Fatalf("timeline rule summary=%q", entries[1].Summary)
	}

	if _, err := s.ReleaseTimeline(ctx, other, releaseID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user timeline error=%v, want sql.ErrNoRows", err)
	}
	if _, err := s.ReleaseTimeline(ctx, owner, 999999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing timeline error=%v, want sql.ErrNoRows", err)
	}
}

func TestReleaseTimelineClosesRowsAfterMalformedTimestamp(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "timeline-malformed@example.com", "hash", "member", "UTC", "timeline-malformed")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "timeline-malformed-artist", Name: "Timeline Malformed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "timeline-malformed-release", artist.ID, "Timeline Malformed Release", "Album", "[]",
		"2026-08-12", 3, "https://musicbrainz.org/release-group/timeline-malformed-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES(?,?,?,?,?)`, "musicbrainz", "timeline-malformed-observation", releaseID, "hash", "not-a-timestamp"); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 8; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := s.ReleaseTimeline(callCtx, userID, releaseID)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "invalid persisted release timeline observation") {
			t.Fatalf("attempt %d timeline error=%v", attempt, err)
		}
	}
}
