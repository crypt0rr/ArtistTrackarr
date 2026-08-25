package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
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
	session, ok := currentSession(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mbid := chi.URLParam(r, "mbid")
	if !artwork.ValidMBID(mbid) {
		http.NotFound(w, r)
		return
	}
	visible, err := a.store.ReleaseGroupVisibleByMBID(r.Context(), session.User.ID, mbid)
	if err != nil {
		if a.logger != nil {
			a.logger.Error("release artwork visibility lookup failed", "user_id", session.User.ID, "error", err)
		}
		http.Error(w, "artwork is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	if !visible {
		http.NotFound(w, r)
		return
	}
	if a.artworkLimiter != nil && !a.artworkLimiter.Allow(fmt.Sprintf("%d", session.User.ID)) {
		rateLimited(w, 60, "artwork requests are temporarily rate limited; try again later")
		return
	}
	asset := a.artwork.Get(r.Context(), mbid)
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
		Username: r.FormValue("username"), Password: r.FormValue("password"),
		Token: r.FormValue("token"), Target: r.FormValue("target"), Topic: r.FormValue("topic"),
	}
	serviceURL, err := notify.BuildURL(input)
	if err == nil {
		err = a.sender.Validate(serviceURL)
	}
	storedService := notify.CanonicalTransportService(serviceURL)
	var encrypted []byte
	if err == nil {
		encrypted, err = a.cipher.Encrypt(serviceURL)
	}
	if err == nil {
		err = a.store.AddDestination(r.Context(), session.User.ID, r.FormValue("name"), storedService, encrypted)
	}
	if err != nil {
		d := a.data(r, "Settings")
		d.Error = notify.RedactError(err)
		status := http.StatusBadRequest
		if !errors.Is(err, store.ErrDestinationLimit) && a.loadSettingsData(r, &d) {
			status = http.StatusInternalServerError
		} else if errors.Is(err, store.ErrDestinationLimit) {
			// This is a user-actionable admission limit, not a database failure;
			// keep the settings page and explain how to recover.
			d.Error = err.Error()
			_ = a.loadSettingsData(r, &d)
		}
		a.render(w, "settings", d, status)
		return
	}
	http.Redirect(w, r, "/settings?"+a.statusQuery("Destination added"), http.StatusSeeOther)
}
func (a *App) testDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, parseErr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if parseErr != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	// Establish ownership first: an unknown or foreign id must 404 without
	// spending the member's budget, and must never reach the limiter at all.
	destination, err := a.store.Destination(r.Context(), session.User.ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	// Keyed on the member alone, not on the destination id. The id is
	// caller-chosen: destinations.id is a plain INTEGER PRIMARY KEY, so
	// delete-then-add mints a fresh id and with it a fresh empty bucket, and
	// the limiter evicts its oldest entry at maxEntries - so 4096 requests
	// naming non-existent ids could flush every other member's bucket out of
	// the shared limiter. The resource being protected is outbound sends from
	// this process, which is a per-member budget.
	if a.destinationTestLimiter != nil && !a.destinationTestLimiter.Allow(strconv.FormatInt(session.User.ID, 10)) {
		rateLimited(w, 900, "destination tests are temporarily rate limited; try again later")
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
		a.logger.Warn("notification test failed", "path", r.URL.Path, "destination_id", id, "error", notify.RedactError(err))
		http.Redirect(w, r, "/settings?"+a.statusQuery("Test failed; see destination health for details."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?"+a.statusQuery("Test sent"), http.StatusSeeOther)
}

func (a *App) retryDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	// Per-member for the same reason as the test limiter above: the id is
	// caller-chosen, so embedding it both multiplies the budget and lets one
	// member evict every other member's bucket from the shared limiter.
	if a.destinationRetryLimiter != nil && !a.destinationRetryLimiter.Allow(strconv.FormatInt(session.User.ID, 10)) {
		rateLimited(w, 900, "destination retries are temporarily rate limited; try again later")
		return
	}
	stats, err := a.store.RetryFailedDeliveries(r.Context(), session.User.ID, id, time.Now().UTC())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("retry destination deliveries failed", "user_id", session.User.ID, "destination_id", id, "error", err)
		http.Error(w, "destination deliveries could not be retried", http.StatusInternalServerError)
		return
	}
	if a.jobs != nil && stats.Requeued > 0 {
		a.jobs.Wake()
	}
	// Say what happened to the rows a pause is holding back. Reporting only the
	// requeued count showed "0 failed deliveries queued for retry" next to a
	// destination still badged with failures, which reads as the button being
	// broken rather than as a pause doing its job.
	message := fmt.Sprintf("%d failed deliveries queued for retry", stats.Requeued)
	if stats.Deferred > 0 {
		message += fmt.Sprintf("; %d unblocked and held until a paused follow resumes", stats.Deferred)
	}
	http.Redirect(w, r, "/settings?"+a.statusQuery(message), http.StatusSeeOther)
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
		d.Error = safeActionMessage(err, "Destination could not be renamed. Please try again.")
		if a.loadSettingsData(r, &d) {
			a.render(w, "settings", d, http.StatusInternalServerError)
		} else {
			a.render(w, "settings", d, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/settings?"+a.statusQuery("Destination renamed"), http.StatusSeeOther)
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
	http.Redirect(w, r, "/settings?"+a.statusQuery("Destination deleted"), http.StatusSeeOther)
}
func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if err := a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time"), session.User.Username); err != nil {
		http.Redirect(w, r, "/settings?"+a.statusQuery(safeActionMessage(err, "Reminder settings could not be saved. Please try again.")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?"+a.statusQuery("Reminder settings updated"), http.StatusSeeOther)
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	d := a.data(r, "Settings")
	status := http.StatusOK
	if a.loadSettingsData(r, &d) {
		status = http.StatusInternalServerError
	}
	a.render(w, "settings", d, status)
}

func (a *App) calendarFeedURL(raw string) string {
	path := "/calendar/feed/" + url.PathEscape(raw)
	if a.cfg.PublicURL == nil {
		return path
	}
	return a.cfg.PublicURL.ResolveReference(&url.URL{Path: path}).String()
}

func (a *App) calendarFeedAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	switch strings.ToLower(strings.TrimSpace(r.FormValue("action"))) {
	case "generate", "rotate", "":
		raw, err := a.store.CreateCalendarFeedToken(r.Context(), session.User.ID)
		if err != nil {
			d := a.data(r, "Settings")
			d.Error = "We couldn't create the calendar feed right now. Please try again."
			if a.logger != nil {
				a.logger.Error("calendar feed token creation failed", "user_id", session.User.ID, "error", err)
			}
			status := http.StatusInternalServerError
			if a.loadSettingsData(r, &d) {
				status = http.StatusInternalServerError
			}
			a.render(w, "settings", d, status)
			return
		}
		d := a.data(r, "Settings")
		a.logger.Info("calendar feed token issued", "event", "auth.feed_token_issued", "user_id", session.User.ID)
		d.Message = "Calendar feed created. Copy this URL now; it will not be shown again."
		d.CalendarFeedURL = a.calendarFeedURL(raw)
		status := http.StatusOK
		if a.loadSettingsData(r, &d) {
			status = http.StatusInternalServerError
		}
		a.render(w, "settings", d, status)
	case "revoke":
		if err := a.store.RevokeCalendarFeedToken(r.Context(), session.User.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			if a.logger != nil {
				a.logger.Error("calendar feed token revocation failed", "user_id", session.User.ID, "error", err)
			}
			http.Error(w, "calendar feed could not be revoked", http.StatusInternalServerError)
			return
		}
		a.logger.Info("calendar feed token revoked", "event", "auth.feed_token_revoked", "user_id", session.User.ID)
		http.Redirect(w, r, "/settings?"+a.statusQuery("Calendar feed revoked"), http.StatusSeeOther)
	default:
		http.Error(w, "invalid calendar feed action", http.StatusBadRequest)
	}
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
		d.Error = safeActionMessage(err, "Settings could not be saved. Please try again.")
		if a.loadSettingsData(r, &d) {
			a.render(w, "settings", d, http.StatusInternalServerError)
		} else {
			a.render(w, "settings", d, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/settings?"+a.statusQuery("Settings updated"), http.StatusSeeOther)
}

func (a *App) settingsPassword(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	_, release, ok := a.acquirePasswordSlot(w, r, a.passwordChangeLimiter, 900, "too many password change attempts; try again later")
	if !ok {
		return
	}
	defer release()

	renderError := func(message string, status int) {
		d := a.data(r, "Settings")
		d.Error = message
		if a.loadSettingsData(r, &d) {
			status = http.StatusInternalServerError
		}
		a.render(w, "settings", d, status)
	}

	user, err := a.store.UserByID(r.Context(), session.User.ID)
	if err != nil {
		a.logger.Error("load password change account failed", "user_id", session.User.ID, "error", err)
		renderError("Password could not be changed right now. Please try again.", http.StatusInternalServerError)
		return
	}
	if !security.CheckPassword(user.PasswordHash, r.FormValue("current_password")) {
		renderError("The current password is incorrect.", http.StatusBadRequest)
		return
	}
	newPassword := r.FormValue("new_password")
	if newPassword != r.FormValue("confirm_password") {
		renderError("The new passwords do not match.", http.StatusBadRequest)
		return
	}
	hash, err := security.HashPassword(newPassword)
	if err != nil {
		renderError(safeActionMessage(err, "The new password could not be used."), http.StatusBadRequest)
		return
	}
	if err := a.store.UpdatePassword(r.Context(), session.User.ID, hash); err != nil {
		a.logger.Error("update password failed", "user_id", session.User.ID, "error", err)
		renderError("Password could not be changed right now. Please try again.", http.StatusInternalServerError)
		return
	}
	// UpdatePassword revokes every session, including the one used for this
	// request. Send the user through the normal login flow rather than leaving
	// a stale cookie that appears authenticated until its next request.
	a.logger.Info("password changed", "event", "auth.password_changed", "user_id", session.User.ID)
	http.Redirect(w, r, "/login?"+a.statusQuery("Password updated"), http.StatusSeeOther)
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
		http.Redirect(w, r, redirectPath+"?"+a.statusQuery(safeActionMessage(err, "Notification preferences could not be saved. Please try again.")), http.StatusSeeOther)
		return err
	}
	http.Redirect(w, r, redirectPath+"?"+a.statusQuery("Notification preferences updated"), http.StatusSeeOther)
	return nil
}
