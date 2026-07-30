package store

import (
	"context"
	"path/filepath"
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
