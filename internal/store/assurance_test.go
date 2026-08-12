package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/logging"
)

func TestCoverageAssuranceClassifiesFreshnessAndProviderFailures(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	old := now.Add(-72 * time.Hour)
	for _, test := range []struct {
		name       string
		item       ArtistCoverage
		want       string
		wantReason string
	}{
		{name: "healthy", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "spotify", Status: "healthy", LastSuccessAt: &fresh}}}, want: "healthy"},
		{name: "delayed", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "musicbrainz", Status: "healthy", LastSuccessAt: &old}}}, want: "delayed"},
		{name: "degraded", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "spotify", Status: "cooldown", LastFailureAt: &fresh}}}, want: "degraded"},
		{name: "pending", item: ArtistCoverage{}, want: "pending"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason, provider := coverageAssurance(test.item, now)
			if got != test.want || strings.TrimSpace(reason) == "" {
				t.Fatalf("coverageAssurance()=(%q,%q,%q), want status %q", got, reason, provider, test.want)
			}
		})
	}
}

func TestWatchlistAssuranceRanksAtRiskArtists(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "assurance@example.com", "hash", "member", "UTC", "assurance")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	artists := []struct {
		mbid   string
		name   string
		status *ArtistProviderStatus
	}{
		{mbid: "assurance-healthy", name: "Healthy Artist", status: &ArtistProviderStatus{Provider: "spotify", Status: "healthy", LastSuccessAt: timePtr(now.Add(-time.Hour))}},
		{mbid: "assurance-delayed", name: "Delayed Artist", status: &ArtistProviderStatus{Provider: "musicbrainz", Status: "healthy", LastSuccessAt: timePtr(now.Add(-72 * time.Hour))}},
		{mbid: "assurance-degraded", name: "Degraded Artist", status: &ArtistProviderStatus{Provider: "itunes", Status: "cooldown", LastFailureAt: timePtr(now), NextCheckAt: timePtr(now.Add(time.Hour))}},
		{mbid: "assurance-pending", name: "Pending Artist"},
	}
	for _, input := range artists {
		artist, upsertErr := s.UpsertArtist(ctx, Artist{MBID: input.mbid, Name: input.name})
		if upsertErr != nil {
			t.Fatal(upsertErr)
		}
		if _, followErr := s.Follow(ctx, userID, artist.ID); followErr != nil {
			t.Fatal(followErr)
		}
		if input.status != nil {
			input.status.ArtistID = artist.ID
			if statusErr := s.RecordArtistProviderStatus(ctx, *input.status); statusErr != nil {
				t.Fatal(statusErr)
			}
		}
	}
	summary, err := s.WatchlistAssurance(ctx, userID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 4 || summary.Healthy != 1 || summary.Delayed != 1 || summary.Degraded != 1 || summary.Pending != 1 {
		t.Fatalf("unexpected assurance summary=%#v", summary)
	}
	if len(summary.AtRisk) != 2 || summary.AtRisk[0].AssuranceStatus != "degraded" || summary.AtRisk[1].AssuranceStatus != "delayed" {
		t.Fatalf("at-risk ordering=%#v", summary.AtRisk)
	}
}

func TestDiagnosticsReturnsSafeOperationalCounters(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "diagnostics@example.com", "hash", "member", "UTC", "diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "diagnostics-artist", Name: "Diagnostics Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManualSyncRequest(ctx, userID, "artist", &artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertApplicationLog(ctx, logging.Entry{Time: time.Now().UTC(), Level: "INFO", Message: "diagnostic test"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.DatabaseHealthy || snapshot.SchemaVersion < 1 || snapshot.FollowedArtists != 1 || snapshot.QueuedSyncs != 1 || snapshot.RecentLogEntries < 1 {
		t.Fatalf("unexpected diagnostics snapshot=%#v", snapshot)
	}
}

func TestDiagnosticsUsesConfiguredProviderCadence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.SetProviderHealthCadences(time.Hour, 2*time.Hour)
	old := time.Now().UTC().Add(-5 * time.Hour)
	if err := s.UpsertProviderHealth(ctx, "spotify", true, nil, false, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE provider_health SET last_success_at=?, updated_at=? WHERE provider=?`,
		timeText(old), timeText(old), "spotify"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range snapshot.Providers {
		if provider.Provider == "spotify" && provider.Status != "stale" {
			t.Fatalf("Spotify status=%q, want stale with 2h cadence", provider.Status)
		}
	}
}

func timePtr(value time.Time) *time.Time { return &value }
