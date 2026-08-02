package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type Runner struct {
	store                 *store.Store
	catalog               catalog.CatalogProvider
	spotify               catalog.SpotifyReleaseProvider
	normalizer            catalog.ReleaseNormalizer
	sender                notify.NotificationSender
	cipher                *security.Cipher
	interval              time.Duration
	spotifyInterval       time.Duration
	logger                *slog.Logger
	syncMu                sync.Mutex
	providerMu            sync.Mutex
	spotifyCooldownLoaded bool
	spotifyCooldownUntil  time.Time
}

type Option func(*Runner)

type resolutionStats struct {
	Processed int
	Followed  int
	Review    int
	Pending   int
	Failed    int
}

type syncStats struct {
	Due       int
	Succeeded int
	Failed    int
	Changed   int
	Unchanged int
	Backoff   int
}

type syncOutcome struct {
	SpotifyChanged   bool
	SpotifyUnchanged bool
	SpotifyBackoff   bool
}

type deliveryStats struct {
	Attempted int
	Sent      int
	Failed    int
}

func WithSpotify(provider catalog.SpotifyReleaseProvider) Option {
	return func(r *Runner) { r.spotify = provider }
}

// WithSpotifyInterval controls the independent Spotify observation cadence.
// Spotify is an enrichment source, so it defaults to a much slower cadence
// than the canonical MusicBrainz sync.
func WithSpotifyInterval(interval time.Duration) Option {
	return func(r *Runner) {
		if interval >= time.Hour {
			r.spotifyInterval = interval
		}
	}
}

func New(s *store.Store, provider catalog.CatalogProvider, normalizer catalog.ReleaseNormalizer,
	sender notify.NotificationSender, cipher *security.Cipher, interval time.Duration, logger *slog.Logger,
	options ...Option) *Runner {
	runner := &Runner{
		store: s, catalog: provider, normalizer: normalizer, sender: sender,
		cipher: cipher, interval: interval, spotifyInterval: 24 * time.Hour, logger: logger,
	}
	for _, option := range options {
		option(runner)
	}
	return runner
}

// spotifyProviderCooldown loads the persisted provider-wide cooldown once per
// process. Individual artist schedules still provide normal cadence, while a
// quota response suppresses every other Spotify attempt until the same safe
// retry time, including after a restart.
func (r *Runner) spotifyProviderCooldown(ctx context.Context, now time.Time) (time.Time, error) {
	r.providerMu.Lock()
	if r.spotifyCooldownLoaded {
		until := r.spotifyCooldownUntil
		r.providerMu.Unlock()
		if until.After(now) {
			return until, nil
		}
		return time.Time{}, nil
	}
	r.providerMu.Unlock()

	health, err := r.store.ProviderHealthByName(ctx, "spotify")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	var until time.Time
	if err == nil && health.NextCheckAt != nil && (health.RateLimited || health.QuotaExceeded) {
		until = *health.NextCheckAt
	}
	r.providerMu.Lock()
	r.spotifyCooldownLoaded = true
	r.spotifyCooldownUntil = until
	r.providerMu.Unlock()
	if until.After(now) {
		return until, nil
	}
	return time.Time{}, nil
}

func (r *Runner) setSpotifyProviderCooldown(until time.Time) {
	if until.IsZero() {
		return
	}
	r.providerMu.Lock()
	if until.After(r.spotifyCooldownUntil) {
		r.spotifyCooldownUntil = until
	}
	r.spotifyCooldownLoaded = true
	r.providerMu.Unlock()
}

func (r *Runner) clearSpotifyProviderCooldown() {
	r.providerMu.Lock()
	r.spotifyCooldownUntil = time.Time{}
	r.spotifyCooldownLoaded = true
	r.providerMu.Unlock()
}

func (r *Runner) Run(ctx context.Context) {
	r.tick(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	tickStarted := time.Now()
	if r.syncMu.TryLock() {
		manualSummary := r.processManualSyncRequests(ctx, time.Now().UTC())
		if manualSummary > 0 {
			r.logger.Info("manual synchronization requests completed", "processed", manualSummary)
		}
		resolutionSummary, err := r.resolveArtistResolutions(ctx, time.Now().UTC())
		if err != nil {
			r.logger.Error("artist resolution failed", "error", err)
		} else {
			r.logger.Info("artist resolution processing completed",
				"processed", resolutionSummary.Processed, "followed", resolutionSummary.Followed,
				"review", resolutionSummary.Review, "pending", resolutionSummary.Pending,
				"failed", resolutionSummary.Failed)
		}
		syncSummary, err := r.syncArtists(ctx, time.Now().UTC())
		if err != nil {
			r.logger.Error("catalog sync failed", "error", err)
		} else {
			r.logger.Info("catalog synchronization completed",
				"due", syncSummary.Due, "succeeded", syncSummary.Succeeded,
				"failed", syncSummary.Failed, "changed", syncSummary.Changed,
				"unchanged", syncSummary.Unchanged, "backoff", syncSummary.Backoff)
		}
		r.syncMu.Unlock()
	} else {
		r.logger.Debug("background tick skipped", "reason", "another synchronization is running")
	}
	if err := r.store.PruneApplicationLogs(ctx, time.Now().UTC().Add(-7*24*time.Hour)); err != nil {
		r.logger.Debug("application log pruning failed", "error", err)
	}
	now := time.Now().UTC()
	if err := r.store.QueueDueReleaseDays(ctx, now); err != nil {
		r.logger.Error("release-day scheduling failed", "error", err)
	} else {
		r.logger.Info("release-day queue completed")
	}
	deliverySummary, err := r.deliver(ctx, now)
	if err != nil {
		r.logger.Error("notification delivery failed", "error", err)
	} else {
		r.logger.Info("notification delivery batch completed",
			"attempted", deliverySummary.Attempted, "sent", deliverySummary.Sent,
			"failed", deliverySummary.Failed)
	}
	r.logger.Info("background tick completed", "duration", time.Since(tickStarted).String())
}

func (r *Runner) processManualSyncRequests(ctx context.Context, now time.Time) int {
	requests, err := r.store.ClaimManualSyncRequests(ctx, 3)
	if err != nil {
		r.logger.Warn("manual synchronization queue failed", "error", err)
		return 0
	}
	for _, req := range requests {
		var syncErr error
		if req.Scope == "artist" && req.ArtistID != nil {
			var artist store.Artist
			artist, syncErr = r.store.ArtistByID(ctx, *req.ArtistID)
			if syncErr == nil {
				_, syncErr = r.syncOne(ctx, artist, now)
			}
		} else if req.Scope == "retry" {
			syncErr = r.store.MarkAllArtistsDue(ctx)
			if syncErr == nil {
				_, syncErr = r.syncArtists(ctx, now)
			}
		}
		if err := r.store.CompleteManualSyncRequest(ctx, req.ID, syncErr); err != nil {
			r.logger.Warn("manual synchronization completion failed", "request_id", req.ID, "error", err)
		}
	}
	return len(requests)
}

func (r *Runner) ResolveArtistResolutionNow(ctx context.Context, resolution store.ArtistResolution) (string, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	return r.resolveArtistResolution(ctx, resolution, time.Now().UTC())
}

func (r *Runner) SelectArtistResolution(ctx context.Context, resolution store.ArtistResolution, candidate store.ResolutionCandidate) (string, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	return r.completeArtistResolution(ctx, resolution, catalog.ArtistResult{
		MBID: candidate.MBID, Name: candidate.Name, SortName: candidate.SortName,
		Type: candidate.Type, Country: candidate.Country, Disambiguation: candidate.Disambiguation,
		Aliases: candidate.Aliases, Score: candidate.Score,
	})
}

func (r *Runner) resolveArtistResolutions(ctx context.Context, now time.Time) (resolutionStats, error) {
	var summary resolutionStats
	resolutions, err := r.store.DueArtistResolutions(ctx, now, 10)
	if err != nil {
		return summary, err
	}
	for _, resolution := range resolutions {
		summary.Processed++
		status, err := r.resolveArtistResolution(ctx, resolution, now)
		if err != nil {
			summary.Failed++
			r.logger.Warn("pending artist resolution failed",
				"resolution_id", resolution.ID, "provider", resolution.Provider, "error", err)
			continue
		}
		switch status {
		case "followed":
			summary.Followed++
		case "review":
			summary.Review++
		case "pending":
			summary.Pending++
		}
	}
	return summary, nil
}

func (r *Runner) resolveArtistResolution(ctx context.Context, resolution store.ArtistResolution, now time.Time) (string, error) {
	matches, err := r.catalog.ResolveExternalArtist(ctx, resolution.ProviderURL)
	if err != nil {
		r.logger.Warn("external artist lookup failed", "resolution_id", resolution.ID, "error", err)
		return "pending", r.retryArtistResolution(ctx, resolution, now, "MusicBrainz is temporarily unavailable.")
	}
	if len(matches) == 1 {
		return r.completeArtistResolution(ctx, resolution, matches[0])
	}
	if len(matches) > 1 {
		candidates := resolutionCandidates(matches)
		if len(candidates) == 0 {
			return "pending", r.retryArtistResolution(ctx, resolution, now, "No MusicBrainz candidates were found yet.")
		}
		return "review", r.store.MarkArtistResolutionReview(
			ctx, resolution.UserID, resolution.ID, candidates,
		)
	}

	matches, err = r.catalog.SearchArtists(ctx, resolution.DisplayName, 10)
	if err != nil {
		r.logger.Warn("artist candidate search failed", "resolution_id", resolution.ID, "error", err)
		return "pending", r.retryArtistResolution(ctx, resolution, now, "MusicBrainz is temporarily unavailable.")
	}
	if len(matches) == 0 {
		return "pending", r.retryArtistResolution(ctx, resolution, now, "No MusicBrainz candidates were found yet.")
	}
	candidates := resolutionCandidates(matches)
	if len(candidates) == 0 {
		return "pending", r.retryArtistResolution(ctx, resolution, now, "No MusicBrainz candidates were found yet.")
	}
	return "review", r.store.MarkArtistResolutionReview(
		ctx, resolution.UserID, resolution.ID, candidates,
	)
}

func (r *Runner) retryArtistResolution(ctx context.Context, resolution store.ArtistResolution, now time.Time, message string) error {
	attempts := resolution.Attempts + 1
	return r.store.RetryArtistResolution(
		ctx, resolution.UserID, resolution.ID, attempts, now.Add(artistResolutionRetryDelay(attempts)), message,
	)
}

func artistResolutionRetryDelay(attempts int) time.Duration {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour}
	return delays[min(max(attempts-1, 0), len(delays)-1)]
}

func resolutionCandidates(matches []catalog.ArtistResult) []store.ResolutionCandidate {
	result := make([]store.ResolutionCandidate, 0, len(matches))
	for _, match := range matches {
		if match.MBID == "" {
			continue
		}
		result = append(result, store.ResolutionCandidate{
			MBID: match.MBID, Name: match.Name, SortName: match.SortName, Type: match.Type,
			Country: match.Country, Disambiguation: match.Disambiguation, Aliases: match.Aliases, Score: match.Score,
		})
	}
	return result
}

func (r *Runner) completeArtistResolution(ctx context.Context, resolution store.ArtistResolution, match catalog.ArtistResult) (string, error) {
	artist := match.StoreArtist()
	artist.SpotifyID = resolution.ProviderID
	artist.SpotifyURL = resolution.ProviderURL
	artist.SpotifyImageURL = resolution.ImageURL
	artist, added, err := r.store.CompleteArtistResolution(ctx, resolution, artist)
	if err != nil {
		return "", err
	}
	if added {
		if _, err := r.syncOne(ctx, artist, time.Now().UTC()); err != nil {
			r.logger.Warn("initial resolved artist sync failed", "artist_id", artist.ID, "error", err)
		}
	}
	return "followed", nil
}

func (r *Runner) SyncArtistNow(ctx context.Context, artist store.Artist) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	// A manual sync is an explicit request to refresh both providers.
	artist.SpotifyNextCheckAt = nil
	_, err := r.syncOne(ctx, artist, time.Now().UTC())
	return err
}

func (r *Runner) syncArtists(ctx context.Context, now time.Time) (syncStats, error) {
	var summary syncStats
	artists, err := r.store.ArtistsDue(ctx, now, 25)
	if err != nil {
		return summary, err
	}
	summary.Due = len(artists)
	for _, artist := range artists {
		outcome, err := r.syncOne(ctx, artist, now)
		if err != nil {
			summary.Failed++
			r.logger.Warn("artist sync failed", "artist_id", artist.ID, "mbid", artist.MBID, "error", err)
			continue
		}
		summary.Succeeded++
		if outcome.SpotifyChanged {
			summary.Changed++
		}
		if outcome.SpotifyUnchanged {
			summary.Unchanged++
		}
		if outcome.SpotifyBackoff {
			summary.Backoff++
		}
	}
	return summary, nil
}

func (r *Runner) syncOne(ctx context.Context, artist store.Artist, now time.Time) (syncOutcome, error) {
	var outcome syncOutcome
	var batches []store.ReleaseBatch
	var providerErrors []error
	var spotifyRateLimit *catalog.SpotifyRateLimitError
	spotifyWasDue := artist.SpotifyID != "" && (artist.SpotifyNextCheckAt == nil || !artist.SpotifyNextCheckAt.After(now))
	spotifySuppressed := false
	var spotifyCooldownUntil time.Time
	if spotifyWasDue && r.spotify != nil {
		cooldown, err := r.spotifyProviderCooldown(ctx, now)
		if err != nil {
			return outcome, err
		}
		if cooldown.After(now) {
			spotifyCooldownUntil = cooldown
			spotifySuppressed = true
			r.logger.Debug("Spotify check suppressed by provider cooldown", "artist_id", artist.ID,
				"retry_after", cooldown.Sub(now).String())
		}
	}
	spotifyKnownDate := ""
	if artist.SpotifyID != "" && r.spotify != nil {
		var err error
		spotifyKnownDate, err = r.store.LatestSpotifyReleaseDate(ctx, artist.ID)
		if err != nil {
			return outcome, err
		}
	}
	spotifyPrimary := artist.SpotifyID != "" && r.spotify != nil && (spotifyWasDue || spotifyKnownDate != "")
	spotifySucceeded := false
	if spotifyPrimary && spotifyWasDue && !spotifySuppressed {
		var spotifyReleases []store.Release
		var spotifyErr error
		if incremental, ok := r.spotify.(catalog.SpotifyIncrementalReleaseProvider); ok {
			spotifyReleases, spotifyErr = incremental.ArtistReleasesSince(ctx, artist.SpotifyID, spotifyKnownDate)
		} else {
			spotifyReleases, spotifyErr = r.spotify.ArtistReleases(ctx, artist.SpotifyID)
		}
		if spotifyErr == nil {
			spotifySucceeded = true
			r.clearSpotifyProviderCooldown()
			changed, err := r.store.SpotifyBatchChanged(ctx, spotifyReleases)
			if err != nil {
				return outcome, err
			}
			outcome.SpotifyChanged = changed
			outcome.SpotifyUnchanged = !changed
			_ = r.store.UpsertProviderHealth(ctx, "spotify", true, nil, false, false, "")
			batches = append(batches, store.ReleaseBatch{
				Provider: "spotify", Releases: r.normalizer.Normalize(spotifyReleases),
			})
		} else {
			var retryAt *time.Time
			if errors.As(spotifyErr, &spotifyRateLimit) {
				t := now.Add(syncRetryDelay(spotifyRateLimit, r.spotifyInterval))
				retryAt = &t
				r.setSpotifyProviderCooldown(t)
			}
			_ = r.store.UpsertProviderHealth(ctx, "spotify", false, retryAt, spotifyRateLimit != nil, spotifyRateLimit != nil && spotifyRateLimit.QuotaExceeded, sanitizedProviderError(spotifyErr))
			providerErrors = append(providerErrors, spotifyErr)
			if errors.As(spotifyErr, &spotifyRateLimit) {
				if !spotifyRateLimit.AlreadyBlocked {
					r.logger.Warn("Spotify release observation rate limited",
						"artist_id", artist.ID,
						"reason", spotifyRateLimit.Reason,
						"retry_after", spotifyRateLimit.RetryAfter.String(),
						"quota_exceeded", spotifyRateLimit.QuotaExceeded,
					)
				}
			} else {
				r.logger.Warn("Spotify release observation failed", "artist_id", artist.ID, "error", spotifyErr)
			}
		}
	}
	// Spotify is authoritative whenever it has a successful observation. If it
	// is unavailable, MusicBrainz remains a temporary fallback so a provider
	// outage does not stop canonical release tracking entirely.
	if !spotifySucceeded && !(spotifyPrimary && !spotifyWasDue) {
		releases, err := r.catalog.ArtistReleases(ctx, artist.MBID)
		if err == nil {
			_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", true, nil, false, false, "")
			batches = append(batches, store.ReleaseBatch{
				Provider: "musicbrainz", Releases: r.normalizer.Normalize(releases),
			})
		} else {
			t := now.Add(providerFailureRetryDelay(nil, r.interval))
			_ = r.store.UpsertProviderHealth(ctx, "musicbrainz", false, &t, false, false, sanitizedProviderError(err))
			providerErrors = append(providerErrors, err)
			r.logger.Warn("MusicBrainz release observation failed", "artist_id", artist.ID, "error", err)
		}
	}
	if spotifyPrimary && !spotifyWasDue && spotifyKnownDate != "" {
		if err := r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
	if len(batches) == 0 {
		retryAt := now.Add(providerFailureRetryDelay(spotifyRateLimit, r.interval))
		if spotifyRateLimit != nil {
			r.logger.Debug("Spotify check retry scheduled", "artist_id", artist.ID,
				"retry_after", syncRetryDelay(spotifyRateLimit, r.spotifyInterval).String(),
				"quota_exceeded", spotifyRateLimit.QuotaExceeded)
			providerErrors = append(providerErrors, r.store.ScheduleSpotifyCheck(ctx, artist.ID,
				now.Add(syncRetryDelay(spotifyRateLimit, r.spotifyInterval))))
		} else if spotifySuppressed {
			providerErrors = append(providerErrors, r.store.ScheduleSpotifyCheck(ctx, artist.ID, spotifyCooldownUntil))
		}
		r.logger.Debug("artist sync retry scheduled", "artist_id", artist.ID,
			"retry_after", providerFailureRetryDelay(spotifyRateLimit, r.interval).String())
		return outcome, errors.Join(errors.Join(providerErrors...), r.store.ScheduleArtistCheck(ctx, artist.ID, retryAt))
	}
	if err := r.store.ApplyReleaseBatches(ctx, artist, batches, now); err != nil {
		return outcome, err
	}
	if err := r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval); err != nil {
		return outcome, err
	}
	if spotifySuppressed {
		if err := r.store.ScheduleSpotifyCheck(ctx, artist.ID, spotifyCooldownUntil); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
	if spotifyWasDue {
		if spotifyRateLimit != nil {
			r.logger.Debug("Spotify check retry scheduled", "artist_id", artist.ID,
				"retry_after", syncRetryDelay(spotifyRateLimit, r.spotifyInterval).String(),
				"quota_exceeded", spotifyRateLimit.QuotaExceeded)
			return outcome, r.store.ScheduleSpotifyCheck(ctx, artist.ID, now.Add(syncRetryDelay(spotifyRateLimit, r.spotifyInterval)))
		}
		upcoming := false
		for _, batch := range batches {
			if batch.Provider != "spotify" {
				continue
			}
			for _, release := range batch.Releases {
				if isFutureRelease(release.FirstReleaseDate, now) {
					upcoming = true
					break
				}
			}
		}
		if err := r.store.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, r.spotifyInterval, outcome.SpotifyChanged, upcoming); err != nil {
			return outcome, err
		}
		outcome.SpotifyBackoff = outcome.SpotifyUnchanged && !upcoming
	}
	return outcome, nil
}

func isFutureRelease(value string, now time.Time) bool {
	date, precision := value, len(value)
	if precision != 4 && precision != 7 && precision != 10 {
		return false
	}
	today := now.UTC()
	if precision == 4 {
		year, err := strconv.Atoi(date)
		return err == nil && year > today.Year()
	}
	if precision == 7 {
		parsed, err := time.Parse("2006-01", date)
		return err == nil && parsed.After(time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC))
	}
	parsed, err := time.Parse("2006-01-02", date)
	return err == nil && parsed.After(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC))
}

func sanitizedProviderError(err error) string {
	if err == nil {
		return ""
	}
	var rateLimit *catalog.SpotifyRateLimitError
	if errors.As(err, &rateLimit) {
		// Retry durations are persisted separately as next_check_at. Keeping
		// them out of this message prevents the admin view from displaying a
		// countdown that becomes stale after the first render.
		message := rateLimit.Operation + " returned 429 Too Many Requests"
		if reason := strings.TrimSpace(rateLimit.Reason); reason != "" {
			message += " (" + reason + ")"
		}
		return message
	}
	msg := strings.TrimSpace(err.Error())
	msg = strings.ReplaceAll(msg, "https://", "[url]")
	msg = strings.ReplaceAll(msg, "http://", "[url]")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

func syncRetryDelay(rateLimit *catalog.SpotifyRateLimitError, interval time.Duration) time.Duration {
	if rateLimit == nil {
		return interval
	}
	if rateLimit.QuotaExceeded {
		return max(rateLimit.RetryAfter, interval)
	}
	delay := max(rateLimit.RetryAfter, time.Minute)
	return min(delay, interval)
}

func providerFailureRetryDelay(rateLimit *catalog.SpotifyRateLimitError, interval time.Duration) time.Duration {
	if rateLimit != nil {
		return syncRetryDelay(rateLimit, interval)
	}
	return min(15*time.Minute, interval)
}

func (r *Runner) deliver(ctx context.Context, now time.Time) (deliveryStats, error) {
	var summary deliveryStats
	deliveries, err := r.store.DueDeliveries(ctx, now, 25)
	if err != nil {
		return summary, err
	}
	for _, delivery := range deliveries {
		summary.Attempted++
		serviceURL, err := r.cipher.Decrypt(delivery.Destination.EncryptedURL)
		if err == nil {
			err = r.sender.Send(ctx, serviceURL, delivery.Title, delivery.Body)
		}
		if err == nil {
			if err := r.store.MarkDeliverySent(ctx, delivery.ID, now); err != nil {
				return summary, err
			}
			summary.Sent++
			continue
		}
		summary.Failed++
		r.logger.Warn("notification attempt failed",
			"delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", err)
		if err := r.store.MarkDeliveryFailed(ctx, delivery.ID, delivery.Attempts+1, err.Error(), now); err != nil {
			return summary, err
		}
	}
	return summary, nil
}
