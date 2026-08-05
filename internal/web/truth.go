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

func (a *App) releaseTruthAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
	if action == "" {
		action = "confirm"
	}
	var operationErr error
	switch action {
	case "clear":
		operationErr = a.store.ClearReleaseTruthDecision(r.Context(), session.User.ID, id)
	case "confirm":
		operationErr = a.store.SetReleaseTruthDecision(r.Context(), session.User.ID, id,
			r.FormValue("provider"), r.FormValue("reason"))
	default:
		http.Error(w, "invalid release truth action", http.StatusBadRequest)
		return
	}
	if operationErr != nil {
		switch {
		case errors.Is(operationErr, sql.ErrNoRows):
			http.NotFound(w, r)
		case errors.Is(operationErr, store.ErrInvalidReleaseTruthProvider), errors.Is(operationErr, store.ErrReleaseTruthProviderUnavailable):
			http.Error(w, "that provider is not available for this release", http.StatusBadRequest)
		default:
			a.logger.Error("release truth decision failed", "path", r.URL.Path, "user_id", session.User.ID,
				"release_id", id, "error", operationErr)
			http.Error(w, "release truth decision could not be saved", http.StatusInternalServerError)
		}
		return
	}
	message := "Release source confirmed"
	if action == "clear" {
		message = "Release source decision cleared"
	}
	redirect := "/releases/" + strconv.FormatInt(id, 10) + "?message=" + url.QueryEscape(message)
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
