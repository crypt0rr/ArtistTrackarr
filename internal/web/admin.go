package web

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/crypt0rr/artist-tracker/internal/version"
)

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	d := a.adminData(r)
	status := http.StatusOK
	if d.Error != "" {
		status = http.StatusInternalServerError
	}
	a.render(w, "admin", d, status)
}

func (a *App) adminDeliveryDetail(w http.ResponseWriter, r *http.Request) {
	// Notification bodies and provider errors are deliberately only fetched
	// after this explicit request. Keep the page out of browser/proxy caches
	// because it contains household-private delivery content.
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	detail, err := a.store.AdminDeliveryDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		d := a.data(r, "Delivery details")
		a.pageStoreError(r, &d, "Delivery details", "notification delivery", err)
		a.render(w, "admin_delivery", d, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin delivery details viewed", "delivery_id", id)
	d := a.data(r, "Delivery details")
	d.AdminDelivery = &detail
	a.render(w, "admin_delivery", d, http.StatusOK)
}

func (a *App) providerHealth(w http.ResponseWriter, r *http.Request) {
	health, err := a.store.ProviderHealth(r.Context())
	if err != nil {
		a.logger.Error("provider health lookup failed", "page", "Household administration", "operation", "provider health", "path", r.URL.Path, "error", err)
		http.Error(w, "provider health unavailable", http.StatusInternalServerError)
		return
	}
	response := make([]providerHealthPayload, 0, len(health))
	timezone := ""
	if session, ok := currentSession(r); ok {
		timezone = session.User.Timezone
	}
	for _, provider := range health {
		response = append(response, providerHealthPayloadForConfig(provider, a.cfg, timezone))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Debug("provider health response interrupted", "error", err)
	}
}

func (a *App) diagnostics(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.store.Diagnostics(r.Context())
	if err != nil {
		a.logger.Error("system diagnostics failed", "path", r.URL.Path, "error", err)
		http.Error(w, "diagnostics unavailable", http.StatusInternalServerError)
		return
	}
	var runner jobs.RunnerStatus
	if a.jobs != nil {
		runner = a.jobs.Status()
	}
	session, _ := currentSession(r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="artisttrackarr-diagnostics.txt"`)
	if _, err := io.WriteString(w, diagnosticReport(snapshot, runner, session.User.Timezone)); err != nil {
		a.logger.Debug("system diagnostics response interrupted", "error", err)
	}
}

// diagnosticsJSON exposes the same redacted operational view as the support
// report, but in a stable machine-readable shape for household monitoring.
// It is deliberately admin-only and contains no provider error text,
// destination URLs, credentials, or notification bodies.
func (a *App) diagnosticsJSON(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	snapshot, err := a.store.Diagnostics(r.Context())
	if err != nil {
		a.logger.Error("system diagnostics JSON failed", "path", r.URL.Path, "error", err)
		http.Error(w, "diagnostics unavailable", http.StatusInternalServerError)
		return
	}
	retention, err := a.store.RetentionReport(r.Context(), now)
	if err != nil {
		a.logger.Error("retention diagnostics JSON failed", "path", r.URL.Path, "error", err)
		http.Error(w, "diagnostics unavailable", http.StatusInternalServerError)
		return
	}
	var runner jobs.RunnerStatus
	if a.jobs != nil {
		runner = a.jobs.Status()
	}
	runnerState := "unknown"
	if a.jobs != nil {
		runnerState = "stopped"
		if runner.Running {
			runnerState = "running"
		}
	}
	status, reasons := store.OperationalStatus(snapshot, runnerState, now)
	payload := diagnosticsJSONPayload{
		Version:            version.Current,
		GeneratedAt:        now,
		CheckedAt:          snapshot.CheckedAt,
		OperationalStatus:  status,
		OperationalReasons: reasons,
		Database: diagnosticsJSONDatabase{
			Healthy:       snapshot.DatabaseHealthy,
			State:         string(snapshot.DatabaseHealthState),
			Schema:        snapshot.SchemaVersion,
			SizeBytes:     snapshot.DatabaseBytes,
			FreeBytes:     snapshot.DatabaseFreeBytes,
			LastBackupAt:  snapshot.LastBackupAt,
			LastRestoreAt: snapshot.LastRestoreAt,
			RestoreResult: snapshot.LastRestoreResult,
		},
		Inventory: diagnosticsJSONInventory{
			FollowedArtists:         snapshot.FollowedArtists,
			Releases:                snapshot.Releases,
			RecentLogEntries:        snapshot.RecentLogEntries,
			ProviderFailures:        snapshot.ProviderFailures,
			OldestProviderFailureAt: snapshot.OldestProviderFailureAt,
		},
		Queue: diagnosticsJSONQueue{
			QueuedSyncs:            snapshot.QueuedSyncs,
			RunningSyncs:           snapshot.RunningSyncs,
			DueSyncArtists:         snapshot.DueSyncArtists,
			OldestDueSyncAt:        snapshot.OldestDueSyncAt,
			PendingDeliveries:      snapshot.PendingDeliveries,
			FailedDeliveries:       snapshot.FailedDeliveries,
			DigestBacklog:          snapshot.DigestBacklog,
			OldestDigestBacklogAt:  snapshot.OldestDigestBacklogAt,
			OldestQueueAt:          snapshot.OldestQueueAt,
			FutureDeliveries:       snapshot.FutureDeliveries,
			EarliestFutureDelivery: snapshot.EarliestFutureDelivery,
			StaleClaims:            snapshot.StaleClaims,
		},
		Destinations: diagnosticsJSONDestinations{
			Paused: snapshot.PausedDestinations,
		},
		Providers: make([]diagnosticsJSONProvider, 0, len(snapshot.Providers)),
		Retention: diagnosticsJSONRetention{
			CheckedAt:               retention.CheckedAt,
			HistoryReviewDue:        retention.HistoryReviewDue,
			HistoryAgeDays:          retention.HistoryAgeDays,
			HistoryReviewDays:       retention.Policy.HistoryReviewDays,
			NotificationEvents:      retention.NotificationEvents,
			Deliveries:              retention.Deliveries,
			DeliveryAttempts:        retention.DeliveryAttempts,
			PrunableApplicationLogs: retention.PrunableApplicationLogs,
			PrunableTransientRows:   retention.PrunableTransientSessions + retention.PrunableAuthTokens + retention.PrunableLoginAttempts + retention.PrunableManualSyncs + retention.PrunableImportJobs,
		},
		Runner: diagnosticsJSONRunner{
			State:                    runnerState,
			LastActivityAt:           runner.LastActivityAt,
			WakeSignals:              runner.Metrics.WakeSignals,
			TaskOverlaps:             runner.Metrics.TaskOverlaps,
			TaskPanics:               runner.Metrics.TaskPanics,
			SyncRuns:                 runner.Metrics.SyncRuns,
			SyncDue:                  runner.Metrics.SyncDue,
			SyncSucceeded:            runner.Metrics.SyncSucceeded,
			SyncFailed:               runner.Metrics.SyncFailed,
			ResolutionRuns:           runner.Metrics.ResolutionRuns,
			ResolutionItems:          runner.Metrics.ResolutionItems,
			ReleaseDayRuns:           runner.Metrics.ReleaseDayRuns,
			MaintenanceRuns:          runner.Metrics.MaintenanceRuns,
			DeliveryBatches:          runner.Metrics.DeliveryBatches,
			DeliveryAttempted:        runner.Metrics.DeliveryAttempted,
			DeliverySent:             runner.Metrics.DeliverySent,
			DeliveryFailed:           runner.Metrics.DeliveryFailed,
			SpotifyCooldownSkips:     runner.Metrics.SpotifyCooldownSkips,
			ITunesCooldownSkips:      runner.Metrics.ITunesCooldownSkips,
			MusicBrainzCooldownSkips: runner.Metrics.MusicBrainzCooldownSkips,
		},
	}
	for _, provider := range snapshot.Providers {
		payload.Providers = append(payload.Providers, diagnosticsJSONProvider{
			Name:        provider.Provider,
			Status:      provider.Status,
			NextCheckAt: provider.NextCheckAt,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		a.logger.Debug("system diagnostics JSON response interrupted", "error", err)
	}
}

type diagnosticsJSONPayload struct {
	Version            string                      `json:"version"`
	GeneratedAt        time.Time                   `json:"generated_at"`
	CheckedAt          time.Time                   `json:"checked_at"`
	OperationalStatus  string                      `json:"operational_status"`
	OperationalReasons []string                    `json:"operational_reasons,omitempty"`
	Database           diagnosticsJSONDatabase     `json:"database"`
	Inventory          diagnosticsJSONInventory    `json:"inventory"`
	Queue              diagnosticsJSONQueue        `json:"queue"`
	Destinations       diagnosticsJSONDestinations `json:"destinations"`
	Providers          []diagnosticsJSONProvider   `json:"providers"`
	Retention          diagnosticsJSONRetention    `json:"retention"`
	Runner             diagnosticsJSONRunner       `json:"runner"`
}

type diagnosticsJSONDatabase struct {
	Healthy       bool       `json:"healthy"`
	State         string     `json:"state"`
	Schema        int        `json:"schema"`
	SizeBytes     int64      `json:"size_bytes"`
	FreeBytes     int64      `json:"free_bytes"`
	LastBackupAt  *time.Time `json:"last_backup_at,omitempty"`
	LastRestoreAt *time.Time `json:"last_restore_at,omitempty"`
	RestoreResult string     `json:"restore_result,omitempty"`
}

type diagnosticsJSONQueue struct {
	QueuedSyncs            int        `json:"queued_syncs"`
	RunningSyncs           int        `json:"running_syncs"`
	DueSyncArtists         int        `json:"due_sync_artists"`
	OldestDueSyncAt        *time.Time `json:"oldest_due_sync_at,omitempty"`
	PendingDeliveries      int        `json:"pending_deliveries"`
	FailedDeliveries       int        `json:"failed_deliveries"`
	DigestBacklog          int        `json:"digest_backlog"`
	OldestDigestBacklogAt  *time.Time `json:"oldest_digest_backlog_at,omitempty"`
	OldestQueueAt          *time.Time `json:"oldest_queue_at,omitempty"`
	FutureDeliveries       int        `json:"future_deliveries"`
	EarliestFutureDelivery *time.Time `json:"earliest_future_delivery,omitempty"`
	StaleClaims            int        `json:"stale_claims"`
}

type diagnosticsJSONInventory struct {
	FollowedArtists         int        `json:"followed_artists"`
	Releases                int        `json:"releases"`
	RecentLogEntries        int        `json:"recent_log_entries"`
	ProviderFailures        int        `json:"provider_failures"`
	OldestProviderFailureAt *time.Time `json:"oldest_provider_failure_at,omitempty"`
}

type diagnosticsJSONDestinations struct {
	Paused int `json:"paused"`
}

type diagnosticsJSONProvider struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	NextCheckAt *time.Time `json:"next_check_at,omitempty"`
}

type diagnosticsJSONRetention struct {
	CheckedAt               time.Time `json:"checked_at"`
	HistoryReviewDue        bool      `json:"history_review_due"`
	HistoryAgeDays          int       `json:"history_age_days"`
	HistoryReviewDays       int       `json:"history_review_days"`
	NotificationEvents      int64     `json:"notification_events"`
	Deliveries              int64     `json:"deliveries"`
	DeliveryAttempts        int64     `json:"delivery_attempts"`
	PrunableApplicationLogs int64     `json:"prunable_application_logs"`
	PrunableTransientRows   int64     `json:"prunable_transient_rows"`
}

type diagnosticsJSONRunner struct {
	State                    string     `json:"state"`
	LastActivityAt           *time.Time `json:"last_activity_at,omitempty"`
	WakeSignals              uint64     `json:"wake_signals"`
	TaskOverlaps             uint64     `json:"task_overlaps"`
	TaskPanics               uint64     `json:"task_panics"`
	SyncRuns                 uint64     `json:"sync_runs"`
	SyncDue                  uint64     `json:"sync_due"`
	SyncSucceeded            uint64     `json:"sync_succeeded"`
	SyncFailed               uint64     `json:"sync_failed"`
	ResolutionRuns           uint64     `json:"resolution_runs"`
	ResolutionItems          uint64     `json:"resolution_items"`
	ReleaseDayRuns           uint64     `json:"release_day_runs"`
	MaintenanceRuns          uint64     `json:"maintenance_runs"`
	DeliveryBatches          uint64     `json:"delivery_batches"`
	DeliveryAttempted        uint64     `json:"delivery_attempted"`
	DeliverySent             uint64     `json:"delivery_sent"`
	DeliveryFailed           uint64     `json:"delivery_failed"`
	SpotifyCooldownSkips     uint64     `json:"spotify_cooldown_skips"`
	ITunesCooldownSkips      uint64     `json:"itunes_cooldown_skips"`
	MusicBrainzCooldownSkips uint64     `json:"musicbrainz_cooldown_skips"`
}

func (a *App) exportAdminDeliveryHistory(w http.ResponseWriter, r *http.Request) {
	const pageSize = 500
	if _, err := a.store.AdminDeliveryHistoryCount(r.Context()); err != nil {
		a.logger.Error("delivery audit export preflight failed", "path", r.URL.Path, "error", err)
		http.Error(w, "delivery audit unavailable", http.StatusInternalServerError)
		return
	}
	payload, err := buildBufferedCSV(func(writer *csv.Writer) error {
		if err := writer.Write([]string{"delivery_id", "user_email", "title", "body", "event_type", "destination", "service", "status", "attempts", "last_error", "created_at", "next_attempt_at", "sent_at"}); err != nil {
			return err
		}
		var cursor *store.AdminDeliveryExportCursor
		for {
			rows, next, err := a.store.AdminDeliveryHistoryExportPage(r.Context(), pageSize, cursor)
			if err != nil {
				return fmt.Errorf("delivery audit page lookup: %w", err)
			}
			for _, item := range rows {
				row := []string{
					strconv.FormatInt(item.DeliveryID, 10), item.UserEmail, item.Title, item.Body,
					item.EventType, item.Destination, item.Service, item.Status,
					strconv.Itoa(item.Attempts), item.LastError,
					item.CreatedAt.Format(time.RFC3339Nano), formatNullableTime(item.NextAttempt), formatNullableTime(item.SentAt),
				}
				if err := writer.Write(neutralizeCSVRow(row)); err != nil {
					return err
				}
			}
			if len(rows) < pageSize || next == nil {
				break
			}
			cursor = next
		}
		return nil
	})
	if err != nil {
		a.logger.Error("delivery audit export failed", "path", r.URL.Path, "error", err)
		http.Error(w, "delivery audit unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="artisttrackarr-delivery-audit.csv"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	if _, err := w.Write(payload); err != nil {
		a.logger.Debug("delivery audit export response interrupted", "error", err)
	}
}

func formatNullableTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func (a *App) adminData(r *http.Request) PageData {
	const pageSize = 50
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	d := a.data(r, "Household administration")
	failed := false
	count, err := a.store.AdminDeliveryHistoryCount(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "delivery history count", err) || failed
	pages := (count + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	d.AppLogs, err = a.store.ApplicationLogs(r.Context(), 200)
	failed = a.pageStoreError(r, &d, "Household administration", "application logs", err) || failed
	if len(d.AppLogs) == 0 {
		if snapshotter, ok := a.logger.Handler().(interface{ Snapshot() []logging.Entry }); ok {
			d.AppLogs = snapshotter.Snapshot()
		}
	}
	d.AdminUsers, err = a.store.AdminUsers(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "household users", err) || failed
	d.AdminArtists, err = a.store.AdminArtists(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "followed artists", err) || failed
	d.ProviderHealth, err = a.store.ProviderHealth(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "provider health", err) || failed
	d.Diagnostics, err = a.store.Diagnostics(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "system diagnostics", err) || failed
	d.Retention, err = a.store.RetentionReport(r.Context(), time.Now().UTC())
	failed = a.pageStoreError(r, &d, "Household administration", "retention report", err) || failed
	if a.jobs != nil {
		d.RunnerStatus = a.jobs.Status()
	}
	runnerState := "unknown"
	if a.jobs != nil {
		runnerState = "stopped"
		if d.RunnerStatus.Running {
			runnerState = "running"
		}
	}
	d.OperationalStatus, d.OperationalReasons = store.OperationalStatus(d.Diagnostics, runnerState, time.Now().UTC())
	d.OperationalSnapshots, err = a.store.OperationalSnapshots(r.Context(), 24)
	failed = a.pageStoreError(r, &d, "Household administration", "operational snapshot history", err) || failed
	if d.User != nil {
		d.DiagnosticReport = diagnosticReport(d.Diagnostics, d.RunnerStatus, d.User.Timezone)
	} else {
		d.DiagnosticReport = diagnosticReport(d.Diagnostics, d.RunnerStatus, "UTC")
	}
	d.AdminDestinationHealth, err = a.store.AdminDestinationHealth(r.Context())
	failed = a.pageStoreError(r, &d, "Household administration", "destination health", err) || failed
	d.ManualSyncs, err = a.store.ManualSyncRequests(r.Context(), 20)
	failed = a.pageStoreError(r, &d, "Household administration", "manual sync history", err) || failed
	d.AdminHistory, err = a.store.AdminDeliveryHistorySummary(r.Context(), pageSize, (page-1)*pageSize)
	failed = a.pageStoreError(r, &d, "Household administration", "delivery audit", err) || failed
	if failed && d.Error == "" {
		d.Error = "We couldn't load this page right now. Please try again."
	}
	d.AdminPage, d.AdminPages = page, pages
	if page > 1 {
		d.AdminPrevPage = page - 1
	}
	if page < pages {
		d.AdminNextPage = page + 1
	}
	return d
}

func (a *App) cleanupRetention(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.FormValue("confirm")) != "cleanup" {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape("Cleanup was not confirmed; no records were removed."), http.StatusSeeOther)
		return
	}
	stats, err := a.store.CleanupRetention(r.Context(), time.Now().UTC())
	if err != nil {
		a.logger.Error("retention cleanup failed", "path", r.URL.Path, "error", err)
		http.Redirect(w, r, "/admin?message="+url.QueryEscape("Retention cleanup could not be completed."), http.StatusSeeOther)
		return
	}
	removed := stats.ApplicationLogs + stats.Sessions + stats.AuthTokens + stats.LoginAttempts + stats.ManualSyncs + stats.ImportJobs
	a.logger.Info("retention cleanup completed", "removed", removed,
		"application_logs", stats.ApplicationLogs, "sessions", stats.Sessions,
		"auth_tokens", stats.AuthTokens, "login_attempts", stats.LoginAttempts,
		"manual_syncs", stats.ManualSyncs, "import_jobs", stats.ImportJobs,
		"wal_checkpointed", stats.WALCheckpointed, "wal_checkpoint_busy", stats.WALCheckpointBusy,
		"wal_checkpoint_error", stats.WALCheckpointError)
	message := fmt.Sprintf("Retention cleanup removed %d transient records; notification and delivery history was preserved.", removed)
	if stats.WALCheckpointed {
		message += " WAL space was checkpointed; freelist pages remain reusable and VACUUM is not run automatically."
	} else if stats.WALCheckpointBusy {
		message += " WAL truncation was deferred because the database was busy; freelist pages remain reusable."
	} else {
		message += " Database file size was not compacted; freelist pages remain reusable and VACUUM is not run automatically."
	}
	http.Redirect(w, r, "/admin?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func diagnosticReport(snapshot store.DiagnosticsSnapshot, runner jobs.RunnerStatus, timezone string) string {
	var report strings.Builder
	runnerState := "stopped"
	if runner.Running {
		runnerState = "running"
	}
	status, reasons := store.OperationalStatus(snapshot, runnerState, snapshot.CheckedAt)
	report.WriteString("ArtistTrackarr release assurance report\n")
	if strings.TrimSpace(timezone) == "" {
		timezone = "UTC"
	}
	fmt.Fprintf(&report, "Timezone: %s\n", timezone)
	fmt.Fprintf(&report, "Generated: %s\n", providerHealthTime(snapshot.CheckedAt, timezone))
	fmt.Fprintf(&report, "Operational status: %s\n", store.DiagnosticStatusLabel(status))
	if len(reasons) > 0 {
		fmt.Fprintf(&report, "Operational reasons: %s\n", strings.Join(reasons, ", "))
	}
	databaseState := string(snapshot.DatabaseHealthState)
	if databaseState == "" {
		if snapshot.DatabaseHealthy {
			databaseState = string(store.DatabaseHealthy)
		} else {
			databaseState = string(store.DatabaseUnavailable)
		}
	}
	fmt.Fprintf(&report, "Database: %s (schema %d)\n", databaseState, snapshot.SchemaVersion)
	fmt.Fprintf(&report, "Followed artists: %d\n", snapshot.FollowedArtists)
	fmt.Fprintf(&report, "Known releases: %d\n", snapshot.Releases)
	fmt.Fprintf(&report, "Queued syncs: %d\n", snapshot.QueuedSyncs)
	fmt.Fprintf(&report, "Running syncs: %d\n", snapshot.RunningSyncs)
	fmt.Fprintf(&report, "Due artist syncs: %d\n", snapshot.DueSyncArtists)
	if snapshot.OldestDueSyncAt != nil {
		fmt.Fprintf(&report, "Oldest due artist sync: %s\n", providerHealthTime(snapshot.OldestDueSyncAt, timezone))
	}
	fmt.Fprintf(&report, "Pending deliveries: %d\n", snapshot.PendingDeliveries)
	fmt.Fprintf(&report, "Clock-skewed future deliveries: %d\n", snapshot.FutureDeliveries)
	if snapshot.EarliestFutureDelivery != nil {
		fmt.Fprintf(&report, "Earliest clock-skewed delivery: %s\n", providerHealthTime(snapshot.EarliestFutureDelivery, timezone))
	}
	fmt.Fprintf(&report, "Digest backlog: %d\n", snapshot.DigestBacklog)
	fmt.Fprintf(&report, "Failed deliveries: %d\n", snapshot.FailedDeliveries)
	fmt.Fprintf(&report, "Stale work claims: %d\n", snapshot.StaleClaims)
	fmt.Fprintf(&report, "Paused destinations: %d\n", snapshot.PausedDestinations)
	fmt.Fprintf(&report, "Provider failures: %d\n", snapshot.ProviderFailures)
	if snapshot.OldestProviderFailureAt != nil {
		fmt.Fprintf(&report, "Oldest provider failure: %s\n", providerHealthTime(snapshot.OldestProviderFailureAt, timezone))
	}
	if snapshot.OldestDigestBacklogAt != nil {
		fmt.Fprintf(&report, "Oldest digest backlog: %s\n", providerHealthTime(snapshot.OldestDigestBacklogAt, timezone))
	}
	fmt.Fprintf(&report, "Database size: %d bytes; reusable space: %d bytes\n", snapshot.DatabaseBytes, snapshot.DatabaseFreeBytes)
	if snapshot.OldestQueueAt != nil {
		fmt.Fprintf(&report, "Oldest queued delivery: %s\n", providerHealthTime(snapshot.OldestQueueAt, timezone))
	}
	if snapshot.LastBackupAt != nil {
		fmt.Fprintf(&report, "Last backup: %s\n", providerHealthTime(snapshot.LastBackupAt, timezone))
	} else {
		fmt.Fprintln(&report, "Last backup: not yet established")
	}
	if snapshot.LastRestoreAt != nil {
		fmt.Fprintf(&report, "Last restore rehearsal: %s (%s)\n", providerHealthTime(snapshot.LastRestoreAt, timezone), snapshot.LastRestoreResult)
	} else {
		fmt.Fprintln(&report, "Last restore rehearsal: not recorded")
	}
	fmt.Fprintf(&report, "Application events (24h): %d\n", snapshot.RecentLogEntries)
	fmt.Fprintf(&report, "Scheduler: %s\n", diagnosticHealthLabel(runner.Running))
	if runner.LastActivityAt != nil {
		fmt.Fprintf(&report, "Scheduler last activity: %s\n", providerHealthTime(runner.LastActivityAt, timezone))
	}
	fmt.Fprintf(&report, "Scheduler wakes: %d; overlaps: %d; recovered panics: %d\n",
		runner.Metrics.WakeSignals, runner.Metrics.TaskOverlaps, runner.Metrics.TaskPanics)
	fmt.Fprintf(&report, "Sync runs: %d; due: %d; succeeded: %d; failed: %d\n",
		runner.Metrics.SyncRuns, runner.Metrics.SyncDue, runner.Metrics.SyncSucceeded, runner.Metrics.SyncFailed)
	fmt.Fprintf(&report, "Delivery batches: %d; attempted: %d; sent: %d; failed: %d\n",
		runner.Metrics.DeliveryBatches, runner.Metrics.DeliveryAttempted,
		runner.Metrics.DeliverySent, runner.Metrics.DeliveryFailed)
	if runner.Metrics.DeliveryBatches > 0 {
		fmt.Fprintf(&report, "Delivery average batch duration: %s\n",
			runner.Metrics.DeliveryLatency/time.Duration(runner.Metrics.DeliveryBatches))
	}
	fmt.Fprintf(&report, "Provider cooldown skips: Spotify %d; iTunes %d; MusicBrainz %d\n",
		runner.Metrics.SpotifyCooldownSkips, runner.Metrics.ITunesCooldownSkips,
		runner.Metrics.MusicBrainzCooldownSkips)
	report.WriteString("Providers:\n")
	for _, provider := range snapshot.Providers {
		fmt.Fprintf(&report, "- %s: %s", provider.Provider, provider.Status)
		if provider.NextCheckAt != nil {
			fmt.Fprintf(&report, "; next check %s", providerHealthTime(provider.NextCheckAt, timezone))
		}
		report.WriteByte('\n')
	}
	return report.String()
}

func diagnosticHealthLabel(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unavailable"
}

func databaseHealthLabel(snapshot store.DiagnosticsSnapshot) string {
	switch snapshot.DatabaseHealthState {
	case store.DatabaseHealthy:
		return "healthy"
	case store.DatabaseReadOnly:
		return "read-only"
	case store.DatabaseFull:
		return "full"
	case store.DatabaseWriteFailed:
		return "write failed"
	case store.DatabaseUnavailable:
		return "unavailable"
	default:
		if snapshot.DatabaseHealthy {
			return "healthy"
		}
		return "unavailable"
	}
}
func (a *App) queueRetrySync(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	if _, err := a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "retry", nil); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if a.jobs != nil {
		a.jobs.Wake()
	}
	http.Redirect(w, r, "/admin?message=Retry+sync+queued", http.StatusSeeOther)
}
func (a *App) queueArtistSync(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err = a.store.ArtistByID(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("admin artist lookup failed", "page", "Household administration", "path", r.URL.Path, "artist_id", id, "error", err)
		http.Error(w, "could not load this artist", http.StatusInternalServerError)
		return
	}
	if _, err = a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "artist", &id); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if a.jobs != nil {
		a.jobs.Wake()
	}
	http.Redirect(w, r, "/admin?message=Artist+sync+queued", http.StatusSeeOther)
}
func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	userID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || userID < 1 {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteUser(r.Context(), session.User.ID, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, store.ErrCannotDeleteSelf) || errors.Is(err, store.ErrLastAdmin) {
			d := a.adminData(r)
			d.Error = err.Error()
			a.render(w, "admin", d, http.StatusBadRequest)
			return
		}
		a.logger.Error("delete user failed", "acting_user_id", session.User.ID, "user_id", userID, "error", err)
		http.Error(w, "user could not be deleted", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?message=User+deleted", http.StatusSeeOther)
}
func (a *App) createInvite(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, "a valid email is required", http.StatusBadRequest)
		return
	}
	if _, err := a.store.UserByEmail(r.Context(), email); err == nil {
		http.Error(w, "that user already exists", http.StatusConflict)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		a.logger.Error("invite user lookup failed", "page", "Household administration", "path", r.URL.Path, "error", err)
		http.Error(w, "could not create invitation", http.StatusInternalServerError)
		return
	}
	raw, err := a.store.CreateAuthToken(r.Context(), "invite", email, nil, session.User.ID, 48*time.Hour)
	if err != nil {
		http.Error(w, "could not create invitation", http.StatusBadRequest)
		return
	}
	d := a.adminData(r)
	d.GeneratedURL = a.cfg.PublicURL.ResolveReference(&url.URL{Path: "/invite/" + raw}).String()
	d.TokenKind, d.TokenEmail = "Invitation", strings.TrimSpace(r.FormValue("email"))
	status := http.StatusOK
	if d.Error != "" {
		status = http.StatusInternalServerError
	}
	a.render(w, "admin", d, status)
}
func (a *App) createReset(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	user, err := a.store.UserByEmail(r.Context(), r.FormValue("email"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		a.logger.Error("reset user lookup failed", "page", "Household administration", "path", r.URL.Path, "error", err)
		http.Error(w, "could not create reset", http.StatusInternalServerError)
		return
	}
	raw, err := a.store.CreateAuthToken(r.Context(), "reset", user.Email, &user.ID, session.User.ID, time.Hour)
	if err != nil {
		http.Error(w, "could not create reset", http.StatusBadRequest)
		return
	}
	d := a.adminData(r)
	d.GeneratedURL = a.cfg.PublicURL.ResolveReference(&url.URL{Path: "/reset/" + raw}).String()
	d.TokenKind, d.TokenEmail = "Password reset", user.Email
	status := http.StatusOK
	if d.Error != "" {
		status = http.StatusInternalServerError
	}
	a.render(w, "admin", d, status)
}
