package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestOperationalStatusSeparatesDatabaseAndBackgroundHealth(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	status, reasons := OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: true}, "running", now)
	if status != "healthy" || len(reasons) != 1 || reasons[0] != "backup not yet established" {
		t.Fatalf("fresh status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{
		DatabaseHealthy: true, LastBackupAt: timePtr(now),
		OldestQueueAt: timePtr(now.Add(-20 * time.Minute)), PausedDestinations: 1,
		ProviderFailures: 1, OldestProviderFailureAt: timePtr(now.Add(-time.Hour)),
		DigestBacklog: 1, OldestDigestBacklogAt: timePtr(now.Add(-16 * time.Minute)),
	}, "running", now)
	if status != "degraded" || len(reasons) != 4 {
		t.Fatalf("degraded status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{
		DatabaseHealthy: true, LastBackupAt: timePtr(now),
		ProviderFailures: 1, OldestProviderFailureAt: timePtr(now.Add(-time.Minute)),
		DigestBacklog: 1, OldestDigestBacklogAt: timePtr(now.Add(-time.Minute)),
	}, "running", now)
	if status != "healthy" || len(reasons) != 0 {
		t.Fatalf("transient status=%q reasons=%v", status, reasons)
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

func TestOperationalStatusPreservesDatabaseFailureClass(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		state  DatabaseHealthState
		reason string
	}{
		{state: DatabaseReadOnly, reason: "database read-only"},
		{state: DatabaseFull, reason: "database full"},
		{state: DatabaseWriteFailed, reason: "database write failed"},
		{state: DatabaseUnavailable, reason: "database unavailable"},
	} {
		status, reasons := OperationalStatus(DiagnosticsSnapshot{DatabaseHealthState: test.state}, "running", now)
		if status != "unavailable" || len(reasons) != 1 || reasons[0] != test.reason {
			t.Fatalf("state=%q status=%q reasons=%v, want %q", test.state, status, reasons, test.reason)
		}
	}
}

func TestOperationalStatusReportsRecoveredAndMissingAgeSafely(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	status, reasons := OperationalStatus(DiagnosticsSnapshot{
		DatabaseHealthy: true, LastBackupAt: timePtr(now), ProviderFailures: 2, DigestBacklog: 3,
	}, "running", now)
	if status != "healthy" || len(reasons) != 0 {
		t.Fatalf("missing-age status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{
		DatabaseHealthy: true, LastBackupAt: timePtr(now), ProviderFailures: 1,
		OldestProviderFailureAt: timePtr(now.Add(-2 * time.Hour)),
	}, "running", now)
	if status != "degraded" || len(reasons) != 1 || reasons[0] != "provider failures" {
		t.Fatalf("persistent provider status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{
		DatabaseHealthy: true, LastBackupAt: timePtr(now), DigestBacklog: 1,
		OldestDigestBacklogAt: timePtr(now.Add(-20 * time.Minute)),
	}, "running", now)
	if status != "degraded" || len(reasons) != 1 || reasons[0] != "digest backlog" {
		t.Fatalf("persistent digest status=%q reasons=%v", status, reasons)
	}
	status, reasons = OperationalStatus(DiagnosticsSnapshot{DatabaseHealthy: true, LastBackupAt: timePtr(now)}, "running", now)
	if status != "healthy" || len(reasons) != 0 {
		t.Fatalf("recovered status=%q reasons=%v", status, reasons)
	}
}

func TestDiagnosticsCapturesBacklogAges(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "diagnostics@example.com", "hash", "member", "UTC", "diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "diagnostics-due", Name: "Diagnostics Due"})
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, userID, artist.ID); err != nil || !added {
		t.Fatalf("follow diagnostics artist: added=%v err=%v", added, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, timeText(old), artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,last_failure_at,last_error,updated_at) VALUES(?,?,?,?)`, "spotify", timeText(old), "temporary", timeText(old)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "diagnostics", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	var destinationID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM destinations WHERE user_id=? ORDER BY id DESC LIMIT 1`, userID).Scan(&destinationID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`, userID, "daily", "2026-08-14", "Digest", "Body", 1, "pending", timeText(old))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`, runID, destinationID, "pending", timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "diagnostics-future", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	var futureDestinationID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM destinations WHERE user_id=? AND id<>? ORDER BY id DESC LIMIT 1`, userID, destinationID).Scan(&futureDestinationID); err != nil {
		t.Fatal(err)
	}
	futureDelivery := now.Add(48 * time.Hour)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`, runID, futureDestinationID, "pending", timeText(futureDelivery)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.OldestProviderFailureAt == nil || now.Sub(*snapshot.OldestProviderFailureAt) < time.Hour {
		t.Fatalf("provider failure age=%v", snapshot.OldestProviderFailureAt)
	}
	if snapshot.OldestDigestBacklogAt == nil || now.Sub(*snapshot.OldestDigestBacklogAt) < time.Hour {
		t.Fatalf("digest backlog age=%v", snapshot.OldestDigestBacklogAt)
	}
	if snapshot.DueSyncArtists != 1 || snapshot.OldestDueSyncAt == nil || !snapshot.OldestDueSyncAt.Equal(old) {
		t.Fatalf("due sync diagnostics count=%d oldest=%v", snapshot.DueSyncArtists, snapshot.OldestDueSyncAt)
	}
	if snapshot.FutureDeliveries != 1 || snapshot.EarliestFutureDelivery == nil || !snapshot.EarliestFutureDelivery.Equal(futureDelivery) {
		t.Fatalf("future delivery diagnostics count=%d earliest=%v", snapshot.FutureDeliveries, snapshot.EarliestFutureDelivery)
	}
	status, reasons := OperationalStatus(snapshot, "running", now)
	if status != "degraded" || !containsString(reasons, "provider failures") || !containsString(reasons, "digest backlog") || !containsString(reasons, "clock-skewed future deliveries") {
		t.Fatalf("status=%q reasons=%v", status, reasons)
	}
}

func TestDiagnosticsTreatsNullDueTimesAsImmediatelyDue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "diagnostics-null@example.com", "hash", "member", "UTC", "diagnostics-null")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "diagnostics-null-due", Name: "Diagnostics Null Due"})
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, userID, artist.ID); err != nil || !added {
		t.Fatalf("follow diagnostics artist: added=%v err=%v", added, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=NULL,spotify_next_check_at=NULL WHERE id=?`, artist.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DueSyncArtists != 1 || snapshot.OldestDueSyncAt == nil {
		t.Fatalf("due sync diagnostics count=%d oldest=%v", snapshot.DueSyncArtists, snapshot.OldestDueSyncAt)
	}
	if snapshot.OldestDueSyncAt.After(snapshot.CheckedAt) || snapshot.CheckedAt.Sub(*snapshot.OldestDueSyncAt) > time.Second {
		t.Fatalf("null due time=%v, want approximately diagnostic check time %v", snapshot.OldestDueSyncAt, snapshot.CheckedAt)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

// TestLogLossAndContentionReasonsClearOnceTheyStop is #267. Both reasons added
// in v0.58.0 are driven by counters that only ever increase for the lifetime of
// the process, with no reset and no age window - unlike every other reason in
// OperationalStatus, which is either current state or age-thresholded. One
// dropped log record at 03:00 therefore left the instance reading degraded until
// someone restarted it, which teaches an operator to ignore the banner.
func TestLogLossAndContentionReasonsClearOnceTheyStop(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-5 * time.Minute)
	old := now.Add(-8 * time.Hour)

	t.Run("recent loss still degrades", func(t *testing.T) {
		status, reasons := OperationalStatus(DiagnosticsSnapshot{
			CheckedAt: now, DatabaseHealthy: true,
			DroppedLogEntries: 7, LastLogLossAt: &recent,
		}, "running", now)
		if status != "degraded" || !slices.Contains(reasons, "application log loss") {
			t.Fatalf("status=%q reasons=%v, want degraded with a log loss reason", status, reasons)
		}
	})

	t.Run("stale loss no longer degrades", func(t *testing.T) {
		_, reasons := OperationalStatus(DiagnosticsSnapshot{
			CheckedAt: now, DatabaseHealthy: true,
			DroppedLogEntries: 7, LastLogLossAt: &old,
		}, "running", now)
		if slices.Contains(reasons, "application log loss") {
			t.Fatal("a log drop from eight hours ago still pins the instance to degraded")
		}
	})

	t.Run("recent contention still degrades", func(t *testing.T) {
		status, reasons := OperationalStatus(DiagnosticsSnapshot{
			CheckedAt: now, DatabaseHealthy: true,
			WriteRetryExhaustions: 1, LastWriteContentionAt: &recent,
		}, "running", now)
		if status != "degraded" || !slices.Contains(reasons, "database write contention") {
			t.Fatalf("status=%q reasons=%v, want degraded with a contention reason", status, reasons)
		}
	})

	t.Run("stale contention no longer degrades", func(t *testing.T) {
		_, reasons := OperationalStatus(DiagnosticsSnapshot{
			CheckedAt: now, DatabaseHealthy: true,
			WriteRetryExhaustions: 1, LastWriteContentionAt: &old,
		}, "running", now)
		if slices.Contains(reasons, "database write contention") {
			t.Fatal("a refused write from eight hours ago still pins the instance to degraded")
		}
	})

	// A snapshot assembled before the timestamps existed must not silently hide
	// a real problem.
	t.Run("a counter with no instant is treated as current", func(t *testing.T) {
		_, reasons := OperationalStatus(DiagnosticsSnapshot{
			CheckedAt: now, DatabaseHealthy: true, DroppedLogEntries: 3,
		}, "running", now)
		if !slices.Contains(reasons, "application log loss") {
			t.Fatal("a loss counter with no recorded instant was ignored")
		}
	})
}
