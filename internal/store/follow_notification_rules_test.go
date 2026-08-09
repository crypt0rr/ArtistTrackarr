package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFollowNotificationRulesDefaultAndOwnerScope(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "rules-owner@example.com", "hash", "member", "UTC", "rules-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "rules-other@example.com", "hash", "member", "UTC", "rules-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "rules-artist", Name: "Rules Artist"})
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.Follow(ctx, owner, artist.ID)
	if err != nil || !added {
		t.Fatalf("follow added=%v err=%v", added, err)
	}
	rule, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.DeliveryMode != FollowDeliveryInherit || !rule.IncludePrimary || !rule.IncludeFeatured || !rule.Albums || !rule.Compilations {
		t.Fatalf("default rule=%#v", rule)
	}
	custom := rule
	custom.DeliveryMode = FollowDeliveryDigest
	custom.IncludeFeatured = false
	custom.Singles = false
	if err := s.UpdateFollowNotificationRule(ctx, owner, artist.ID, custom); err != nil {
		t.Fatal(err)
	}
	stored, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeliveryMode != FollowDeliveryDigest || stored.IncludeFeatured || stored.Singles {
		t.Fatalf("stored rule=%#v", stored)
	}
	if err := s.UpdateFollowNotificationRule(ctx, other, artist.ID, custom); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user update error=%v, want sql.ErrNoRows", err)
	}
	if _, err := s.SetFollowNotificationDeliveryMode(ctx, owner, []int64{artist.ID, artist.ID}, FollowDeliveryImmediate); err != nil {
		t.Fatal(err)
	}
	stored, err = s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil || stored.DeliveryMode != FollowDeliveryImmediate || stored.IncludeFeatured || stored.Singles {
		t.Fatalf("bulk mode changed unrelated fields: rule=%#v err=%v", stored, err)
	}
	until := time.Now().UTC().Add(time.Hour)
	if err := s.PauseFollowNotificationRule(ctx, owner, artist.ID, &until); err != nil {
		t.Fatal(err)
	}
	paused, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil || paused.PausedUntil == nil {
		t.Fatalf("paused rule=%#v err=%v", paused, err)
	}
	if paused.queuesImmediate(time.Now().UTC()) || paused.belongsInDigest(time.Now().UTC()) {
		t.Fatal("future pause still allowed delivery")
	}
	if err := s.PauseFollowNotificationRule(ctx, owner, artist.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Unfollow(ctx, owner, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FollowNotificationRule(ctx, owner, artist.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("follow notification rule survived unfollow: %v", err)
	}
	if err := s.Unfollow(ctx, owner, artist.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second unfollow error=%v, want sql.ErrNoRows", err)
	}
}

func TestFollowNotificationRulesFilterEventsAndQueueModes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "rules-events@example.com", "hash", "member", "UTC", "rules-events")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "rules-events-artist", Name: "Rules Events"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	baselineAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	baseline := Release{SpotifyID: "rules-old", SpotifyURL: "https://open.spotify.com/album/rules-old", Title: "Old Release", PrimaryType: "Album", FirstReleaseDate: "2020-01-01", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline}}}, baselineAt); err != nil {
		t.Fatal(err)
	}
	// Keep the onboarding event out of the delivery assertion below; the
	// follow rule applies to releases observed after the baseline.
	if err := s.AddDestination(ctx, userID, "Rules destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryDigest, IncludePrimary: true, IncludeFeatured: false,
		Albums: true, EPs: true, Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	future := Release{SpotifyID: "rules-future", SpotifyURL: "https://open.spotify.com/album/rules-future", Title: "Future Release", PrimaryType: "Album", FirstReleaseDate: "2026-09-01", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future}}}, baselineAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
	var deliveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id IN (SELECT id FROM notification_events WHERE user_id=?)`, userID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("digest-only rule queued %d immediate deliveries", deliveries)
	}
	// A featured release is suppressed while the credit filter is disabled.
	featured := Release{SpotifyID: "rules-featured", SpotifyURL: "https://open.spotify.com/album/rules-featured", Title: "Featured Release", PrimaryType: "Single", FirstReleaseDate: "2026-09-02", DatePrecision: 3, ArtistCreditRole: "featured"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured}}}, baselineAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
	// Turning the credit filter on does not change delivery mode, so the next
	// featured event remains visible in history but still belongs in a digest.
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.IncludeFeatured = true
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	featuredNext := Release{SpotifyID: "rules-featured-next", SpotifyURL: "https://open.spotify.com/album/rules-featured-next", Title: "Featured Next", PrimaryType: "Single", FirstReleaseDate: "2026-09-03", DatePrecision: 3, ArtistCreditRole: "featured"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext}}}, baselineAt.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 3)
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryOff, IncludePrimary: true, IncludeFeatured: true,
		Albums: true, EPs: true, Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	offRelease := Release{SpotifyID: "rules-off", SpotifyURL: "https://open.spotify.com/album/rules-off", Title: "Off Release", PrimaryType: "Album", FirstReleaseDate: "2026-09-04", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext, offRelease}}}, baselineAt.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 4)
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id IN (SELECT id FROM notification_events WHERE user_id=?)`, userID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("off rule queued deliveries after %d events", deliveries)
	}
	// Content and event-moment filters are independent from delivery mode.
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryImmediate, IncludePrimary: true, IncludeFeatured: true,
		Albums: true, EPs: true, Singles: false, Compilations: true, Announcements: true, ReleaseDay: false,
	}); err != nil {
		t.Fatal(err)
	}
	single := Release{SpotifyID: "rules-single-filtered", SpotifyURL: "https://open.spotify.com/album/rules-single-filtered", Title: "Filtered Single", PrimaryType: "Single", FirstReleaseDate: "2026-09-05", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext, offRelease, single}}}, baselineAt.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 4)
}
