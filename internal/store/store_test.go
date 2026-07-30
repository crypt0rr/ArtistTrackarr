package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReleaseBaselineAndExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	initial := []Release{
		{MBID: "old", Title: "Back Catalogue", PrimaryType: "Album", FirstReleaseDate: "2001-01-01", DatePrecision: 3},
		{MBID: "future", Title: "Tomorrow", PrimaryType: "EP", FirstReleaseDate: "2026-08-30", DatePrecision: 3},
	}
	if err := s.ApplyReleaseSync(ctx, artist, initial, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	if err := s.ApplyReleaseSync(ctx, artist, initial, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	newRelease := append(initial, Release{
		MBID: "new", Title: "Just Released", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-29", DatePrecision: 3,
	})
	if err := s.ApplyReleaseSync(ctx, artist, newRelease, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
}

func TestInitialSyncChoosesNearestUpcomingRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	if err := s.AddDestination(ctx, userID, "Phone", "ntfy", []byte("encrypted-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Living room", "gotify", []byte("encrypted-two")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	releases := []Release{
		{MBID: "old", Title: "Last Year", PrimaryType: "Album", FirstReleaseDate: "2025-01-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.test/old"},
		{MBID: "far", Title: "Far Future", PrimaryType: "Album", FirstReleaseDate: "2027", DatePrecision: 1, MusicBrainzURL: "https://musicbrainz.test/far"},
		{MBID: "near", Title: "Next Month", PrimaryType: "EP", FirstReleaseDate: "2026-08", DatePrecision: 2, MusicBrainzURL: "https://musicbrainz.test/near"},
	}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	var title, body, releaseMBID string
	if err := s.DB.QueryRow(`SELECT e.title,e.body,rg.mbid FROM notification_events e
		JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&title, &body, &releaseMBID); err != nil {
		t.Fatal(err)
	}
	if title != "Upcoming release from Example" || releaseMBID != "near" ||
		!strings.Contains(body, "2026-08") || !strings.Contains(body, "https://musicbrainz.test/near") {
		t.Fatalf("unexpected initial notification: title=%q body=%q release=%q", title, body, releaseMBID)
	}
	var deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want 2", deliveries)
	}
}

func TestInitialSyncChoosesLatestPastAndSkipsUndated(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	releases := []Release{
		{MBID: "undated", Title: "Mystery", PrimaryType: "Album"},
		{MBID: "older", Title: "Older", PrimaryType: "Album", FirstReleaseDate: "2025", DatePrecision: 1},
		{MBID: "latest", Title: "Latest", PrimaryType: "EP", FirstReleaseDate: "2026-06", DatePrecision: 2},
	}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	var releaseMBID, title string
	if err := s.DB.QueryRow(`SELECT rg.mbid,e.title FROM notification_events e
		JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&releaseMBID, &title); err != nil {
		t.Fatal(err)
	}
	if releaseMBID != "latest" || title != "Latest release from Example" {
		t.Fatalf("selected %q with title %q", releaseMBID, title)
	}
}

func TestInitialSyncWithOnlyUndatedReleasesCreatesNoEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{MBID: "undated", Title: "Mystery", PrimaryType: "Album"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "listener@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	raw, csrf, err := s.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Session(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if session.User.ID != userID || session.CSRFToken != csrf {
		t.Fatalf("unexpected session: %#v", session)
	}
}

func TestReleaseDayUsesUserTimezoneAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "Europe/Amsterdam")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 7, 1, 0, 0, time.UTC) // 09:01 in Amsterdam.
	releases := []Release{{
		MBID: "today", Title: "Today", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-30", DatePrecision: 3,
	}}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "release_day", 1)
}

func TestRenameDestinationIsOwnerScopedAndPreservesCredentials(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ownerID, _ := s.CreateUser(ctx, "owner@example.com", "unused", "member", "UTC")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
	encrypted := []byte("encrypted destination URL")
	if err := s.AddDestination(ctx, ownerID, "  Phone  ", "ntfy", encrypted); err != nil {
		t.Fatal(err)
	}
	destinations, _ := s.Destinations(ctx, ownerID)
	if len(destinations) != 1 || destinations[0].Name != "Phone" {
		t.Fatalf("unexpected destination: %#v", destinations)
	}
	id := destinations[0].ID
	if err := s.RenameDestination(ctx, otherID, id, "Stolen"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user rename error = %v", err)
	}
	if err := s.RenameDestination(ctx, ownerID, id, "  My phone  "); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.Destination(ctx, ownerID, id)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "My phone" || !bytes.Equal(renamed.EncryptedURL, encrypted) || renamed.Service != "ntfy" {
		t.Fatalf("rename changed protected fields: %#v", renamed)
	}
	for _, name := range []string{"   ", strings.Repeat("é", 81)} {
		if err := s.RenameDestination(ctx, ownerID, id, name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
}

func assertEventCount(t *testing.T, s *Store, userID int64, eventType string, want int) {
	t.Helper()
	var got int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=? AND event_type=?`,
		userID, eventType).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s events = %d, want %d", eventType, got, want)
	}
}
