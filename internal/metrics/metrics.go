// Package metrics contains the process-local operational counters used by the
// scheduler and administrator diagnostics. The registry deliberately keeps a
// fixed, low-cardinality set of values: it must never grow with users,
// artists, destinations, or provider payloads.
package metrics

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot is a point-in-time copy of the operational counters. Durations are
// cumulative nanoseconds; callers can derive averages without exposing
// per-request or per-artist data.
type Snapshot struct {
	StartedAt time.Time

	WakeSignals        uint64
	TaskOverlaps       uint64
	TaskPanics         uint64
	SyncRuns           uint64
	SyncDue            uint64
	SyncSucceeded      uint64
	SyncFailed         uint64
	SyncChanged        uint64
	SyncUnchanged      uint64
	SyncBackoff        uint64
	ResolutionRuns     uint64
	ResolutionItems    uint64
	ResolutionFollowed uint64
	ResolutionReview   uint64
	ResolutionPending  uint64
	ResolutionFailed   uint64
	ReleaseDayRuns     uint64
	MaintenanceRuns    uint64
	DeliveryBatches    uint64
	DeliveryAttempted  uint64
	DeliverySent       uint64
	DeliveryFailed     uint64
	DeliveryLatency    time.Duration

	SpotifyCooldownSkips     uint64
	ITunesCooldownSkips      uint64
	MusicBrainzCooldownSkips uint64
}

// Registry is safe for concurrent updates from runner tasks and HTTP
// diagnostics reads.
type Registry struct {
	startedAt                time.Time
	wakeSignals              atomic.Uint64
	taskOverlaps             atomic.Uint64
	taskPanics               atomic.Uint64
	syncRuns                 atomic.Uint64
	syncDue                  atomic.Uint64
	syncSucceeded            atomic.Uint64
	syncFailed               atomic.Uint64
	syncChanged              atomic.Uint64
	syncUnchanged            atomic.Uint64
	syncBackoff              atomic.Uint64
	resolutionRuns           atomic.Uint64
	resolutionItems          atomic.Uint64
	resolutionFollowed       atomic.Uint64
	resolutionReview         atomic.Uint64
	resolutionPending        atomic.Uint64
	resolutionFailed         atomic.Uint64
	releaseDayRuns           atomic.Uint64
	maintenanceRuns          atomic.Uint64
	deliveryBatches          atomic.Uint64
	deliveryAttempted        atomic.Uint64
	deliverySent             atomic.Uint64
	deliveryFailed           atomic.Uint64
	deliveryLatencyNanos     atomic.Int64
	spotifyCooldownSkips     atomic.Uint64
	itunesCooldownSkips      atomic.Uint64
	musicBrainzCooldownSkips atomic.Uint64
}

func New() *Registry {
	return &Registry{startedAt: time.Now().UTC()}
}

func (r *Registry) RecordWake() { r.wakeSignals.Add(1) }

func (r *Registry) RecordTaskOverlap() { r.taskOverlaps.Add(1) }

func (r *Registry) RecordTaskPanic() { r.taskPanics.Add(1) }

func (r *Registry) RecordSync(due, succeeded, failed, changed, unchanged, backoff int) {
	r.syncRuns.Add(1)
	addNonNegative(&r.syncDue, due)
	addNonNegative(&r.syncSucceeded, succeeded)
	addNonNegative(&r.syncFailed, failed)
	addNonNegative(&r.syncChanged, changed)
	addNonNegative(&r.syncUnchanged, unchanged)
	addNonNegative(&r.syncBackoff, backoff)
}

func (r *Registry) RecordResolution(processed, followed, review, pending, failed int) {
	r.resolutionRuns.Add(1)
	addNonNegative(&r.resolutionItems, processed)
	addNonNegative(&r.resolutionFollowed, followed)
	addNonNegative(&r.resolutionReview, review)
	addNonNegative(&r.resolutionPending, pending)
	addNonNegative(&r.resolutionFailed, failed)
}

func (r *Registry) RecordReleaseDay() { r.releaseDayRuns.Add(1) }

func (r *Registry) RecordMaintenance() { r.maintenanceRuns.Add(1) }

func (r *Registry) RecordDelivery(attempted, sent, failed int, duration time.Duration) {
	r.deliveryBatches.Add(1)
	addNonNegative(&r.deliveryAttempted, attempted)
	addNonNegative(&r.deliverySent, sent)
	addNonNegative(&r.deliveryFailed, failed)
	if duration > 0 {
		r.deliveryLatencyNanos.Add(duration.Nanoseconds())
	}
}

func (r *Registry) RecordProviderCooldown(provider string) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "spotify":
		r.spotifyCooldownSkips.Add(1)
	case "itunes":
		r.itunesCooldownSkips.Add(1)
	case "musicbrainz":
		r.musicBrainzCooldownSkips.Add(1)
	}
}

func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	return Snapshot{
		StartedAt:   r.startedAt,
		WakeSignals: r.wakeSignals.Load(), TaskOverlaps: r.taskOverlaps.Load(), TaskPanics: r.taskPanics.Load(),
		SyncRuns: r.syncRuns.Load(), SyncDue: r.syncDue.Load(), SyncSucceeded: r.syncSucceeded.Load(),
		SyncFailed: r.syncFailed.Load(), SyncChanged: r.syncChanged.Load(), SyncUnchanged: r.syncUnchanged.Load(), SyncBackoff: r.syncBackoff.Load(),
		ResolutionRuns: r.resolutionRuns.Load(), ResolutionItems: r.resolutionItems.Load(), ResolutionFollowed: r.resolutionFollowed.Load(),
		ResolutionReview: r.resolutionReview.Load(), ResolutionPending: r.resolutionPending.Load(), ResolutionFailed: r.resolutionFailed.Load(),
		ReleaseDayRuns: r.releaseDayRuns.Load(), MaintenanceRuns: r.maintenanceRuns.Load(), DeliveryBatches: r.deliveryBatches.Load(), DeliveryAttempted: r.deliveryAttempted.Load(),
		DeliverySent: r.deliverySent.Load(), DeliveryFailed: r.deliveryFailed.Load(), DeliveryLatency: time.Duration(r.deliveryLatencyNanos.Load()),
		SpotifyCooldownSkips: r.spotifyCooldownSkips.Load(), ITunesCooldownSkips: r.itunesCooldownSkips.Load(),
		MusicBrainzCooldownSkips: r.musicBrainzCooldownSkips.Load(),
	}
}

func addNonNegative(counter *atomic.Uint64, value int) {
	if value > 0 {
		counter.Add(uint64(value))
	}
}
