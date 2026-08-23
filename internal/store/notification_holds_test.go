package store

import (
	"context"
	"testing"
	"time"
)

// TestNotifyAnywayKeepsTheHoldWhenNothingWillBeDelivered covers the override on
// a follow whose own rules admit nothing. "Notify anyway" bypasses the conflict
// hold, not the member's delivery rules, so a disabled follow still delivers
// nothing. The handler used to test for the notification_events row, which is
// written before admission is decided and therefore always present: the hold
// was marked released, nothing was sent, and the unique constraint on
// (user, release, event type) made the event unrepeatable.
func TestNotifyAnywayKeepsTheHoldWhenNothingWillBeDelivered(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "notify-anyway@example.com", "hash", "member", "UTC", "notify-anyway")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "notify-anyway-artist", Name: "Notify Anyway Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.DeliveryMode = FollowDeliveryOff
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "notify-anyway-release", artist.ID, "Notify Anyway Release",
		"Album", "[]", now.Format("2006-01-02"), 3,
		"https://musicbrainz.org/release-group/notify-anyway-release", "musicbrainz",
		timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	var releaseID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, "notify-anyway-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at)
		VALUES(?,?,?,?,?,?,?,?, 'held',?)`, userID, releaseID, "announcement", "Held title", "Held body",
		"Providers disagree", "notify-anyway-fingerprint", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	var holdID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM notification_holds WHERE user_id=?`, userID).Scan(&holdID); err != nil {
		t.Fatal(err)
	}

	if err := s.ResolveNotificationHold(ctx, userID, holdID, "notify"); err != nil {
		t.Fatal(err)
	}

	var deliveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries d
		JOIN notification_events ne ON ne.id=d.event_id WHERE ne.user_id=?`, userID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("a disabled follow queued %d deliveries", deliveries)
	}
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, holdID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "held" {
		t.Fatalf("hold status=%q after an override that delivered nothing, want held so the owner can retry", status)
	}
}
