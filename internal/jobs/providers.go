package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

// providerObservation is the deliberately small hand-off between an
// individual provider and the ordered release strategy. Provider-specific
// cooldown and error handling stays with the provider, while syncOne remains
// responsible for persistence and scheduling decisions shared by all sources.
type providerObservation struct {
	provider    string
	releases    []store.Release
	err         error
	status      string
	lastError   string
	nextCheckAt *time.Time
	attempted   bool

	// healthy means the provider request completed successfully. succeeded
	// means it returned an actionable release batch; an empty catalog is
	// healthy but intentionally hands control to the next fallback provider.
	healthy    bool
	succeeded  bool
	empty      bool
	suppressed bool
	deferred   bool
	cooldown   time.Time

	spotifyRateLimit *catalog.SpotifyRateLimitError
	itunesRateLimit  *catalog.ITunesRateLimitError
	spotifyChanged   bool
	spotifyUnchanged bool
}

type providerStrategyResult struct {
	batches        []store.ReleaseBatch
	providerErrors []error

	spotifySucceeded  bool
	spotifyHealthy    bool
	spotifyAttempted  bool
	itunesSucceeded   bool
	itunesHealthy     bool
	spotifySuppressed bool
	spotifyDeferred   bool
	spotifyCooldown   time.Time

	spotifyRateLimit *catalog.SpotifyRateLimitError
	itunesRateLimit  *catalog.ITunesRateLimitError
	spotifyChanged   bool
	spotifyUnchanged bool
}

// observeReleaseProviders runs the release sources in their documented order:
// Spotify, iTunes, then MusicBrainz. A successful source stops the fallback
// chain; a failed or cooling-down source hands control to the next provider.
func (r *Runner) observeReleaseProviders(ctx context.Context, artist store.Artist, now time.Time,
	spotifyKnownDate string, spotifyWasDue bool, spotifyPrimary bool) (providerStrategyResult, error) {
	var result providerStrategyResult

	spotify, err := r.observeSpotify(ctx, artist, now, spotifyKnownDate, spotifyWasDue, spotifyPrimary)
	if err != nil {
		return result, err
	}
	r.recordProviderStatus(ctx, artist.ID, spotify, now)
	result.spotifySucceeded = spotify.succeeded
	result.spotifyHealthy = spotify.healthy
	result.spotifyAttempted = spotify.attempted
	result.spotifySuppressed = spotify.suppressed
	result.spotifyDeferred = spotify.deferred
	result.spotifyCooldown = spotify.cooldown
	result.spotifyRateLimit = spotify.spotifyRateLimit
	result.spotifyChanged = spotify.spotifyChanged
	result.spotifyUnchanged = spotify.spotifyUnchanged
	if spotify.succeeded {
		result.batches = append(result.batches, store.ReleaseBatch{
			Provider: spotify.provider,
			Releases: r.normalizer.Normalize(spotify.releases),
		})
	}
	if spotify.err != nil {
		result.providerErrors = append(result.providerErrors, spotify.err)
	}

	// A Spotify artist with a known release date is intentionally deferred
	// until its adaptive cadence says it is due. This prevents fallback sources
	// from turning an enrichment-only check into a high-volume poll.
	allowITunes := !result.spotifySucceeded && (!spotifyPrimary || spotifyWasDue)
	itunes, err := r.observeITunes(ctx, artist, now, allowITunes)
	if err != nil {
		return result, err
	}
	r.recordProviderStatus(ctx, artist.ID, itunes, now)
	result.itunesSucceeded = itunes.succeeded
	result.itunesHealthy = itunes.healthy
	result.itunesRateLimit = itunes.itunesRateLimit
	if itunes.succeeded {
		result.batches = append(result.batches, store.ReleaseBatch{
			Provider: itunes.provider,
			Releases: r.normalizer.Normalize(itunes.releases),
		})
	}
	if itunes.err != nil {
		result.providerErrors = append(result.providerErrors, itunes.err)
	}

	allowMusicBrainz := !result.spotifySucceeded && !result.itunesSucceeded && (!spotifyPrimary || spotifyWasDue)
	if allowMusicBrainz {
		musicBrainz, err := r.observeMusicBrainz(ctx, artist, now)
		if err != nil {
			return result, err
		}
		r.recordProviderStatus(ctx, artist.ID, musicBrainz, now)
		if musicBrainz.succeeded {
			result.batches = append(result.batches, store.ReleaseBatch{
				Provider: musicBrainz.provider,
				Releases: r.normalizer.Normalize(musicBrainz.releases),
			})
		}
		if musicBrainz.err != nil {
			result.providerErrors = append(result.providerErrors, musicBrainz.err)
		}
	} else {
		// MusicBrainz was intentionally not contacted because an earlier
		// provider supplied the release catalog (or Spotify's adaptive check
		// was deferred). Keep that distinction visible per artist without
		// overwriting a previous failure or cooldown record.
		r.recordProviderStatus(ctx, artist.ID, providerObservation{
			provider: "musicbrainz", status: "standby",
		}, now)
	}

	return result, nil
}

func (r *Runner) observeSpotify(ctx context.Context, artist store.Artist, now time.Time,
	knownDate string, wasDue, primary bool) (providerObservation, error) {
	observation := providerObservation{provider: "spotify"}
	if !primary || !wasDue || r.spotify == nil {
		observation.deferred = primary && !wasDue && knownDate != ""
		if r.spotify == nil {
			observation.status = "not_configured"
		} else {
			observation.status = "deferred"
			if artist.SpotifyNextCheckAt != nil {
				value := *artist.SpotifyNextCheckAt
				observation.nextCheckAt = &value
			}
		}
		return observation, nil
	}
	cooldown, err := r.spotifyProviderCooldown(ctx, now)
	if err != nil {
		return observation, err
	}
	if cooldown.After(now) {
		r.metrics.RecordProviderCooldown("spotify")
		observation.suppressed = true
		observation.cooldown = cooldown
		observation.status = "cooldown"
		observation.nextCheckAt = &cooldown
		r.logger.Debug("Spotify check suppressed by provider cooldown", "artist_id", artist.ID,
			"retry_after", cooldown.Sub(now).String())
		return observation, nil
	}

	var releases []store.Release
	// The Spotify release cache is intended for short discovery/follow bursts,
	// not for scheduler decisions. A due scheduled check must reach the
	// provider so a 24-hour cache cannot be mistaken for an unchanged catalog
	// when the adaptive interval is shorter than the cache TTL.
	r.invalidateSpotifyReleaseCache(artist)
	if incremental, ok := r.spotify.(catalog.SpotifyIncrementalReleaseProvider); ok {
		releases, err = incremental.ArtistReleasesSince(ctx, artist.SpotifyID, knownDate)
	} else {
		releases, err = r.spotify.ArtistReleases(ctx, artist.SpotifyID)
	}
	observation.attempted = true
	if err == nil {
		changed, changedErr := r.store.SpotifyBatchChanged(ctx, releases)
		if changedErr != nil {
			return observation, changedErr
		}
		r.clearSpotifyProviderCooldown()
		observation.healthy = true
		observation.succeeded = len(releases) > 0
		observation.empty = len(releases) == 0
		observation.releases = releases
		observation.spotifyChanged = changed
		// An empty response is a healthy request, but not an unchanged
		// catalog. Keeping it out of the unchanged signal prevents callers
		// from treating a fallback-triggering empty result as adaptive
		// Spotify success.
		observation.spotifyUnchanged = len(releases) > 0 && !changed
		observation.status = "healthy"
		observation.nextCheckAt = timePtr(now.Add(r.spotifyInterval))
		_ = r.store.UpsertProviderHealth(ctx, "spotify", true, nil, false, false, "")
		return observation, nil
	}

	var rateLimit *catalog.SpotifyRateLimitError
	var retryAt *time.Time
	if errors.As(err, &rateLimit) {
		t := now.Add(syncRetryDelay(rateLimit, r.spotifyInterval))
		retryAt = &t
		observation.spotifyRateLimit = rateLimit
		r.setSpotifyProviderCooldown(t)
	}
	_ = r.store.UpsertProviderHealth(ctx, "spotify", false, retryAt, rateLimit != nil,
		rateLimit != nil && rateLimit.QuotaExceeded, sanitizedProviderError(err))
	observation.err = err
	observation.status = "failed"
	observation.lastError = sanitizedProviderError(err)
	observation.nextCheckAt = retryAt
	if rateLimit != nil {
		if !rateLimit.AlreadyBlocked {
			r.logger.Warn("Spotify release observation rate limited", "artist_id", artist.ID,
				"reason", rateLimit.Reason, "retry_after", rateLimit.RetryAfter.String(),
				"quota_exceeded", rateLimit.QuotaExceeded)
		}
	} else {
		r.logger.Warn("Spotify release observation failed", "artist_id", artist.ID, "error", err)
	}
	return observation, nil
}

func (r *Runner) observeITunes(ctx context.Context, artist store.Artist, now time.Time,
	allowed bool) (providerObservation, error) {
	observation := providerObservation{provider: "itunes"}
	if !allowed || r.itunes == nil {
		if r.itunes == nil {
			observation.status = "not_configured"
		} else {
			observation.status = "standby"
		}
		return observation, nil
	}
	cooldown, err := r.itunesProviderCooldown(ctx, now)
	if err != nil {
		return observation, err
	}
	if cooldown.After(now) {
		r.metrics.RecordProviderCooldown("itunes")
		observation.status = "cooldown"
		observation.cooldown = cooldown
		observation.nextCheckAt = &cooldown
		r.logger.Debug("iTunes check suppressed by provider cooldown", "artist_id", artist.ID,
			"retry_after", cooldown.Sub(now).String())
		return observation, nil
	}

	observation.attempted = true
	// A credit-enrichment failure does not invalidate the releases themselves, so
	// it is carried alongside them rather than replacing the observation.
	creditFailure := ""
	releases, err := r.itunesReleasesForArtist(ctx, artist)
	// Apple's lookup endpoint hard-caps at 200 collections and ignores offset,
	// so a prolific artist's catalogue comes back as an arbitrary subset. The
	// releases found are still worth importing, but this is not a clean healthy
	// check: reporting it as one set succeeded, which suppressed the MusicBrainz
	// fallback that would have supplied the missing releases.
	truncated := &catalog.ITunesCatalogTruncatedError{}
	catalogTruncated := errors.As(err, &truncated)
	if catalogTruncated {
		err = nil
	}
	if err == nil {
		r.clearITunesProviderCooldown()
		if creditProvider, ok := r.itunes.(catalog.ReleaseCreditProvider); ok {
			credits, creditErr := creditProvider.ArtistReleaseCredits(ctx, artist.Name, releases)
			switch {
			case creditErr == nil:
				releases = append(releases, credits...)
			case errors.Is(creditErr, context.Canceled), errors.Is(creditErr, context.DeadlineExceeded):
				// Shutdown or a cancelled sync is not a provider fault.
				r.logger.Debug("iTunes credit enrichment cancelled", "artist_id", artist.ID)
			default:
				// The releases themselves are good, so this is not an outage and
				// the fallback chain should not be disturbed - but it must not be
				// invisible either. The MusicBrainz credit path records health and
				// a reason for exactly this; iTunes reported "healthy" with the
				// failure only at Debug, so repeated credit failures looked like a
				// provider with nothing to say.
				creditFailure = sanitizedProviderError(creditErr)
				r.logger.Warn("iTunes credit enrichment failed",
					"artist_id", artist.ID, "error", creditFailure)
			}
		}
		observation.healthy = true
		// A truncated catalogue must not count as success, or the fallback that
		// would fill the gap never runs.
		observation.succeeded = len(releases) > 0 && !catalogTruncated
		observation.empty = len(releases) == 0
		observation.releases = releases
		observation.status = "healthy"
		if creditFailure != "" {
			observation.status = "degraded"
			observation.lastError = creditFailure
		}
		if catalogTruncated {
			observation.status = "degraded"
			if observation.lastError == "" {
				observation.lastError = truncated.Error()
			}
			r.logger.Warn("iTunes catalogue truncated at the provider maximum; falling back to MusicBrainz",
				"artist_id", artist.ID, "limit", truncated.Limit)
		}
		observation.nextCheckAt = timePtr(now.Add(r.interval))
		_ = r.store.UpsertProviderHealth(ctx, "itunes", true, nil, false, false, creditFailure)
		return observation, nil
	}

	// A reachable iTunes API can legitimately have no exact identity for a
	// canonical artist. Treat that negative result as a healthy lookup so the
	// fallback chain continues without turning Trust Center/provider health
	// into a false outage signal.
	var notFound *catalog.ITunesArtistNotFoundError
	var ambiguous *catalog.ITunesAmbiguousArtistError
	if errors.As(err, &notFound) || errors.As(err, &ambiguous) {
		observation.healthy = true
		observation.empty = true
		observation.status = "not_found"
		if ambiguous != nil {
			observation.status = "ambiguous"
		}
		observation.lastError = sanitizedProviderError(err)
		observation.nextCheckAt = timePtr(now.Add(r.interval))
		_ = r.store.UpsertProviderHealth(ctx, "itunes", true, nil, false, false, "")
		r.logger.Debug("iTunes artist lookup returned no usable identity", "artist_id", artist.ID,
			"status", observation.status)
		return observation, nil
	}

	var rateLimit *catalog.ITunesRateLimitError
	var retryAt *time.Time
	if errors.As(err, &rateLimit) {
		t := now.Add(max(rateLimit.RetryAfter, time.Minute))
		retryAt = &t
		observation.itunesRateLimit = rateLimit
		r.setITunesProviderCooldown(t)
	}
	_ = r.store.UpsertProviderHealth(ctx, "itunes", false, retryAt, rateLimit != nil, false, sanitizedProviderError(err))
	observation.err = err
	observation.status = "failed"
	observation.lastError = sanitizedProviderError(err)
	observation.nextCheckAt = retryAt
	if rateLimit != nil {
		if !rateLimit.AlreadyBlocked {
			r.logger.Warn("iTunes release observation rate limited", "artist_id", artist.ID,
				"retry_after", rateLimit.RetryAfter.String())
		}
	} else {
		r.logger.Warn("iTunes release observation failed", "artist_id", artist.ID, "error", err)
	}
	return observation, nil
}

func (r *Runner) observeMusicBrainz(ctx context.Context, artist store.Artist, now time.Time) (providerObservation, error) {
	observation := providerObservation{provider: "musicbrainz"}
	cooldown, err := r.musicBrainzProviderCooldown(ctx, now)
	if err != nil {
		return observation, err
	}
	if cooldown.After(now) {
		r.metrics.RecordProviderCooldown("musicbrainz")
		observation.status = "cooldown"
		observation.nextCheckAt = &cooldown
		r.logger.Debug("MusicBrainz check suppressed by provider cooldown", "artist_id", artist.ID,
			"retry_after", cooldown.Sub(now).String())
		return observation, nil
	}
	observation.attempted = true
	releases, err := r.catalog.ArtistReleases(ctx, artist.MBID)
	if err == nil {
		if creditProvider, ok := r.catalog.(catalog.ReleaseCreditProvider); ok {
			credits, creditErr := creditProvider.ArtistReleaseCredits(ctx, artist.MBID, releases)
			if creditErr != nil {
				// Credit discovery is a second scheduled MusicBrainz call. Keep
				// its outage state in the same persisted provider cooldown as the
				// release lookup so a restart cannot immediately repeat a burst.
				retryAt := now.Add(r.musicBrainzFailureDelay())
				r.setMusicBrainzCooldown(retryAt)
				observation.cooldown = retryAt
				observation.nextCheckAt = &retryAt
				observation.status = "degraded"
				observation.lastError = sanitizedProviderError(creditErr)
				observation.err = creditErr
				_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", false, &retryAt, false, false, observation.lastError)
				r.logger.Warn("MusicBrainz credit observation failed", "artist_id", artist.ID,
					"retry_after", retryAt.Sub(now).String(), "error", observation.lastError)
			} else {
				releases = append(releases, credits...)
			}
		}
		observation.healthy = true
		observation.succeeded = len(releases) > 0
		observation.empty = len(releases) == 0
		observation.releases = releases
		if observation.status == "" {
			observation.status = "healthy"
			observation.nextCheckAt = timePtr(now.Add(r.interval))
			r.clearMusicBrainzCooldown()
			_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", true, nil, false, false, "")
		}
		return observation, nil
	}
	retryAt := now.Add(r.musicBrainzFailureDelay())
	r.setMusicBrainzCooldown(retryAt)
	observation.cooldown = retryAt
	_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", false, &retryAt, false, false, sanitizedProviderError(err))
	observation.err = err
	observation.status = "failed"
	observation.lastError = sanitizedProviderError(err)
	observation.nextCheckAt = &retryAt
	r.logger.Warn("MusicBrainz release observation failed", "artist_id", artist.ID, "error", err)
	return observation, nil
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func (r *Runner) recordProviderStatus(ctx context.Context, artistID int64, observation providerObservation, now time.Time) {
	if observation.provider == "" || observation.status == "" {
		return
	}
	var attempt, success, failure *time.Time
	if observation.attempted {
		attempt = timePtr(now)
	}
	if observation.healthy {
		success = timePtr(now)
	}
	if observation.err != nil {
		failure = timePtr(now)
	}
	releaseCount := -1
	if observation.healthy {
		releaseCount = len(observation.releases)
	}
	if err := r.store.RecordArtistProviderStatus(ctx, store.ArtistProviderStatus{
		ArtistID: artistID, Provider: observation.provider, Status: observation.status,
		LastAttemptAt: attempt, LastSuccessAt: success, LastFailureAt: failure,
		NextCheckAt: observation.nextCheckAt, ReleaseCount: releaseCount,
		LastError: observation.lastError, UpdatedAt: now,
	}); err != nil {
		r.logger.Debug("artist provider status persistence failed", "artist_id", artistID,
			"provider", observation.provider, "error", err)
	}
}
