package web

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	count, err := a.store.UserCount(r.Context())
	if err != nil {
		d := a.data(r, "Create administrator")
		a.pageStoreError(r, &d, "Create administrator", "user count", err)
		a.render(w, "setup", d, http.StatusInternalServerError)
		return
	}
	if count > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, "setup", a.data(r, "Create administrator"), http.StatusOK)
}
func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	count, err := a.store.UserCount(r.Context())
	if err != nil {
		a.logger.Error("setup user count failed", "page", "Create administrator", "path", r.URL.Path, "error", err)
		http.Error(w, "could not load setup", http.StatusInternalServerError)
		return
	}
	if count > 0 {
		http.Error(w, "setup has already completed", http.StatusConflict)
		return
	}
	if a.cfg.SetupToken == "" || subtle.ConstantTimeCompare([]byte(r.FormValue("setup_token")), []byte(a.cfg.SetupToken)) != 1 {
		d := a.data(r, "Create administrator")
		d.Error = "The setup token is incorrect."
		a.render(w, "setup", d, http.StatusForbidden)
		return
	}
	hash, err := security.HashPassword(r.FormValue("password"))
	if err == nil {
		username := strings.TrimSpace(r.FormValue("username"))
		if _, supplied := r.Form["username"]; supplied && username == "" {
			err = store.ErrInvalidUsername
		} else {
			_, err = a.store.CreateUser(r.Context(), r.FormValue("email"), hash, "admin", r.FormValue("timezone"), username)
		}
	}
	if err != nil {
		d := a.data(r, "Create administrator")
		d.Error = err.Error()
		a.render(w, "setup", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Administrator+created", http.StatusSeeOther)
}
func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	count, err := a.store.UserCount(r.Context())
	d := a.data(r, "Sign in")
	if err != nil {
		a.pageStoreError(r, &d, "Sign in", "user count", err)
		a.render(w, "login", d, http.StatusInternalServerError)
		return
	}
	d.SetupNeeded = count == 0
	a.render(w, "login", d, http.StatusOK)
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	key := a.clientIP(r) + "|" + email
	allowed, err := a.store.LoginAllowed(r.Context(), key)
	if err != nil {
		a.logger.Error("login throttling lookup failed", "page", "Sign in", "path", r.URL.Path, "error", err)
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	if !allowed {
		d := a.data(r, "Sign in")
		d.Error = "Too many attempts. Try again in 15 minutes."
		a.render(w, "login", d, http.StatusTooManyRequests)
		return
	}
	user, err := a.store.UserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.logger.Error("login user lookup failed", "page", "Sign in", "path", r.URL.Path, "error", err)
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	passwordValid := false
	if err == nil {
		passwordValid = security.CheckPassword(user.PasswordHash, r.FormValue("password"))
	} else {
		// Keep unknown-email attempts on the same Argon2id path as an account
		// with a wrong password, without exposing whether the email exists.
		_ = security.CheckPassword(security.DummyPasswordHash, r.FormValue("password"))
	}
	if err != nil || !passwordValid {
		_ = a.store.RecordLoginFailure(r.Context(), key)
		time.Sleep(250 * time.Millisecond)
		d := a.data(r, "Sign in")
		d.Error = "Email or password is incorrect."
		a.render(w, "login", d, http.StatusUnauthorized)
		return
	}
	a.store.ClearLoginFailures(r.Context(), key)
	raw, _, err := a.store.CreateSession(r.Context(), user.ID, 30*24*time.Hour)
	if err != nil {
		a.logger.Error("create session", "user_id", user.ID, "error", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "artist_session", Value: security.SignedToken(a.cfg.SessionSecret, raw), Path: "/",
		HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("artist_session"); err == nil {
		if raw, ok := security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value); ok {
			_ = a.store.DeleteSession(r.Context(), raw)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "artist_session", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
func (a *App) tokenForm(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := a.data(r, map[string]string{"invite": "Accept invitation", "reset": "Reset password"}[kind])
		d.Token, d.TokenKind = chi.URLParam(r, "token"), kind
		a.render(w, "token", d, http.StatusOK)
	}
}
func (a *App) acceptInvite(w http.ResponseWriter, r *http.Request) {
	hash, err := security.HashPassword(r.FormValue("password"))
	if err == nil {
		username := strings.TrimSpace(r.FormValue("username"))
		if _, supplied := r.Form["username"]; supplied && username == "" {
			err = store.ErrInvalidUsername
		} else {
			err = a.store.CreateUserFromInvite(r.Context(), chi.URLParam(r, "token"), hash, username, r.FormValue("timezone"))
		}
	}
	if err != nil {
		d := a.data(r, "Accept invitation")
		d.Error, d.Token, d.TokenKind = "Invitation is invalid, expired, or already used: "+err.Error(), chi.URLParam(r, "token"), "invite"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Account+created", http.StatusSeeOther)
}
func (a *App) acceptReset(w http.ResponseWriter, r *http.Request) {
	hash, err := security.HashPassword(r.FormValue("password"))
	var userID *int64
	if err == nil {
		_, userID, err = a.store.ConsumeAuthToken(r.Context(), chi.URLParam(r, "token"), "reset")
		if err == nil && userID == nil {
			err = errors.New("reset token has no user")
		}
		if err == nil {
			err = a.store.UpdatePassword(r.Context(), *userID, hash)
		}
	}
	if err != nil {
		d := a.data(r, "Reset password")
		d.Error, d.Token, d.TokenKind = "Reset link is invalid, expired, or already used: "+err.Error(), chi.URLParam(r, "token"), "reset"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Password+updated", http.StatusSeeOther)
}
func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		if first := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
