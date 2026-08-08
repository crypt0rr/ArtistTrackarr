package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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
		response = append(response, providerHealthPayloadFor(provider))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Debug("provider health response interrupted", "error", err)
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
