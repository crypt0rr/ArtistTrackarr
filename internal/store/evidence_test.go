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
	// Keep the fixture relative to the clock used by SetEvidenceIssueState so
	// this validation remains stable as the calendar advances.
	now := time.Now().UTC().Truncate(time.Second)
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
	var releaseID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, base.MBID).Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	releaseIssues, err := s.EvidenceIssuesForRelease(ctx, userID, releaseID, now.Add(2*time.Minute))
	if err != nil || len(releaseIssues) != 3 {
		t.Fatalf("release-scoped issues=%#v err=%v", releaseIssues, err)
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
	// Once the snooze expires, the same issue is visible as unread again and
	// the nullable review timestamp is normalized by the scanner.
	if _, err := s.DB.ExecContext(ctx, `UPDATE release_evidence_reviews SET snoozed_until=? WHERE user_id=? AND issue_id=?`,
		now.Add(-time.Minute).Format(time.RFC3339Nano), userID, issues[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.EvidenceIssues(ctx, userID, "open", "unread", issues[0].IssueType, "", 50, 0, now)
	if err != nil || len(got) != 1 || got[0].ReviewState != "unread" {
		t.Fatalf("expired snooze issues=%#v err=%v", got, err)
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

func TestEvidenceConfirmationOnlyDrainsConfirmingMembersHold(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice, err := s.CreateUser(ctx, "evidence-alice@example.com", "hash", "member", "UTC", "evidence-alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "evidence-bob@example.com", "hash", "member", "UTC", "evidence-bob")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "evidence-shared-artist", Name: "Evidence Shared Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, alice, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, bob, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "evidence-shared-release", artist.ID, "Evidence Shared Release", "Album", "[]", "2099-01-01", 3,
		"https://musicbrainz.org/release-group/evidence-shared-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,evidence_json,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, releaseID, "date_conflict", "warning", "evidence-shared-fingerprint", "resolved conflict", "[]", "resolved", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	issueID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{alice, bob} {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_holds
			(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, userID, releaseID, "announcement", "Shared release", "Body", "review", "evidence-shared-fingerprint", nowText(), "held", nowText()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetEvidenceIssueState(ctx, alice, issueID, "confirmed", nil); err != nil {
		t.Fatal(err)
	}
	var aliceStatus, bobStatus string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE user_id=? AND release_group_id=?`, alice, releaseID).Scan(&aliceStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE user_id=? AND release_group_id=?`, bob, releaseID).Scan(&bobStatus); err != nil {
		t.Fatal(err)
	}
	if aliceStatus != "released" || bobStatus != "held" {
		t.Fatalf("hold statuses alice=%q bob=%q", aliceStatus, bobStatus)
	}
	assertEventCount(t, s, alice, "announcement", 1)
	assertEventCount(t, s, bob, "announcement", 0)
}

func ptrTime(value time.Time) *time.Time { return &value }
