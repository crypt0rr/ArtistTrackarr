package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReleaseEvidenceDetectsConflictsAndOwnerReview(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "evidence@example.com", "hash", "member", "UTC", "evidence")
	if err != nil {
		t.Fatal(err)
	}
	otherUser, err := s.CreateUser(ctx, "other-evidence@example.com", "hash", "member", "UTC", "other-evidence")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "evidence-artist", Name: "Evidence Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	base := Release{MBID: "evidence-release", Title: "Truth", PrimaryType: "Album", FirstReleaseDate: "2026-09-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/evidence-release"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "musicbrainz", Releases: []Release{base}}}, now); err != nil {
		t.Fatal(err)
	}
	spotify := base
	spotify.SpotifyID = "spotify-evidence"
	spotify.SpotifyURL = "https://open.spotify.com/album/spotify-evidence"
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{spotify}}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	changed := spotify
	changed.Title = "Truth Reframed"
	changed.PrimaryType = "EP"
	changed.FirstReleaseDate = "2026-09-08"
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{changed}}}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	issues, err := s.EvidenceIssues(ctx, userID, "open", "unread", "", "", 50, 0, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 3 {
		t.Fatalf("issues=%#v, want date/title/type conflicts", issues)
	}
	seen := map[string]bool{}
	for _, issue := range issues {
		seen[issue.IssueType] = true
		if len(issue.Evidence) != 2 || issue.Summary == "" {
			t.Fatalf("issue evidence=%#v", issue)
		}
	}
	for _, issueType := range []string{"date_conflict", "title_conflict", "type_conflict"} {
		if !seen[issueType] {
			t.Fatalf("missing issue type %q: %#v", issueType, seen)
		}
	}
	count, err := s.EvidenceIssueUnreadCount(ctx, userID, now.Add(2*time.Minute))
	if err != nil || count != 3 {
		t.Fatalf("unread count=%d err=%v", count, err)
	}
	if err := s.SetEvidenceIssueState(ctx, userID, issues[0].ID, "confirmed", nil); err != nil {
		t.Fatal(err)
	}
	count, err = s.EvidenceIssueUnreadCount(ctx, userID, now.Add(2*time.Minute))
	if err != nil || count != 2 {
		t.Fatalf("after confirm count=%d err=%v", count, err)
	}
	if err := s.SetEvidenceIssueState(ctx, userID, issues[0].ID, "unread", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEvidenceIssueState(ctx, otherUser, issues[0].ID, "dismissed", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user state error=%v, want sql.ErrNoRows", err)
	}
	if err := s.SetEvidenceIssueState(ctx, userID, issues[0].ID, "snoozed", ptrTime(now.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EvidenceIssues(ctx, userID, "open", "snoozed", "", "", 50, 0, now); err != nil || len(got) != 1 {
		t.Fatalf("snoozed issues=%#v err=%v", got, err)
	}
}

func TestReleaseEvidenceMatchingProvidersStayClean(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "clean-evidence@example.com", "hash", "member", "UTC", "clean-evidence")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "clean-evidence-artist", Name: "Clean Evidence Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	release := Release{MBID: "clean-evidence-release", Title: "Same", PrimaryType: "Album", FirstReleaseDate: "2026-09-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/clean-evidence-release", SpotifyID: "spotify-clean", SpotifyURL: "https://open.spotify.com/album/spotify-clean"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "musicbrainz", Releases: []Release{release}}, {Provider: "spotify", Releases: []Release{release}}}, now); err != nil {
		t.Fatal(err)
	}
	issues, err := s.EvidenceIssues(ctx, userID, "open", "all", "", "", 50, 0, now)
	if err != nil || len(issues) != 0 {
		t.Fatalf("clean issues=%#v err=%v", issues, err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
