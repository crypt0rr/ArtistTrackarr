package metrics

import (
	"testing"
	"time"
)

func TestRegistryRecordsBoundedOperationalCounters(t *testing.T) {
	registry := New()
	registry.RecordWake()
	registry.RecordTaskOverlap()
	registry.RecordTaskPanic()
	registry.RecordSync(5, 4, 1, 2, 2, 1)
	registry.RecordResolution(3, 1, 1, 1, 0)
	registry.RecordReleaseDay()
	registry.RecordMaintenance()
	registry.RecordDelivery(4, 3, 1, 25*time.Millisecond)
	registry.RecordProviderCooldown("spotify")
	registry.RecordProviderCooldown("itunes")
	registry.RecordProviderCooldown("musicbrainz")
	registry.RecordProviderCooldown("artist-123")

	snapshot := registry.Snapshot()
	if snapshot.WakeSignals != 1 || snapshot.TaskOverlaps != 1 || snapshot.TaskPanics != 1 {
		t.Fatalf("scheduler metrics=%#v", snapshot)
	}
	if snapshot.SyncRuns != 1 || snapshot.SyncDue != 5 || snapshot.SyncSucceeded != 4 || snapshot.SyncFailed != 1 || snapshot.SyncChanged != 2 || snapshot.SyncBackoff != 1 {
		t.Fatalf("sync metrics=%#v", snapshot)
	}
	if snapshot.ResolutionRuns != 1 || snapshot.ResolutionItems != 3 || snapshot.ResolutionFollowed != 1 {
		t.Fatalf("resolution metrics=%#v", snapshot)
	}
	if snapshot.ReleaseDayRuns != 1 || snapshot.MaintenanceRuns != 1 || snapshot.DeliveryBatches != 1 || snapshot.DeliveryLatency != 25*time.Millisecond {
		t.Fatalf("cadence metrics=%#v", snapshot)
	}
	if snapshot.SpotifyCooldownSkips != 1 || snapshot.ITunesCooldownSkips != 1 || snapshot.MusicBrainzCooldownSkips != 1 {
		t.Fatalf("provider metrics=%#v", snapshot)
	}
}

func TestRegistryIgnoresNegativeBatchCounts(t *testing.T) {
	registry := New()
	registry.RecordSync(-1, -1, -1, -1, -1, -1)
	registry.RecordResolution(-1, -1, -1, -1, -1)
	registry.RecordDelivery(-1, -1, -1, -time.Second)
	snapshot := registry.Snapshot()
	if snapshot.SyncDue != 0 || snapshot.ResolutionItems != 0 || snapshot.DeliveryAttempted != 0 || snapshot.DeliveryLatency != 0 {
		t.Fatalf("negative counts changed metrics=%#v", snapshot)
	}
}
