package store

import (
	"context"
	"testing"
	"time"
)

func TestOperationalStatusSeparatesDatabaseAndBackgroundHealth(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	status, reasons := OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: true}, "running", now)
	if status != "degraded" || len(reasons) != 1 || reasons[0] != "backup not recorded" {
		t.Fatalf("fresh status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: true, LastBackupAt: timePtr(now), OldestQueueAt: timePtr(now.Add(-20 * time.Minute)), PausedDestinations: 1}, "running", now)
	if status != "degraded" || len(reasons) != 2 {
		t.Fatalf("degraded status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: true, LastBackupAt: timePtr(now)}, "running", now)
	if status != "healthy" || len(reasons) != 0 {
		t.Fatalf("healthy status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: false}, "running", now)
	if status != "unavailable" || len(reasons) != 1 || reasons[0] != "database unavailable" {
		t.Fatalf("unavailable status=%q reasons=%v", status, reasons)
	}
}

func TestOperationalSnapshotsAreRedactedAndBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	snapshot := DiagnosticsSnapshot{
		CheckedAt: now, DatabaseHealthy: true, SchemaVersion: 26, FollowedArtists: 2,
		Releases: 4, PendingDeliveries: 3, DatabaseBytes: 123456,
		LastBackupAt: timePtr(now), LastRestoreResult: "passed",
	}
	if err := s.RecordOperationalSnapshot(ctx, snapshot, "healthy", "running"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.OperationalSnapshots(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "healthy" || rows[0].RunnerStatus != "running" || rows[0].DatabaseBytes != 123456 {
		t.Fatalf("snapshots=%#v", rows)
	}
	if rows[0].LastBackupAt == nil || rows[0].LastRestoreResult != "passed" {
		t.Fatalf("snapshot timestamps/result=%#v", rows[0])
	}

	for i := 1; i <= operationalSnapshotLimit+10; i++ {
		item := snapshot
		item.CheckedAt = now.Add(time.Duration(i) * time.Minute)
		if err := s.RecordOperationalSnapshot(ctx, item, "healthy", "running"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM operational_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != operationalSnapshotLimit {
		t.Fatalf("snapshot count=%d, want %d", count, operationalSnapshotLimit)
	}
}
