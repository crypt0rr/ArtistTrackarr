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

	succeeded  bool
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
	itunesSucceeded   bool
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
	allowITunes := !result.spotifySucceeded && !(spotifyPrimary && !spotifyWasDue)
	itunes, err := r.observeITunes(ctx, artist, now, allowITunes)
	if err != nil {
		return result, err
	}
	r.recordProviderStatus(ctx, artist.ID, itunes, now)
	result.itunesSucceeded = itunes.succeeded
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

	allowMusicBrainz := !result.spotifySucceeded && !result.itunesSucceeded && !(spotifyPrimary && !spotifyWasDue)
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
		observation.suppressed = true
		observation.cooldown = cooldown
		observation.status = "cooldown"
		observation.nextCheckAt = &cooldown
		r.logger.Debug("Spotify check suppressed by provider cooldown", "artist_id", artist.ID,
			"retry_after", cooldown.Sub(now).String())
		return observation, nil
	}

	var releases []store.Release
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
		observation.succeeded = true
		observation.releases = releases
		observation.spotifyChanged = changed
		observation.spotifyUnchanged = !changed
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
			observation.status = "deferred"
		}
		return observation, nil
	}
	cooldown, err := r.itunesProviderCooldown(ctx, now)
	if err != nil {
		return observation, err
	}
	if cooldown.After(now) {
		observation.status = "cooldown"
		observation.cooldown = cooldown
		observation.nextCheckAt = &cooldown
		r.logger.Debug("iTunes check suppressed by provider cooldown", "artist_id", artist.ID,
			"retry_after", cooldown.Sub(now).String())
		return observation, nil
	}

	observation.attempted = true
	releases, err := r.itunes.ArtistReleases(ctx, artist.Name)
	if err == nil {
		r.clearITunesProviderCooldown()
		if creditProvider, ok := r.itunes.(catalog.ReleaseCreditProvider); ok {
			credits, creditErr := creditProvider.ArtistReleaseCredits(ctx, artist.Name, releases)
			if creditErr != nil {
				r.logger.Debug("iTunes credit enrichment failed", "artist_id", artist.ID, "error", creditErr)
			} else {
				releases = append(releases, credits...)
			}
		}
		observation.succeeded = true
		observation.releases = releases
		observation.status = "healthy"
		observation.nextCheckAt = timePtr(now.Add(r.interval))
		_ = r.store.UpsertProviderHealth(ctx, "itunes", true, nil, false, false, "")
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
				r.logger.Debug("MusicBrainz credit enrichment failed", "artist_id", artist.ID, "error", creditErr)
			} else {
				releases = append(releases, credits...)
			}
		}
		observation.succeeded = true
		observation.releases = releases
		observation.status = "healthy"
		observation.nextCheckAt = timePtr(now.Add(r.interval))
		r.clearMusicBrainzCooldown()
		_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", true, nil, false, false, "")
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
	if observation.succeeded {
		success = timePtr(now)
	}
	if observation.err != nil {
		failure = timePtr(now)
	}
	releaseCount := -1
	if observation.succeeded {
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
