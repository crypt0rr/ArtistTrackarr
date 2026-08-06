package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReleaseTruthDecisionIsOwnerScopedAndReversible(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "truth@example.com", "hash", "member", "UTC", "truth")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "other-truth@example.com", "hash", "member", "UTC", "othertruth")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "truth-artist", Name: "Truth Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_id,spotify_url,itunes_id,itunes_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "truth-release", artist.ID, "Truth Release", "Album", "[]",
		"2026-08-08", 3, "https://musicbrainz.org/release-group/truth-release", "spotify-release",
		"https://open.spotify.com/album/truth-release", "itunes-release",
		"https://music.apple.com/us/album/truth-release/1", "both", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES
		('spotify','spotify-release',?,'hash',?),('itunes','itunes-release',?,'hash',?)`,
		releaseID, nowText(), releaseID, nowText()); err != nil {
		t.Fatal(err)
	}
	detail, err := s.ReleaseDetail(ctx, userID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TruthState != "fallback_confirmed" {
		t.Fatalf("automatic truth state=%q, want fallback_confirmed", detail.TruthState)
	}
	if err := s.SetReleaseTruthDecision(ctx, userID, releaseID, "itunes", "Apple listing has the clearest date"); err != nil {
		t.Fatal(err)
	}
	detail, err = s.ReleaseDetail(ctx, userID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TruthState != "confirmed" || detail.TruthProvider != "itunes" || detail.TruthProviderID != "itunes-release" || detail.TruthReason == "" || detail.TruthUpdatedAt == nil {
		t.Fatalf("saved truth decision=%+v", detail.Release)
	}
	if err := s.SetReleaseTruthDecision(ctx, otherID, releaseID, "spotify", "should fail"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user decision error=%v, want sql.ErrNoRows", err)
	}
	if err := s.ClearReleaseTruthDecision(ctx, userID, releaseID); err != nil {
		t.Fatal(err)
	}
	detail, err = s.ReleaseDetail(ctx, userID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TruthState != "fallback_confirmed" || detail.TruthProvider != "" {
		t.Fatalf("cleared truth decision=%+v", detail.Release)
	}
	if err := s.ClearReleaseTruthDecision(ctx, userID, releaseID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second clear error=%v, want sql.ErrNoRows", err)
	}
}

func TestReleaseTruthDecisionRejectsUnobservedProvider(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "truth-unobserved@example.com", "hash", "member", "UTC", "truthunobserved")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "truth-unobserved-artist", Name: "Truth Unobserved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"truth-unobserved-release", artist.ID, "Truth Unobserved Release", "Album", "[]", "2026", 1,
		"https://musicbrainz.org/release-group/truth-unobserved-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if err := s.SetReleaseTruthDecision(ctx, userID, releaseID, "spotify", "missing"); !errors.Is(err, ErrReleaseTruthProviderUnavailable) {
		t.Fatalf("unobserved provider error=%v, want ErrReleaseTruthProviderUnavailable", err)
	}
	if err := s.SetReleaseTruthDecision(ctx, userID, releaseID, "invalid", "bad"); !errors.Is(err, ErrInvalidReleaseTruthProvider) {
		t.Fatalf("invalid provider error=%v, want ErrInvalidReleaseTruthProvider", err)
	}
}

func TestReleaseTruthState(t *testing.T) {
	cases := []struct {
		name, want, explicit, source string
		count, issues                int
		sources                      []string
	}{
		{name: "explicit", want: "confirmed", explicit: "confirmed", source: "spotify"},
		{name: "warning", want: "needs_review", issues: 1, count: 2, sources: []string{"itunes", "spotify"}},
		{name: "canonical agreement", want: "verified", count: 2, sources: []string{"musicbrainz", "spotify"}},
		{name: "fallback agreement", want: "fallback_confirmed", count: 2, sources: []string{"itunes", "spotify"}},
		{name: "canonical only", want: "canonical", source: "musicbrainz", count: 1, sources: []string{"musicbrainz"}},
		{name: "single observation", want: "observed", source: "spotify", count: 1, sources: []string{"spotify"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseTruthState(tc.explicit, tc.source, tc.count, tc.sources, tc.issues); got != tc.want {
				t.Fatalf("releaseTruthState()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestConflictingNotificationCanBeHeldAndReleased(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "hold@example.com", "hash", "member", "UTC", "hold-user")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "hold-other@example.com", "hash", "member", "UTC", "hold-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "hold-artist", Name: "Hold Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,first_release_date,date_precision,musicbrainz_url,
		 spotify_id,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "hold-release", artist.ID, "Held Release", "Album", "2026-09-01", 3,
		"https://musicbrainz.org/release-group/hold-release", "hold-spotify",
		"https://open.spotify.com/album/hold-spotify", "both", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,'open',?,?)`, releaseID, "date_conflict", "warning", "hold-fingerprint",
		"Providers disagree on the release date", nowText(), nowText()); err != nil {
		t.Fatal(err)
	}
	prefs, err := s.NotificationPreferences(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	prefs.HoldConflictingNotifications = true
	if err := s.UpdateNotificationPreferences(ctx, prefs); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueEvent(ctx, userID, releaseID, "announcement", "Held release", "Review this release", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("held notification created %d events", events)
	}
	holds, err := s.NotificationHolds(ctx, userID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Reason == "" {
		t.Fatalf("holds=%+v", holds)
	}
	if err := s.ResolveNotificationHold(ctx, otherID, holds[0].ID, "notify"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user hold action error=%v, want sql.ErrNoRows", err)
	}
	if err := s.SetReleaseTruthDecision(ctx, userID, releaseID, "spotify", "Spotify has the current listing"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("released notification events=%d, want 1", events)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, holds[0].ID).Scan(&holds[0].Status); err != nil {
		t.Fatal(err)
	}
	if holds[0].Status != "released" {
		t.Fatalf("hold status=%q, want released", holds[0].Status)
	}
}
