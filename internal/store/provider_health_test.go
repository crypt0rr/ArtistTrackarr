package store

import (
	"testing"
	"time"
)

func TestProviderHealthStatusUsesFreshnessAndFailureOrder(t *testing.T) {
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	old := now.Add(-49 * time.Hour)
	failure := now.Add(-30 * time.Minute)

	if got := ProviderHealthStatus(ProviderHealth{LastSuccessAt: &fresh}, now, 48*time.Hour); got != "healthy" {
		t.Fatalf("fresh status=%q", got)
	}
	if got := ProviderHealthStatus(ProviderHealth{LastSuccessAt: &old}, now, 48*time.Hour); got != "stale" {
		t.Fatalf("old status=%q, want stale", got)
	}
	if got := ProviderHealthStatus(ProviderHealth{LastSuccessAt: &fresh, LastFailureAt: &failure}, now, 48*time.Hour); got != "degraded" {
		t.Fatalf("failure status=%q", got)
	}
	if got := ProviderHealthStatus(ProviderHealth{LastSuccessAt: &old, LastFailureAt: &failure, RateLimited: true}, now, 48*time.Hour); got != "rate limited" {
		t.Fatalf("rate-limited status=%q", got)
	}
}

func TestProviderHealthStaleAfterDefaults(t *testing.T) {
	if got := ProviderHealthStaleAfter("musicbrainz"); got != 12*time.Hour {
		t.Fatalf("MusicBrainz stale threshold=%v", got)
	}
	if got := ProviderHealthStaleAfter("spotify"); got != 48*time.Hour {
		t.Fatalf("Spotify stale threshold=%v", got)
	}
	if got := ProviderHealthStaleAfter("unknown"); got != 24*time.Hour {
		t.Fatalf("unknown stale threshold=%v", got)
	}
}
