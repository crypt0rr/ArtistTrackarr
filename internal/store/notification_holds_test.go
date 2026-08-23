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

// seedCreditedPauseScenario builds the shape that #220 turned on: the release
// group belongs to an artist the member does NOT follow, and the member's only
// route to it is a follow on a credited artist, which they have paused.
func seedCreditedPauseScenario(t *testing.T, s *Store) (userID, guestArtistID, releaseID int64, resume time.Time) {
	t.Helper()
	ctx := context.Background()
	var err error
	userID, err = s.CreateUser(ctx, "credited-pause@example.com", "hash", "member", "UTC", "credited-pause")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := s.UpsertArtist(ctx, Artist{MBID: "canonical-artist", Name: "Canonical Artist"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.UpsertArtist(ctx, Artist{MBID: "guest-artist", Name: "Guest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	guestArtistID = guest.ID
	// The member follows only the guest artist, never the canonical one.
	if _, err := s.Follow(ctx, userID, guest.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "credited-release", canonical.ID, "Credited Release", "Album", "[]",
		now.Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/credited-release",
		"musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, "credited-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_credits
		(release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,provider_url,confidence,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, releaseID, guest.ID, "musicbrainz", "rec-1", "guest", "Track",
		"Guest Artist", "https://musicbrainz.org/recording/rec-1", "confirmed", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	resume = now.Add(7 * 24 * time.Hour)
	rule, err := s.FollowNotificationRule(ctx, userID, guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.PausedUntil = &resume
	if err := s.UpdateFollowNotificationRule(ctx, userID, guest.ID, rule); err != nil {
		t.Fatal(err)
	}
	// A delivery deferred to the pause expiry, as the admission path writes it.
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Credited Release", "body", timeText(now)); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM notification_events WHERE user_id=?`, userID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "dest", "discord", []byte("x")); err != nil {
		t.Fatal(err)
	}
	var destID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM destinations WHERE user_id=?`, userID).Scan(&destID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries
		(event_id,destination_id,status,attempts,next_attempt_at) VALUES(?,?, 'pending',0,?)`,
		eventID, destID, timeText(resume)); err != nil {
		t.Fatal(err)
	}
	return userID, guestArtistID, releaseID, resume
}

func TestPauseOnACreditedArtistIsNotTreatedAsClockSkew(t *testing.T) {
	// The exclusion that keeps a deliberate pause out of clock-skew handling
	// used to correlate on the release's canonical artist only. Admission
	// accepts a follow through release_credits too, so a member whose only route
	// to a release is a paused credited follow had that pause classified as
	// skew: the instance went degraded, and the repair button - surfaced by this
	// very row - delivered the notification while the UI still said "Paused".
	ctx := context.Background()
	s := testStore(t)
	_, _, _, resume := seedCreditedPauseScenario(t, s)
	now := time.Now().UTC()

	snapshot, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FutureDeliveries != 0 {
		t.Fatalf("a deliberately paused credited follow counted as clock skew: FutureDeliveries=%d", snapshot.FutureDeliveries)
	}

	stats, err := s.RepairClockSkewedDeliveries(ctx, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deliveries != 0 {
		t.Fatalf("clock-skew repair cancelled %d paused credited-follow deliveries", stats.Deliveries)
	}
	var next string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != timeText(resume) {
		t.Fatalf("next_attempt_at=%q, want the pause expiry %q", next, timeText(resume))
	}
}

func TestLiftingAPauseBringsDeferredDeliveriesForward(t *testing.T) {
	// Pausing defers a delivery to the pause expiry, so lifting the pause early
	// must move it. Otherwise the follow shows as active while its notifications
	// stay parked until the original expiry.
	ctx := context.Background()
	s := testStore(t)
	userID, guestArtistID, _, resume := seedCreditedPauseScenario(t, s)

	var before string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != timeText(resume) {
		t.Fatalf("precondition: delivery is not deferred (next=%q)", before)
	}

	if err := s.PauseFollowNotificationRule(ctx, userID, guestArtistID, nil); err != nil {
		t.Fatal(err)
	}

	var after string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("lifting the pause left the delivery parked at the original expiry %q", after)
	}
	parsed, err := time.Parse("2006-01-02 15:04:05", after)
	if err != nil {
		if parsed, err = time.Parse(time.RFC3339, after); err != nil {
			t.Fatalf("unparseable next_attempt_at %q: %v", after, err)
		}
	}
	if parsed.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("next_attempt_at=%q is still in the future after the pause was lifted", after)
	}
}
