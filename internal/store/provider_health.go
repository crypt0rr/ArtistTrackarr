package store

import (
	"strings"
	"time"
)

// ProviderHealthStaleAfter returns the maximum useful age of a successful
// provider check for the health display. It intentionally uses a conservative
// multiple of each provider's normal cadence so a slow but functioning
// provider is not presented as unavailable.
func ProviderHealthStaleAfter(provider string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "musicbrainz", "itunes":
		return 12 * time.Hour
	case "spotify", "listenbrainz":
		return 48 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ProviderHealthStatus is the single status policy used by both the web
// health view and administrator diagnostics. A failure newer than the last
// success always wins; an old success becomes stale rather than remaining
// indefinitely healthy.
func ProviderHealthStatus(provider ProviderHealth, now time.Time, staleAfter time.Duration) string {
	latestFailure := provider.LastFailureAt != nil &&
		(provider.LastSuccessAt == nil || !provider.LastFailureAt.Before(*provider.LastSuccessAt))
	if latestFailure {
		switch {
		case provider.QuotaExceeded:
			return "quota limited"
		case provider.RateLimited:
			return "rate limited"
		default:
			return "degraded"
		}
	}
	if provider.LastSuccessAt != nil {
		if staleAfter > 0 && !now.Before(*provider.LastSuccessAt) && now.Sub(*provider.LastSuccessAt) > staleAfter {
			return "stale"
		}
		return "healthy"
	}
	if provider.LastFailureAt != nil {
		return "unavailable"
	}
	return "no success yet"
}
