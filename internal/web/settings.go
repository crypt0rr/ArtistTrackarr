package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func (a *App) destinations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	location := "/settings"
	if encoded := query.Encode(); encoded != "" {
		location += "?" + encoded
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}
func (a *App) releaseGroupArt(w http.ResponseWriter, r *http.Request) {
	asset := a.artwork.Get(r.Context(), chi.URLParam(r, "mbid"))
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(asset.MaxAge.Seconds())))
	w.Header().Set("X-Artwork-Status", asset.Status)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.Data)))
	_, _ = w.Write(asset.Data)
}
func (a *App) addDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	input := notify.DestinationInput{
		Service: r.FormValue("service"), RawURL: r.FormValue("raw_url"), Host: r.FormValue("host"),
		Port: r.FormValue("port"), Username: r.FormValue("username"), Password: r.FormValue("password"),
		Token: r.FormValue("token"), Target: r.FormValue("target"), From: r.FormValue("from"),
		To: r.FormValue("to"), Topic: r.FormValue("topic"),
	}
	serviceURL, err := notify.BuildURL(input)
	if err == nil {
		err = a.sender.Validate(serviceURL)
	}
	var encrypted []byte
	if err == nil {
		encrypted, err = a.cipher.Encrypt(serviceURL)
	}
	if err == nil {
		err = a.store.AddDestination(r.Context(), session.User.ID, r.FormValue("name"), input.Service, encrypted)
	}
	if err != nil {
		d := a.data(r, "Settings")
		d.Error = notify.RedactError(err)
		if a.loadSettingsData(r, &d) {
			a.render(w, "settings", d, http.StatusInternalServerError)
		} else {
			a.render(w, "settings", d, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/settings?message=Destination+added", http.StatusSeeOther)
}
func (a *App) testDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, parseErr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if parseErr != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	destination, err := a.store.Destination(r.Context(), session.User.ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		var serviceURL string
		serviceURL, err = a.cipher.Decrypt(destination.EncryptedURL)
		if err == nil {
			err = a.sender.Send(r.Context(), serviceURL, "ArtistTrackarr test", "Your notification destination is working.")
		}
	}
	if err != nil {
		http.Redirect(w, r, "/settings?message="+url.QueryEscape("Test failed: "+notify.RedactError(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?message=Test+sent", http.StatusSeeOther)
}

func (a *App) retryDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	count, err := a.store.RetryFailedDeliveries(r.Context(), session.User.ID, id, time.Now().UTC())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("retry destination deliveries failed", "user_id", session.User.ID, "destination_id", id, "error", err)
		http.Error(w, "destination deliveries could not be retried", http.StatusInternalServerError)
		return
	}
	if a.jobs != nil && count > 0 {
		a.jobs.Wake()
	}
	http.Redirect(w, r, "/settings?message="+url.QueryEscape(fmt.Sprintf("%d failed deliveries queued for retry", count)), http.StatusSeeOther)
}
func (a *App) renameDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.RenameDestination(r.Context(), session.User.ID, id, r.FormValue("name")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		d := a.data(r, "Settings")
		d.Error = err.Error()
		if a.loadSettingsData(r, &d) {
			a.render(w, "settings", d, http.StatusInternalServerError)
		} else {
			a.render(w, "settings", d, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/settings?message=Destination+renamed", http.StatusSeeOther)
}
func (a *App) deleteDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteDestination(r.Context(), session.User.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("delete destination failed", "user_id", session.User.ID, "destination_id", id, "error", err)
		http.Error(w, "destination could not be deleted", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?message=Destination+deleted", http.StatusSeeOther)
}
func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if err := a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time"), session.User.Username); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?message=Reminder+settings+updated", http.StatusSeeOther)
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	d := a.data(r, "Settings")
	status := http.StatusOK
	if a.loadSettingsData(r, &d) {
		status = http.StatusInternalServerError
	}
	a.render(w, "settings", d, status)
}
func (a *App) settingsProfile(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	username := session.User.Username
	if _, supplied := r.Form["username"]; supplied {
		username = r.FormValue("username")
	}
	err := a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time"), username)
	if err != nil {
		d := a.data(r, "Settings")
		d.Error = err.Error()
		if a.loadSettingsData(r, &d) {
			a.render(w, "settings", d, http.StatusInternalServerError)
		} else {
			a.render(w, "settings", d, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/settings?message=Settings+updated", http.StatusSeeOther)
}
func (a *App) settingsPreferences(w http.ResponseWriter, r *http.Request) {
	if err := a.savePreferences(w, r, "/settings"); err != nil {
		return
	}
}
func (a *App) updatePreferences(w http.ResponseWriter, r *http.Request) {
	if err := a.savePreferences(w, r, "/settings"); err != nil {
		return
	}
}
func (a *App) savePreferences(w http.ResponseWriter, r *http.Request, redirectPath string) error {
	session, _ := currentSession(r)
	p := store.NotificationPreferences{UserID: session.User.ID,
		Albums: r.FormValue("albums") == "on", EPs: r.FormValue("eps") == "on", Singles: r.FormValue("singles") == "on",
		Announcements: r.FormValue("announcements") == "on", ReleaseDay: r.FormValue("release_day") == "on",
		DigestEnabled: r.FormValue("digest_enabled") == "on", DigestFrequency: r.FormValue("digest_frequency"),
		HoldConflictingNotifications: r.FormValue("hold_conflicting_notifications") == "on"}
	if err := a.store.UpdateNotificationPreferences(r.Context(), p); err != nil {
		http.Redirect(w, r, redirectPath+"?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return err
	}
	http.Redirect(w, r, redirectPath+"?message=Notification+preferences+updated", http.StatusSeeOther)
	return nil
}
