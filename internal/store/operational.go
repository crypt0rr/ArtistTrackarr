package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const (
	operationalSnapshotRetention = 30 * 24 * time.Hour
	operationalSnapshotLimit     = 1000
	operationalQueueWarnAfter    = 15 * time.Minute
	operationalBackupWarnAfter   = 7 * 24 * time.Hour
)

// OperationalStatus classifies safe, non-sensitive health signals for an
// administrator or readiness probe. Degraded means the database is available
// but background work needs attention; it is deliberately not a database
// failure and therefore should not cause a container restart by itself.
func OperationalStatus(snapshot DiagnosticsSnapshot, runnerStatus string, now time.Time) (string, []string) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !snapshot.DatabaseHealthy {
		return "unavailable", []string{"database unavailable"}
	}
	status := "healthy"
	reasons := make([]string, 0, 6)
	addReason := func(reason string) {
		status = "degraded"
		reasons = append(reasons, reason)
	}
	switch strings.ToLower(strings.TrimSpace(runnerStatus)) {
	case "stopped":
		addReason("scheduler stopped")
	case "unknown", "":
		// Lightweight store fixtures and the unauthenticated readiness probe
		// may not have a runner. Do not report a false failure in that case.
	}
	if snapshot.StaleClaims > 0 {
		addReason("stale work claims")
	}
	if snapshot.PausedDestinations > 0 {
		addReason("paused destinations")
	}
	if snapshot.ProviderFailures > 0 {
		addReason("provider failures")
	}
	if snapshot.DigestBacklog > 0 {
		addReason("digest backlog")
	}
	if snapshot.OldestQueueAt != nil && now.Sub(snapshot.OldestQueueAt.UTC()) >= operationalQueueWarnAfter {
		addReason("delivery queue age")
	}
	if snapshot.LastBackupAt == nil {
		addReason("backup not recorded")
	} else if now.Sub(snapshot.LastBackupAt.UTC()) >= operationalBackupWarnAfter {
		addReason("backup overdue")
	}
	return status, reasons
}

// RecordOperationalSnapshot persists one redacted diagnostics point and keeps
// only a bounded recent history. It is called by hourly maintenance, so this
// table cannot grow with users, artists, releases, or delivery volume.
func (s *Store) RecordOperationalSnapshot(ctx context.Context, snapshot DiagnosticsSnapshot, status, runnerStatus string) error {
	status = normalizeOperationalStatus(status)
	runnerStatus = normalizeRunnerStatus(runnerStatus)
	captured := snapshot.CheckedAt.UTC()
	if captured.IsZero() {
		captured = time.Now().UTC()
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO operational_snapshots
		(captured_at,status,runner_status,database_healthy,schema_version,followed_artists,releases,
		 queued_syncs,running_syncs,pending_deliveries,failed_deliveries,recent_log_entries,
		 oldest_queue_at,stale_claims,paused_destinations,provider_failures,digest_backlog,database_bytes,
		 last_backup_at,last_restore_at,last_restore_result)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			timeText(captured), status, runnerStatus, boolInt(snapshot.DatabaseHealthy), snapshot.SchemaVersion,
			snapshot.FollowedArtists, snapshot.Releases, snapshot.QueuedSyncs, snapshot.RunningSyncs,
			snapshot.PendingDeliveries, snapshot.FailedDeliveries, snapshot.RecentLogEntries,
			nullableTime(snapshot.OldestQueueAt), snapshot.StaleClaims, snapshot.PausedDestinations,
			snapshot.ProviderFailures, snapshot.DigestBacklog, snapshot.DatabaseBytes,
			nullableTime(snapshot.LastBackupAt), nullableTime(snapshot.LastRestoreAt), snapshot.LastRestoreResult)
		if err != nil {
			return err
		}
		cutoff := captured.Add(-operationalSnapshotRetention)
		if _, err = tx.ExecContext(ctx, `DELETE FROM operational_snapshots WHERE captured_at < ?`, timeText(cutoff)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM operational_snapshots WHERE id NOT IN
		(SELECT id FROM operational_snapshots ORDER BY captured_at DESC,id DESC LIMIT ?)`, operationalSnapshotLimit); err != nil {
			return err
		}
		return nil
	})
}

// OperationalSnapshots returns the most recent persisted diagnostics points
// for the admin history view. The limit is deliberately bounded.
func (s *Store) OperationalSnapshots(ctx context.Context, limit int) ([]OperationalSnapshot, error) {
	if limit < 1 {
		limit = 24
	}
	if limit > operationalSnapshotLimit {
		limit = operationalSnapshotLimit
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT id,captured_at,status,runner_status,database_healthy,
		schema_version,followed_artists,releases,queued_syncs,running_syncs,pending_deliveries,
		failed_deliveries,recent_log_entries,oldest_queue_at,stale_claims,paused_destinations,
		provider_failures,digest_backlog,database_bytes,last_backup_at,last_restore_at,last_restore_result
		FROM operational_snapshots ORDER BY captured_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]OperationalSnapshot, 0, limit)
	for rows.Next() {
		var item OperationalSnapshot
		var captured, oldest, backup, restore sql.NullString
		var healthy int
		if err := rows.Scan(&item.ID, &captured, &item.Status, &item.RunnerStatus, &healthy,
			&item.SchemaVersion, &item.FollowedArtists, &item.Releases, &item.QueuedSyncs,
			&item.RunningSyncs, &item.PendingDeliveries, &item.FailedDeliveries, &item.RecentLogEntries,
			&oldest, &item.StaleClaims, &item.PausedDestinations, &item.ProviderFailures,
			&item.DigestBacklog, &item.DatabaseBytes, &backup, &restore, &item.LastRestoreResult); err != nil {
			return nil, err
		}
		item.DatabaseHealthy = healthy != 0
		if item.CapturedAt, err = parseStoredTime(captured.String, "operational snapshot captured_at"); err != nil {
			return nil, err
		}
		if item.OldestQueueAt, err = parseStoredNullableTime(oldest, "operational snapshot oldest_queue_at"); err != nil {
			return nil, err
		}
		if item.LastBackupAt, err = parseStoredNullableTime(backup, "operational snapshot last_backup_at"); err != nil {
			return nil, err
		}
		if item.LastRestoreAt, err = parseStoredNullableTime(restore, "operational snapshot last_restore_at"); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func normalizeOperationalStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy", "degraded", "unavailable":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return "degraded"
	}
}

func normalizeRunnerStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "stopped", "unknown":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return "unknown"
	}
}

// DiagnosticStatusLabel is kept in store so the persisted value and the web
// readiness presentation use the same vocabulary.
func DiagnosticStatusLabel(status string) string {
	switch normalizeOperationalStatus(status) {
	case "healthy":
		return "Healthy"
	case "unavailable":
		return "Unavailable"
	default:
		return "Degraded"
	}
}
