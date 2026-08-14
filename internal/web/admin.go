package web

import (
	"database/sql"
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
	for _, provider := range health {
		response = append(response, providerHealthPayloadForConfig(provider, a.cfg))
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
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="artisttrackarr-diagnostics.txt"`)
	if _, err := io.WriteString(w, diagnosticReport(snapshot, runner)); err != nil {
		a.logger.Debug("system diagnostics response interrupted", "error", err)
	}
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
	if a.jobs != nil {
		d.RunnerStatus = a.jobs.Status()
	}
	d.DiagnosticReport = diagnosticReport(d.Diagnostics, d.RunnerStatus)
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

func diagnosticReport(snapshot store.DiagnosticsSnapshot, runner jobs.RunnerStatus) string {
	var report strings.Builder
	report.WriteString("ArtistTrackarr release assurance report\n")
	fmt.Fprintf(&report, "Generated: %s\n", snapshot.CheckedAt.Format(time.RFC3339))
	fmt.Fprintf(&report, "Database: %s (schema %d)\n", diagnosticHealthLabel(snapshot.DatabaseHealthy), snapshot.SchemaVersion)
	fmt.Fprintf(&report, "Followed artists: %d\n", snapshot.FollowedArtists)
	fmt.Fprintf(&report, "Known releases: %d\n", snapshot.Releases)
	fmt.Fprintf(&report, "Queued syncs: %d\n", snapshot.QueuedSyncs)
	fmt.Fprintf(&report, "Running syncs: %d\n", snapshot.RunningSyncs)
	fmt.Fprintf(&report, "Pending deliveries: %d\n", snapshot.PendingDeliveries)
	fmt.Fprintf(&report, "Digest backlog: %d\n", snapshot.DigestBacklog)
	fmt.Fprintf(&report, "Failed deliveries: %d\n", snapshot.FailedDeliveries)
	fmt.Fprintf(&report, "Stale work claims: %d\n", snapshot.StaleClaims)
	fmt.Fprintf(&report, "Paused destinations: %d\n", snapshot.PausedDestinations)
	fmt.Fprintf(&report, "Provider failures: %d\n", snapshot.ProviderFailures)
	fmt.Fprintf(&report, "Database size: %d bytes\n", snapshot.DatabaseBytes)
	if snapshot.OldestQueueAt != nil {
		fmt.Fprintf(&report, "Oldest queued delivery: %s\n", snapshot.OldestQueueAt.Format(time.RFC3339))
	}
	if snapshot.LastBackupAt != nil {
		fmt.Fprintf(&report, "Last backup: %s\n", snapshot.LastBackupAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(&report, "Last backup: not recorded")
	}
	if snapshot.LastRestoreAt != nil {
		fmt.Fprintf(&report, "Last restore rehearsal: %s (%s)\n", snapshot.LastRestoreAt.Format(time.RFC3339), snapshot.LastRestoreResult)
	} else {
		fmt.Fprintln(&report, "Last restore rehearsal: not recorded")
	}
	fmt.Fprintf(&report, "Application events (24h): %d\n", snapshot.RecentLogEntries)
	fmt.Fprintf(&report, "Scheduler: %s\n", diagnosticHealthLabel(runner.Running))
	if runner.LastActivityAt != nil {
		fmt.Fprintf(&report, "Scheduler last activity: %s\n", runner.LastActivityAt.Format(time.RFC3339))
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
			fmt.Fprintf(&report, "; next check %s", provider.NextCheckAt.Format(time.RFC3339))
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
