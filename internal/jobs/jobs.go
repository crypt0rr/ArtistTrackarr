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
