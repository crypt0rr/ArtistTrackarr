package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type Runner struct {
	store      *store.Store
	catalog    catalog.CatalogProvider
	normalizer catalog.ReleaseNormalizer
	sender     notify.NotificationSender
	cipher     *security.Cipher
	interval   time.Duration
	logger     *slog.Logger
	syncMu     sync.Mutex
}

func New(s *store.Store, provider catalog.CatalogProvider, normalizer catalog.ReleaseNormalizer,
	sender notify.NotificationSender, cipher *security.Cipher, interval time.Duration, logger *slog.Logger) *Runner {
	return &Runner{
		store: s, catalog: provider, normalizer: normalizer, sender: sender,
		cipher: cipher, interval: interval, logger: logger,
	}
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
	if !r.syncMu.TryLock() {
		return
	}
	defer r.syncMu.Unlock()
	now := time.Now().UTC()
	if err := r.resolveArtistResolutions(ctx, now); err != nil {
		r.logger.Error("artist resolution failed", "error", err)
	}
	if err := r.syncArtists(ctx, now); err != nil {
		r.logger.Error("catalog sync failed", "error", err)
	}
	if err := r.store.QueueDueReleaseDays(ctx, now); err != nil {
		r.logger.Error("release-day scheduling failed", "error", err)
	}
	if err := r.deliver(ctx, now); err != nil {
		r.logger.Error("notification delivery failed", "error", err)
	}
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

func (r *Runner) resolveArtistResolutions(ctx context.Context, now time.Time) error {
	resolutions, err := r.store.DueArtistResolutions(ctx, now, 10)
	if err != nil {
		return err
	}
	for _, resolution := range resolutions {
		if _, err := r.resolveArtistResolution(ctx, resolution, now); err != nil {
			r.logger.Warn("pending artist resolution failed",
				"resolution_id", resolution.ID, "provider", resolution.Provider, "error", err)
		}
	}
	return nil
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
		if err := r.syncOne(ctx, artist, time.Now().UTC()); err != nil {
			r.logger.Warn("initial resolved artist sync failed", "artist_id", artist.ID, "error", err)
		}
	}
	return "followed", nil
}

func (r *Runner) SyncArtistNow(ctx context.Context, artist store.Artist) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	return r.syncOne(ctx, artist, time.Now().UTC())
}

func (r *Runner) syncArtists(ctx context.Context, now time.Time) error {
	artists, err := r.store.ArtistsDue(ctx, now, 25)
	if err != nil {
		return err
	}
	for _, artist := range artists {
		if err := r.syncOne(ctx, artist, now); err != nil {
			r.logger.Warn("artist sync failed", "artist_id", artist.ID, "mbid", artist.MBID, "error", err)
			continue
		}
	}
	return nil
}

func (r *Runner) syncOne(ctx context.Context, artist store.Artist, now time.Time) error {
	releases, err := r.catalog.ArtistReleases(ctx, artist.MBID)
	if err != nil {
		return err
	}
	releases = r.normalizer.Normalize(releases)
	if err := r.store.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		return err
	}
	return r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval)
}

func (r *Runner) deliver(ctx context.Context, now time.Time) error {
	deliveries, err := r.store.DueDeliveries(ctx, now, 25)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		serviceURL, err := r.cipher.Decrypt(delivery.Destination.EncryptedURL)
		if err == nil {
			err = r.sender.Send(ctx, serviceURL, delivery.Title, delivery.Body)
		}
		if err == nil {
			if err := r.store.MarkDeliverySent(ctx, delivery.ID, now); err != nil {
				return err
			}
			continue
		}
		r.logger.Warn("notification attempt failed",
			"delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", err)
		if err := r.store.MarkDeliveryFailed(ctx, delivery.ID, delivery.Attempts+1, err.Error(), now); err != nil {
			return err
		}
	}
	return nil
}
