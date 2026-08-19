package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/metrics"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type Runner struct {
	store                     *store.Store
	catalog                   catalog.CatalogProvider
	spotify                   catalog.SpotifyReleaseProvider
	itunes                    catalog.ITunesReleaseProvider
	listenbrainz              catalog.ListenBrainzProvider
	artwork                   *artwork.Cache
	normalizer                catalog.ReleaseNormalizer
	sender                    notify.NotificationSender
	cipher                    *security.Cipher
	interval                  time.Duration
	spotifyInterval           time.Duration
	logger                    *slog.Logger
	metrics                   *metrics.Registry
	syncMu                    sync.Mutex
	syncTaskMu                sync.Mutex
	deliveryTaskMu            sync.Mutex
	releaseDayTaskMu          sync.Mutex
	maintenanceTaskMu         sync.Mutex
	tasks                     sync.WaitGroup
	lifecycleOnce             sync.Once
	wake                      chan struct{}
	done                      chan struct{}
	providerMu                sync.Mutex
	spotifyCooldownLoaded     bool
	spotifyCooldownUntil      time.Time
	itunesCooldownLoaded      bool
	itunesCooldownUntil       time.Time
	musicBrainzCooldownLoaded bool
	musicBrainzCooldownUntil  time.Time
	musicBrainzFailureStreak  int
	running                   atomic.Bool
	lastActivity              atomic.Int64
	workerID                  string
}

// RunnerStatus is a process-local scheduler snapshot for the admin assurance
// view. It contains no provider credentials or per-artist payloads.
type RunnerStatus struct {
	Running        bool
	LastActivityAt *time.Time
	Metrics        metrics.Snapshot
}

const (
	deliveryCadence    = 10 * time.Second
	syncCadence        = time.Minute
	maintenanceCadence = time.Hour
)

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

type deliveryResult struct {
	sent   bool
	failed bool
	err    error
}

type deliveryWork struct {
	normal *store.Delivery
	digest *store.DigestDelivery
}

type artworkBackfillStats struct {
	ArtistID int64
	Checked  int
	Updated  int
}

func WithSpotify(provider catalog.SpotifyReleaseProvider) Option {
	return func(r *Runner) { r.spotify = provider }
}

func WithITunes(provider catalog.ITunesReleaseProvider) Option {
	return func(r *Runner) { r.itunes = provider }
}

func WithListenBrainz(provider catalog.ListenBrainzProvider) Option {
	return func(r *Runner) { r.listenbrainz = provider }
}

func WithArtworkCache(cache *artwork.Cache) Option {
	return func(r *Runner) { r.artwork = cache }
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
		metrics: metrics.New(),
	}
	if token, err := security.Token(12); err == nil {
		runner.workerID = "runner-" + token
	} else {
		runner.workerID = fmt.Sprintf("runner-%d", time.Now().UnixNano())
	}
	runner.initLifecycle()
	for _, option := range options {
		option(runner)
	}
	return runner
}

func (r *Runner) initLifecycle() {
	r.lifecycleOnce.Do(func() {
		if r.wake == nil {
			r.wake = make(chan struct{}, 1)
		}
		if r.done == nil {
			r.done = make(chan struct{})
		}
		if r.metrics == nil {
			r.metrics = metrics.New()
		}
	})
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

func (r *Runner) itunesProviderCooldown(ctx context.Context, now time.Time) (time.Time, error) {
	r.providerMu.Lock()
	if r.itunesCooldownLoaded {
		until := r.itunesCooldownUntil
		r.providerMu.Unlock()
		if until.After(now) {
			return until, nil
		}
		return time.Time{}, nil
	}
	r.providerMu.Unlock()
	health, err := r.store.ProviderHealthByName(ctx, "itunes")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	var until time.Time
	if err == nil && health.NextCheckAt != nil && health.RateLimited {
		until = *health.NextCheckAt
	}
	r.providerMu.Lock()
	r.itunesCooldownLoaded = true
	r.itunesCooldownUntil = until
	r.providerMu.Unlock()
	if until.After(now) {
		return until, nil
	}
	return time.Time{}, nil
}

func (r *Runner) setITunesProviderCooldown(until time.Time) {
	if until.IsZero() {
		return
	}
	r.providerMu.Lock()
	if until.After(r.itunesCooldownUntil) {
		r.itunesCooldownUntil = until
	}
	r.itunesCooldownLoaded = true
	r.providerMu.Unlock()
}

func (r *Runner) clearITunesProviderCooldown() {
	r.providerMu.Lock()
	r.itunesCooldownUntil = time.Time{}
	r.itunesCooldownLoaded = true
	r.providerMu.Unlock()
}

func (r *Runner) musicBrainzProviderCooldown(ctx context.Context, now time.Time) (time.Time, error) {
	r.providerMu.Lock()
	if r.musicBrainzCooldownLoaded {
		until := r.musicBrainzCooldownUntil
		r.providerMu.Unlock()
		if until.After(now) {
			return until, nil
		}
		return time.Time{}, nil
	}
	r.providerMu.Unlock()
	health, err := r.store.ProviderHealthByName(ctx, "musicbrainz")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	var until time.Time
	if err == nil && health.NextCheckAt != nil {
		until = *health.NextCheckAt
	}
	r.providerMu.Lock()
	r.musicBrainzCooldownLoaded = true
	r.musicBrainzCooldownUntil = until
	r.providerMu.Unlock()
	if until.After(now) {
		return until, nil
	}
	return time.Time{}, nil
}

func (r *Runner) setMusicBrainzCooldown(until time.Time) {
	r.providerMu.Lock()
	r.musicBrainzCooldownLoaded = true
	r.musicBrainzCooldownUntil = until
	r.providerMu.Unlock()
}

func (r *Runner) clearMusicBrainzCooldown() {
	r.providerMu.Lock()
	r.musicBrainzCooldownLoaded = true
	r.musicBrainzCooldownUntil = time.Time{}
	r.musicBrainzFailureStreak = 0
	r.providerMu.Unlock()
}

func (r *Runner) musicBrainzFailureDelay() time.Duration {
	r.providerMu.Lock()
	r.musicBrainzFailureStreak++
	streak := r.musicBrainzFailureStreak
	r.providerMu.Unlock()
	delay := time.Minute * time.Duration(1<<min(streak-1, 6))
	if r.interval > 0 && delay > r.interval {
		return r.interval
	}
	return delay
}

func (r *Runner) Run(ctx context.Context) {
	r.initLifecycle()
	r.running.Store(true)
	r.markActivity()
	defer func() {
		r.running.Store(false)
		r.tasks.Wait()
		close(r.done)
	}()
	if recovered, err := r.store.RecoverExpiredWork(ctx, time.Now().UTC()); err != nil {
		r.logger.Warn("durable work recovery failed", "error", err)
	} else if recovered > 0 {
		r.logger.Info("durable work recovered", "rows", recovered)
	}
	if reconciled, err := r.store.ReconcileStaleDeliveryAttempts(ctx, time.Now().UTC(), 10*time.Minute); err != nil {
		r.logger.Warn("stale delivery attempt reconciliation failed", "error", err)
	} else if reconciled > 0 {
		r.logger.Info("stale delivery attempts reconciled", "attempts", reconciled)
	}
	if ctx.Err() == nil {
		r.launchSync(ctx)
		r.launchReleaseDayQueue(ctx)
		r.launchDelivery(ctx)
		r.launchMaintenance(ctx)
	}
	deliveryTicker := time.NewTicker(deliveryCadence)
	defer deliveryTicker.Stop()
	syncTicker := time.NewTicker(syncCadence)
	defer syncTicker.Stop()
	maintenanceTicker := time.NewTicker(maintenanceCadence)
	defer maintenanceTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deliveryTicker.C:
			if ctx.Err() != nil {
				return
			}
			r.launchDelivery(ctx)
		case <-syncTicker.C:
			if ctx.Err() != nil {
				return
			}
			r.launchSync(ctx)
			r.launchReleaseDayQueue(ctx)
		case <-maintenanceTicker.C:
			if ctx.Err() != nil {
				return
			}
			r.launchMaintenance(ctx)
		case <-r.wake:
			if ctx.Err() != nil {
				return
			}
			// A wake is intended for newly queued/manual synchronization. Run
			// the small queueing and delivery jobs as well so an onboarding
			// notification does not wait for the next regular cadence.
			r.launchSync(ctx)
			r.launchReleaseDayQueue(ctx)
			r.launchDelivery(ctx)
		}
	}
}

// Wake asks the background runner to process due work promptly. The signal is
// coalesced so a multi-follow request cannot create an unbounded goroutine or
// wake-up backlog.
func (r *Runner) Wake() {
	r.initLifecycle()
	r.markActivity()
	select {
	case r.wake <- struct{}{}:
		r.metrics.RecordWake()
	default:
	}
}

// Status returns the scheduler's current lifecycle state. The timestamp is
// best-effort and is intentionally process-local; persisted provider health
// remains the source of truth for cooldowns and last successful observations.
func (r *Runner) Status() RunnerStatus {
	status := RunnerStatus{Running: r.running.Load(), Metrics: r.metrics.Snapshot()}
	if nanos := r.lastActivity.Load(); nanos > 0 {
		value := time.Unix(0, nanos).UTC()
		status.LastActivityAt = &value
	}
	return status
}

func (r *Runner) markActivity() {
	r.lastActivity.Store(time.Now().UTC().UnixNano())
}

// Done closes when Run has stopped and no background tick can access the
// store anymore.
func (r *Runner) Done() <-chan struct{} {
	r.initLifecycle()
	return r.done
}

func (r *Runner) startTask(ctx context.Context, name string, guard *sync.Mutex, work func(context.Context)) {
	r.initLifecycle()
	r.markActivity()
	if !guard.TryLock() {
		r.metrics.RecordTaskOverlap()
		r.logger.Debug("background task skipped", "task", name, "reason", "already running")
		return
	}
	r.tasks.Add(1)
	go func() {
		defer r.tasks.Done()
		defer guard.Unlock()
		defer func() {
			if recovered := recover(); recovered != nil {
				r.metrics.RecordTaskPanic()
				r.logPanic("scheduled task", name, recovered)
			}
		}()
		work(ctx)
	}()
}

// logPanic keeps scheduler failures isolated. Panic values and stack traces
// are bounded and redacted before they reach the structured log sink; the
// scheduler itself remains alive so one malformed provider response cannot
// stop unrelated jobs.
func (r *Runner) logPanic(scope string, name string, recovered any) {
	message := notify.RedactError(fmt.Errorf("%v", recovered))
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	stack := notify.RedactError(errors.New(string(debug.Stack())))
	if len(stack) > 2048 {
		stack = stack[:2048] + "..."
	}
	if r.logger != nil {
		r.logger.Error("background panic recovered", "scope", scope, "task", name, "panic", message, "stack", stack)
	}
}

func (r *Runner) safeDelivery(ctx context.Context, now time.Time, item deliveryWork) (result deliveryResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logPanic("notification delivery", "worker", recovered)
			result = deliveryResult{failed: true, err: errors.New("notification delivery panic recovered")}
		}
	}()
	if item.normal != nil {
		return r.deliverOne(ctx, now, *item.normal)
	}
	if item.digest != nil {
		return r.deliverDigestOne(ctx, now, *item.digest)
	}
	return deliveryResult{failed: true, err: errors.New("empty notification delivery")}
}

func (r *Runner) launchSync(ctx context.Context) {
	r.startTask(ctx, "synchronization", &r.syncTaskMu, r.runSyncCadence)
}

func (r *Runner) launchDelivery(ctx context.Context) {
	r.startTask(ctx, "delivery", &r.deliveryTaskMu, r.runDeliveryCadence)
}

func (r *Runner) launchReleaseDayQueue(ctx context.Context) {
	r.startTask(ctx, "release-day queue", &r.releaseDayTaskMu, r.runReleaseDayQueue)
}

func (r *Runner) launchMaintenance(ctx context.Context) {
	r.startTask(ctx, "maintenance", &r.maintenanceTaskMu, r.runMaintenance)
}

func (r *Runner) runSyncCadence(ctx context.Context) {
	if !r.syncMu.TryLock() {
		r.logger.Debug("synchronization task skipped", "reason", "another synchronization is running")
		return
	}
	defer r.syncMu.Unlock()
	manualSummary := r.processManualSyncRequests(ctx, time.Now().UTC())
	if manualSummary > 0 {
		r.logger.Info("manual synchronization requests completed", "processed", manualSummary)
	}
	resolutionSummary, err := r.resolveArtistResolutions(ctx, time.Now().UTC())
	if err != nil {
		r.metrics.RecordResolution(resolutionSummary.Processed, resolutionSummary.Followed,
			resolutionSummary.Review, resolutionSummary.Pending, resolutionSummary.Failed)
		r.logger.Error("artist resolution failed", "error", err)
	} else {
		r.metrics.RecordResolution(resolutionSummary.Processed, resolutionSummary.Followed,
			resolutionSummary.Review, resolutionSummary.Pending, resolutionSummary.Failed)
		r.logger.Info("artist resolution processing completed",
			"processed", resolutionSummary.Processed, "followed", resolutionSummary.Followed,
			"review", resolutionSummary.Review, "pending", resolutionSummary.Pending,
			"failed", resolutionSummary.Failed)
	}
	syncSummary, err := r.syncArtists(ctx, time.Now().UTC())
	if err != nil {
		r.metrics.RecordSync(syncSummary.Due, syncSummary.Succeeded, syncSummary.Failed,
			syncSummary.Changed, syncSummary.Unchanged, syncSummary.Backoff)
		r.logger.Error("catalog sync failed", "error", err)
	} else {
		r.metrics.RecordSync(syncSummary.Due, syncSummary.Succeeded, syncSummary.Failed,
			syncSummary.Changed, syncSummary.Unchanged, syncSummary.Backoff)
		r.logger.Info("catalog synchronization completed",
			"due", syncSummary.Due, "succeeded", syncSummary.Succeeded,
			"failed", syncSummary.Failed, "changed", syncSummary.Changed,
			"unchanged", syncSummary.Unchanged, "backoff", syncSummary.Backoff)
	}
	if artworkSummary, err := r.backfillITunesArtwork(ctx, time.Now().UTC()); err != nil {
		r.logger.Warn("iTunes artwork backfill failed", "error", err)
	} else if artworkSummary != nil {
		r.logger.Info("iTunes artwork backfill completed", "artist_id", artworkSummary.ArtistID,
			"checked", artworkSummary.Checked, "updated", artworkSummary.Updated)
	}
	if statsSummary, err := r.refreshListenBrainz(ctx, time.Now().UTC()); err != nil {
		r.logger.Warn("ListenBrainz statistics refresh failed", "error", err)
	} else if statsSummary > 0 {
		r.logger.Info("ListenBrainz statistics refresh completed", "artists", statsSummary)
	}
}

func (r *Runner) runMaintenance(ctx context.Context) {
	r.metrics.RecordMaintenance()
	now := time.Now().UTC()
	if recovered, err := r.store.RecoverExpiredWork(ctx, now); err != nil {
		r.logger.Warn("durable work recovery failed", "error", err)
	} else if recovered > 0 {
		r.logger.Info("durable work recovered", "rows", recovered)
	}
	if reconciled, err := r.store.ReconcileStaleDeliveryAttempts(ctx, now, 10*time.Minute); err != nil {
		r.logger.Warn("stale delivery attempt reconciliation failed", "error", err)
	} else if reconciled > 0 {
		r.logger.Info("stale delivery attempts reconciled", "attempts", reconciled)
	}
	policy := r.store.RetentionPolicy()
	if err := r.store.PruneApplicationLogs(ctx, now.Add(-time.Duration(policy.ApplicationLogsDays)*24*time.Hour)); err != nil {
		r.logger.Debug("application log pruning failed", "error", err)
	}
	if maintenance, err := r.store.PruneExpiredState(ctx, now); err != nil {
		r.logger.Warn("state maintenance failed", "error", err)
	} else if maintenance.Sessions+maintenance.AuthTokens+maintenance.LoginAttempts+maintenance.ManualSyncs+maintenance.ImportJobs > 0 {
		r.logger.Info("state maintenance completed",
			"sessions", maintenance.Sessions, "auth_tokens", maintenance.AuthTokens,
			"login_attempts", maintenance.LoginAttempts, "manual_syncs", maintenance.ManualSyncs,
			"import_jobs", maintenance.ImportJobs)
	}
	if r.artwork != nil {
		stats, err := r.artwork.Prune(ctx, artwork.DefaultMaxCacheBytes, artwork.DefaultMaxCacheFiles)
		if err != nil {
			r.logger.Warn("artwork cache pruning failed", "error", err)
		} else if stats.RemovedFiles > 0 {
			r.logger.Info("artwork cache pruning completed",
				"removed_files", stats.RemovedFiles, "removed_bytes", stats.RemovedBytes,
				"stale_files", stats.StaleFiles)
		}
	}
	// Persist a redacted hourly point after maintenance has recovered stale
	// work. This gives operators a short historical view even after a restart,
	// while the bounded store method prevents unbounded database growth.
	if snapshot, err := r.store.Diagnostics(ctx); err != nil {
		r.logger.Warn("operational snapshot capture failed", "error", err)
	} else {
		status, _ := store.OperationalStatus(snapshot, "running", now)
		if err := r.store.RecordOperationalSnapshot(ctx, snapshot, status, "running"); err != nil {
			r.logger.Warn("operational snapshot persistence failed", "error", err)
		}
	}
}

func (r *Runner) runReleaseDayQueue(ctx context.Context) {
	r.metrics.RecordReleaseDay()
	now := time.Now().UTC()
	if err := r.store.QueueDueReleaseDays(ctx, now); err != nil {
		r.logger.Error("release-day scheduling failed", "error", err)
	} else {
		r.logger.Info("release-day queue completed")
	}
	if queued, err := r.store.QueueDueReleaseDigests(ctx, now); err != nil {
		r.logger.Warn("release digest scheduling failed", "error", err)
	} else if queued > 0 {
		r.logger.Info("release digest queue completed", "runs", queued)
	}
}

func (r *Runner) runDeliveryCadence(ctx context.Context) {
	now := time.Now().UTC()
	started := time.Now()
	deliverySummary, err := r.deliver(ctx, now)
	r.metrics.RecordDelivery(deliverySummary.Attempted, deliverySummary.Sent, deliverySummary.Failed, time.Since(started))
	if err != nil {
		r.logger.Error("notification delivery failed", "error", err)
	} else if deliverySummary.Attempted > 0 {
		r.logger.Info("notification delivery batch completed",
			"attempted", deliverySummary.Attempted, "sent", deliverySummary.Sent,
			"failed", deliverySummary.Failed)
	}
}

// backfillITunesArtwork fills artwork on existing iTunes rows only. It is
// intentionally one artist per tick: the provider request is rate limited and
// this work must never create release records or notification events.
func (r *Runner) backfillITunesArtwork(ctx context.Context, now time.Time) (*artworkBackfillStats, error) {
	if r.itunes == nil {
		return nil, nil
	}
	artist, ok, err := r.store.DueITunesArtworkArtist(ctx, now)
	if err != nil || !ok {
		return nil, err
	}
	if cooldown, err := r.itunesProviderCooldown(ctx, now); err != nil {
		return nil, err
	} else if cooldown.After(now) {
		r.logger.Debug("iTunes artwork backfill suppressed by provider cooldown",
			"artist_id", artist.ID, "retry_after", cooldown.Sub(now).String())
		return nil, nil
	}
	releases, err := r.itunes.ArtistReleases(ctx, artist.Name)
	if err != nil {
		var rateLimit *catalog.ITunesRateLimitError
		if errors.As(err, &rateLimit) {
			delay := min(max(rateLimit.RetryAfter, time.Minute), 6*time.Hour)
			next := now.Add(delay)
			r.setITunesProviderCooldown(next)
			_ = r.store.UpsertProviderHealth(ctx, "itunes", false, &next, true, false, sanitizedProviderError(err))
			if retryErr := r.store.ScheduleITunesArtworkRetry(ctx, artist.ID, next); retryErr != nil {
				return nil, errors.Join(err, retryErr)
			}
			if !rateLimit.AlreadyBlocked {
				r.logger.Warn("iTunes artwork backfill rate limited", "artist_id", artist.ID,
					"retry_after", delay.String())
			}
			return nil, nil
		}
		delay := artistResolutionRetryDelay(artist.Attempts + 1)
		next := now.Add(delay)
		_ = r.store.UpsertProviderHealth(ctx, "itunes", false, &next, false, false, sanitizedProviderError(err))
		if retryErr := r.store.ScheduleITunesArtworkRetry(ctx, artist.ID, next); retryErr != nil {
			return nil, errors.Join(err, retryErr)
		}
		return nil, nil
	}
	r.clearITunesProviderCooldown()
	_ = r.store.UpsertProviderHealth(ctx, "itunes", true, nil, false, false, "")
	checked, updated, err := r.store.ApplyITunesArtworkBackfill(ctx, artist.ID, releases, now)
	if err != nil {
		return nil, err
	}
	return &artworkBackfillStats{ArtistID: artist.ID, Checked: checked, Updated: updated}, nil
}

func (r *Runner) refreshListenBrainz(ctx context.Context, now time.Time) (int, error) {
	if r.listenbrainz == nil {
		return 0, nil
	}
	artists, err := r.store.DueListenBrainzArtists(ctx, now, 50)
	if err != nil || len(artists) == 0 {
		return 0, err
	}
	mbids := make([]string, 0, len(artists))
	ids := make([]int64, 0, len(artists))
	eligible := make([]store.Artist, 0, len(artists))
	for _, artist := range artists {
		if strings.TrimSpace(artist.MBID) == "" {
			continue
		}
		mbids = append(mbids, artist.MBID)
		ids = append(ids, artist.ID)
		eligible = append(eligible, artist)
	}
	if len(mbids) == 0 {
		return 0, nil
	}
	values, err := r.listenbrainz.Popularity(ctx, mbids)
	if err != nil {
		next := now.Add(6 * time.Hour)
		_ = r.store.ScheduleListenBrainzRetry(ctx, ids, next, sanitizedProviderError(err))
		_ = r.store.UpsertProviderHealth(ctx, "listenbrainz", false, &next, false, false, sanitizedProviderError(err))
		return 0, err
	}
	byID := make(map[int64]store.ListenBrainzStats, len(ids))
	missingIDs := make([]int64, 0)
	for _, artist := range eligible {
		stats, ok := values[strings.ToLower(strings.TrimSpace(artist.MBID))]
		if !ok {
			// ListenBrainz may legitimately omit an MBID from a successful
			// response. Do not overwrite known totals with zeros; just move the
			// next refresh forward while retaining the previous row.
			missingIDs = append(missingIDs, artist.ID)
			continue
		}
		byID[artist.ID] = store.ListenBrainzStats{ArtistID: artist.ID, MBID: artist.MBID, TotalListenCount: stats.TotalListenCount, TotalUserCount: stats.TotalUserCount}
	}
	if err := r.store.SaveListenBrainzStats(ctx, byID, now, now.Add(24*time.Hour)); err != nil {
		return 0, err
	}
	if err := r.store.ScheduleListenBrainzRefresh(ctx, missingIDs, now.Add(24*time.Hour)); err != nil {
		return 0, err
	}
	_ = r.store.UpsertProviderHealth(ctx, "listenbrainz", true, nil, false, false, "")
	return len(byID), nil
}

func (r *Runner) processManualSyncRequests(ctx context.Context, now time.Time) int {
	requests, err := r.store.ClaimManualSyncRequestsWithLease(ctx, 3, r.workerID, 5*time.Minute)
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
				r.invalidateSpotifyReleaseCache(artist)
				_, syncErr = r.syncOne(ctx, artist, now)
			}
		} else if req.Scope == "retry" {
			syncErr = r.store.MarkAllArtistsDue(ctx)
			if syncErr == nil {
				_, syncErr = r.syncArtists(ctx, now)
			}
		}
		if err := r.store.CompleteManualSyncRequestOwned(ctx, req.ID, r.workerID, syncErr); err != nil {
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
		Aliases: candidate.Aliases, Genres: candidate.Genres, Score: candidate.Score,
	}, true)
}

// QueueSelectedArtistResolution completes a reviewed mapping without doing
// provider work in the HTTP request. The Follow operation brings the artist
// due immediately; Wake lets the runner perform the initial sync.
func (r *Runner) QueueSelectedArtistResolution(ctx context.Context, resolution store.ArtistResolution, candidate store.ResolutionCandidate) (string, error) {
	status, err := r.completeArtistResolution(ctx, resolution, catalog.ArtistResult{
		MBID: candidate.MBID, Name: candidate.Name, SortName: candidate.SortName,
		Type: candidate.Type, Country: candidate.Country, Disambiguation: candidate.Disambiguation,
		Aliases: candidate.Aliases, Genres: candidate.Genres, Score: candidate.Score,
	}, false)
	if err == nil {
		r.Wake()
	}
	return status, err
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
		return r.completeArtistResolution(ctx, resolution, matches[0], true)
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
			Genres: match.Genres,
		})
	}
	return result
}

func (r *Runner) completeArtistResolution(ctx context.Context, resolution store.ArtistResolution, match catalog.ArtistResult, syncInitial bool) (string, error) {
	artist := match.StoreArtist()
	if resolution.Provider == "spotify" {
		artist.SpotifyID = resolution.ProviderID
		artist.SpotifyURL = resolution.ProviderURL
		artist.SpotifyImageURL = resolution.ImageURL
	}
	artist, added, err := r.store.CompleteArtistResolution(ctx, resolution, artist)
	if err != nil {
		return "", err
	}
	if added && syncInitial {
		r.invalidateSpotifyReleaseCache(artist)
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
	r.invalidateSpotifyReleaseCache(artist)
	_, err := r.syncOne(ctx, artist, time.Now().UTC())
	return err
}

type spotifyReleaseCacheInvalidator interface {
	InvalidateArtistReleases(string)
}

func (r *Runner) invalidateSpotifyReleaseCache(artist store.Artist) {
	if r.spotify == nil || strings.TrimSpace(artist.SpotifyID) == "" {
		return
	}
	if invalidator, ok := r.spotify.(spotifyReleaseCacheInvalidator); ok {
		invalidator.InvalidateArtistReleases(artist.SpotifyID)
	}
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
	if genres, genreErr := r.store.ArtistGenres(ctx, artist.ID); genreErr == nil && len(genres) == 0 {
		if metadata, resolveErr := r.catalog.ResolveArtist(ctx, artist.MBID); resolveErr == nil && len(metadata.Genres) > 0 {
			if saveErr := r.store.SaveArtistGenres(ctx, artist.ID, metadata.Genres); saveErr != nil {
				r.logger.Debug("artist genre metadata save failed", "artist_id", artist.ID, "error", saveErr)
			}
		}
	}
	spotifyWasDue := artist.SpotifyID != "" && (artist.SpotifyNextCheckAt == nil || !artist.SpotifyNextCheckAt.After(now))
	spotifyKnownDate := ""
	if artist.SpotifyID != "" && r.spotify != nil {
		var err error
		spotifyKnownDate, err = r.store.LatestSpotifyReleaseDate(ctx, artist.ID)
		if err != nil {
			return outcome, err
		}
	}
	spotifyPrimary := artist.SpotifyID != "" && r.spotify != nil && (spotifyWasDue || spotifyKnownDate != "")
	strategy, err := r.observeReleaseProviders(ctx, artist, now, spotifyKnownDate, spotifyWasDue, spotifyPrimary)
	if err != nil {
		return outcome, err
	}
	outcome.SpotifyChanged = strategy.spotifyChanged
	outcome.SpotifyUnchanged = strategy.spotifyUnchanged
	if strategy.spotifyDeferred {
		if err := r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval); err != nil {
			return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
		}
		return outcome, nil
	}
	if len(strategy.batches) == 0 {
		// Empty provider catalogs are successful health checks but are not
		// actionable release observations. Once every fallback provider has
		// answered successfully, advance the normal artist cadence instead of
		// treating the empty response as a failure.
		// A successful empty catalog is safe to treat as a normal cadence only
		// when every provider that was attempted completed cleanly.  If a
		// fallback provider failed or is cooling down, keep the artist on a
		// bounded retry cadence instead of letting an empty response mask the
		// outage.
		if len(strategy.providerErrors) == 0 && (strategy.spotifyHealthy || strategy.itunesHealthy) {
			if err := r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval); err != nil {
				return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
			}
			if spotifyWasDue && strategy.spotifyAttempted {
				// Empty Spotify results are a healthy request but not an
				// actionable catalog. They must not enter adaptive backoff;
				// retry on the bounded failure cadence instead.
				retryAt := now.Add(providerFailureRetryDelay(strategy.spotifyRateLimit, r.interval))
				if strategy.spotifyRateLimit != nil {
					retryAt = now.Add(syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval))
				}
				if err := r.store.ScheduleSpotifyCheck(ctx, artist.ID, retryAt); err != nil {
					return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
				}
			}
			return outcome, nil
		}
		retryAt := now.Add(providerFailureRetryDelay(strategy.spotifyRateLimit, r.interval))
		if strategy.spotifyRateLimit != nil {
			r.logger.Debug("Spotify check retry scheduled", "artist_id", artist.ID,
				"retry_after", syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval).String(),
				"quota_exceeded", strategy.spotifyRateLimit.QuotaExceeded)
			if scheduleErr := r.store.ScheduleSpotifyCheck(ctx, artist.ID,
				now.Add(syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval))); scheduleErr != nil {
				strategy.providerErrors = append(strategy.providerErrors,
					r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, scheduleErr))
			}
		} else if strategy.spotifySuppressed {
			if scheduleErr := r.store.ScheduleSpotifyCheck(ctx, artist.ID, strategy.spotifyCooldown); scheduleErr != nil {
				strategy.providerErrors = append(strategy.providerErrors,
					r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, scheduleErr))
			}
		}
		r.logger.Debug("artist sync retry scheduled", "artist_id", artist.ID,
			"retry_after", providerFailureRetryDelay(strategy.spotifyRateLimit, r.interval).String())
		retryErr := r.store.ScheduleArtistCheck(ctx, artist.ID, retryAt)
		if retryErr != nil {
			retryErr = r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, retryErr)
		}
		return outcome, errors.Join(errors.Join(strategy.providerErrors...), retryErr)
	}
	if err := r.store.ApplyReleaseBatches(ctx, artist, strategy.batches, now); err != nil {
		return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
	}
	if err := r.store.MarkArtistChecked(ctx, artist.ID, now, r.interval); err != nil {
		return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
	}
	if strategy.spotifySuppressed {
		if err := r.store.ScheduleSpotifyCheck(ctx, artist.ID, strategy.spotifyCooldown); err != nil {
			return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
		}
		return outcome, nil
	}
	if spotifyWasDue && strategy.spotifySucceeded {
		if strategy.spotifyRateLimit != nil {
			r.logger.Debug("Spotify check retry scheduled", "artist_id", artist.ID,
				"retry_after", syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval).String(),
				"quota_exceeded", strategy.spotifyRateLimit.QuotaExceeded)
			if err := r.store.ScheduleSpotifyCheck(ctx, artist.ID, now.Add(syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval))); err != nil {
				return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
			}
			return outcome, nil
		}
		upcoming := false
		for _, batch := range strategy.batches {
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
			return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
		}
		outcome.SpotifyBackoff = outcome.SpotifyUnchanged && !upcoming
	} else if spotifyWasDue && strategy.spotifyAttempted && !strategy.spotifySucceeded {
		// A failed or empty Spotify response must not be counted as an
		// unchanged catalog merely because a fallback provider produced data.
		// Keep Spotify on the bounded retry cadence and leave adaptive streaks
		// untouched.
		retryAt := now.Add(providerFailureRetryDelay(strategy.spotifyRateLimit, r.interval))
		if strategy.spotifyRateLimit != nil {
			retryAt = now.Add(syncRetryDelay(strategy.spotifyRateLimit, r.spotifyInterval))
		}
		if err := r.store.ScheduleSpotifyCheck(ctx, artist.ID, retryAt); err != nil {
			return outcome, r.scheduleSyncPersistenceFailure(ctx, artist.ID, now, err)
		}
	}
	return outcome, nil
}

// scheduleSyncPersistenceFailure keeps a malformed or otherwise failed
// persistence step from pinning an artist at the front of the due queue. The
// original error remains the caller's primary signal; retry scheduling is a
// best-effort recovery action with the same bounded delay used for provider
// failures.
func (r *Runner) scheduleSyncPersistenceFailure(ctx context.Context, artistID int64, now time.Time, cause error) error {
	retryAfter := providerFailureRetryDelay(nil, r.interval)
	retryAt := now.Add(retryAfter)
	if err := r.store.ScheduleArtistRetry(ctx, artistID, now, retryAt); err != nil {
		r.logger.Warn("artist sync retry scheduling failed", "artist_id", artistID, "error", err)
		return errors.Join(cause, err)
	}
	r.logger.Debug("artist sync persistence retry scheduled", "artist_id", artistID,
		"retry_after", retryAfter.String())
	return cause
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
	var itunesRateLimit *catalog.ITunesRateLimitError
	if errors.As(err, &itunesRateLimit) {
		return itunesRateLimit.Operation + " returned 429 Too Many Requests"
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
	deliveries, err := r.store.ClaimDueDeliveries(ctx, now, 25, r.workerID, 5*time.Minute)
	if err != nil {
		return summary, err
	}
	digestLimit := 25 - len(deliveries)
	if digestLimit < 0 {
		digestLimit = 0
	}
	digestDeliveries, err := r.store.ClaimDueDigestDeliveries(ctx, now, digestLimit, r.workerID, 5*time.Minute)
	if err != nil {
		return summary, err
	}
	if len(deliveries) == 0 && len(digestDeliveries) == 0 {
		return summary, nil
	}

	workerCount := min(4, len(deliveries)+len(digestDeliveries))
	work := make(chan deliveryWork)
	results := make(chan deliveryResult, len(deliveries)+len(digestDeliveries))
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range work {
				result := r.safeDelivery(ctx, now, item)
				results <- result
			}
		}()
	}
	for _, delivery := range deliveries {
		item := delivery
		select {
		case <-ctx.Done():
			close(work)
			workers.Wait()
			close(results)
			return summary, ctx.Err()
		case work <- deliveryWork{normal: &item}:
		}
	}
	for _, delivery := range digestDeliveries {
		item := delivery
		select {
		case <-ctx.Done():
			close(work)
			workers.Wait()
			close(results)
			return summary, ctx.Err()
		case work <- deliveryWork{digest: &item}:
		}
	}
	close(work)
	workers.Wait()
	close(results)

	var storageErrors []error
	for result := range results {
		summary.Attempted++
		if result.sent {
			summary.Sent++
		}
		if result.failed {
			summary.Failed++
		}
		if result.err != nil {
			storageErrors = append(storageErrors, result.err)
		}
	}
	if len(storageErrors) > 0 {
		return summary, errors.Join(storageErrors...)
	}
	return summary, nil
}

func (r *Runner) deliverDigestOne(ctx context.Context, now time.Time, delivery store.DigestDelivery) deliveryResult {
	result := deliveryResult{}
	attemptID, attemptErr := r.store.StartDeliveryAttempt(ctx, 0, delivery.ID, delivery.Destination, delivery.Attempts+1, time.Now().UTC())
	if attemptErr != nil {
		r.logger.Warn("record delivery attempt failed", "digest_delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", notify.RedactError(attemptErr))
	}
	var err error
	if r.cipher == nil {
		err = errors.New("notification cipher is unavailable")
	} else {
		var serviceURL string
		serviceURL, err = r.cipher.Decrypt(delivery.Destination.EncryptedURL)
		if err == nil && r.sender == nil {
			err = errors.New("notification sender is unavailable")
		}
		if err == nil {
			err = r.sender.Send(ctx, serviceURL, delivery.Title, delivery.Body)
		}
	}
	if err == nil {
		if markErr := r.store.MarkDigestDeliverySentOwned(ctx, delivery.ID, delivery.ClaimOwner, now); markErr != nil {
			if attemptID > 0 {
				_ = r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, false, markErr.Error(), nil, time.Now().UTC())
			}
			return deliveryResult{failed: true, err: markErr}
		}
		if attemptID > 0 {
			if finishErr := r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, true, "", nil, time.Now().UTC()); finishErr != nil {
				return deliveryResult{sent: true, err: finishErr}
			}
		}
		result.sent = true
		return result
	}

	result.failed = true
	redactedError := notify.RedactError(err)
	r.logger.Warn("release digest delivery attempt failed",
		"digest_delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", redactedError)
	if markErr := r.store.MarkDigestDeliveryFailedOwned(ctx, delivery.ID, delivery.Attempts+1, redactedError, delivery.ClaimOwner, now); markErr != nil {
		result.err = markErr
	}
	if attemptID > 0 {
		var nextRetry *time.Time
		if delivery.Attempts+1 < 5 {
			next := now.Add(time.Minute * time.Duration(1<<min(delivery.Attempts+1, 6)))
			nextRetry = &next
		}
		if finishErr := r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, false, redactedError, nextRetry, time.Now().UTC()); finishErr != nil && result.err == nil {
			result.err = finishErr
		}
	}
	return result
}

func (r *Runner) deliverOne(ctx context.Context, now time.Time, delivery store.Delivery) deliveryResult {
	result := deliveryResult{}
	attemptID, attemptErr := r.store.StartDeliveryAttempt(ctx, delivery.ID, 0, delivery.Destination, delivery.Attempts+1, time.Now().UTC())
	if attemptErr != nil {
		r.logger.Warn("record delivery attempt failed", "delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", notify.RedactError(attemptErr))
	}
	var err error
	if r.cipher == nil {
		err = errors.New("notification cipher is unavailable")
	} else {
		var serviceURL string
		serviceURL, err = r.cipher.Decrypt(delivery.Destination.EncryptedURL)
		if err == nil && r.sender == nil {
			err = errors.New("notification sender is unavailable")
		}
		if err == nil {
			err = r.sender.Send(ctx, serviceURL, delivery.Title, delivery.Body)
		}
	}
	if err == nil {
		if markErr := r.store.MarkDeliverySentOwned(ctx, delivery.ID, delivery.ClaimOwner, now); markErr != nil {
			if attemptID > 0 {
				_ = r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, false, markErr.Error(), nil, time.Now().UTC())
			}
			return deliveryResult{failed: true, err: markErr}
		}
		if attemptID > 0 {
			if finishErr := r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, true, "", nil, time.Now().UTC()); finishErr != nil {
				return deliveryResult{sent: true, err: finishErr}
			}
		}
		result.sent = true
		return result
	}

	result.failed = true
	redactedError := notify.RedactError(err)
	r.logger.Warn("notification attempt failed",
		"delivery_id", delivery.ID, "destination_id", delivery.Destination.ID, "error", redactedError)
	if markErr := r.store.MarkDeliveryFailedOwned(ctx, delivery.ID, delivery.Attempts+1, redactedError, delivery.ClaimOwner, now); markErr != nil {
		result.err = markErr
	}
	if attemptID > 0 {
		var nextRetry *time.Time
		if delivery.Attempts+1 < 5 {
			next := now.Add(time.Minute * time.Duration(1<<min(delivery.Attempts+1, 6)))
			nextRetry = &next
		}
		if finishErr := r.store.FinishDeliveryAttempt(ctx, attemptID, delivery.Destination.ID, false, redactedError, nextRetry, time.Now().UTC()); finishErr != nil && result.err == nil {
			result.err = finishErr
		}
	}
	return result
}
