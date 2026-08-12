package web

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/go-chi/chi/v5"
)

func (a *App) notificationHoldAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	action := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "action")))
	if action != "notify" && action != "discard" {
		http.Error(w, "invalid notification hold action", http.StatusBadRequest)
		return
	}
	if err := a.store.ResolveNotificationHold(r.Context(), session.User.ID, id, action); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, store.ErrInvalidNotificationHoldAction) {
			http.Error(w, "invalid notification hold action", http.StatusBadRequest)
			return
		}
		a.logger.Error("notification hold action failed", "path", r.URL.Path,
			"user_id", session.User.ID, "hold_id", id, "action", action, "error", err)
		http.Error(w, "notification hold could not be updated", http.StatusInternalServerError)
		return
	}
	redirect := localReturnPath(r.FormValue("return"), "/", "/")
	message := "Notification released"
	if action == "discard" {
		message = "Notification discarded"
	}
	separator := "?"
	if strings.Contains(redirect, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirect+separator+"message="+url.QueryEscape(message), http.StatusSeeOther)
}
