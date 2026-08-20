package web

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
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
	_, release, ok := a.acquirePasswordSlot(w, r, a.setupLimiter, 900, "too many setup attempts; try again later")
	if !ok {
		return
	}
	defer release()
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
			_, err = a.store.CreateInitialAdmin(r.Context(), r.FormValue("email"), hash, r.FormValue("timezone"), username)
		}
	}
	if err != nil {
		if errors.Is(err, store.ErrSetupCompleted) {
			http.Error(w, "setup has already completed", http.StatusConflict)
			return
		}
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
	clientIP, release, ok := a.acquirePasswordSlot(w, r, a.loginLimiter, 300, "too many login attempts; try again later")
	if !ok {
		return
	}
	defer release()
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	keys := loginThrottleKeys(clientIP, email)
	for _, key := range keys {
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
		for _, key := range keys {
			_ = a.store.RecordLoginFailure(r.Context(), key)
		}
		time.Sleep(250 * time.Millisecond)
		d := a.data(r, "Sign in")
		d.Error = "Email or password is incorrect."
		a.render(w, "login", d, http.StatusUnauthorized)
		return
	}
	for _, key := range keys {
		a.store.ClearLoginFailures(r.Context(), key)
	}
	raw, _, err := a.store.CreateSession(r.Context(), user.ID, sessionLifetime)
	if err != nil {
		a.logger.Error("create session", "user_id", user.ID, "error", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "artist_session", Value: security.SignedToken(a.cfg.SessionSecret, raw), Path: "/",
		HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionLifetime.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("artist_session"); err == nil {
		if raw, ok := security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value); ok {
			_ = a.store.DeleteSession(r.Context(), raw)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: "artist_session", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
	})
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
	_, release, ok := a.acquirePasswordSlot(w, r, a.tokenLimiter, 300, "too many account setup attempts; try again later")
	if !ok {
		return
	}
	defer release()
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
		if !errors.Is(err, sql.ErrNoRows) {
			digest := security.Digest(chi.URLParam(r, "token"))
			a.logger.Warn("invitation acceptance failed", "page", "Accept invitation", "route", "/invite/{token}",
				"token_fingerprint", fmt.Sprintf("%x", digest[:6]), "error", err)
		}
		d := a.data(r, "Accept invitation")
		d.Error, d.Token, d.TokenKind = invitationErrorMessage(err), chi.URLParam(r, "token"), "invite"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Account+created", http.StatusSeeOther)
}

// invitationErrorMessage intentionally keeps token-backed account errors
// generic. Validation errors are actionable without exposing database details;
// the full cause is retained in the structured server log above.
func invitationErrorMessage(err error) string {
	switch {
	case errors.Is(err, store.ErrInvalidUsername):
		return "Invitation is invalid, expired, or already used. Choose a username with 3–32 letters, numbers, dots, underscores, or hyphens."
	case errors.Is(err, store.ErrUsernameTaken):
		return "Invitation is invalid, expired, or already used. That username is already in use; choose another."
	case strings.Contains(strings.ToLower(err.Error()), "invalid iana timezone"):
		return "Invitation is invalid, expired, or already used. Choose a valid IANA timezone."
	default:
		return "Invitation is invalid, expired, or already used. Check your details and try again."
	}
}
func (a *App) acceptReset(w http.ResponseWriter, r *http.Request) {
	_, release, ok := a.acquirePasswordSlot(w, r, a.tokenLimiter, 300, "too many password reset attempts; try again later")
	if !ok {
		return
	}
	defer release()
	hash, err := security.HashPassword(r.FormValue("password"))
	if err == nil {
		err = a.store.ResetPasswordWithToken(r.Context(), chi.URLParam(r, "token"), hash)
	}
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			digest := security.Digest(chi.URLParam(r, "token"))
			a.logger.Warn("password reset failed", "page", "Reset password", "route", "/reset/{token}",
				"token_fingerprint", fmt.Sprintf("%x", digest[:6]), "error", err)
		}
		d := a.data(r, "Reset password")
		d.Error, d.Token, d.TokenKind = "Reset link is invalid, expired, or already used. Please request a new link.", chi.URLParam(r, "token"), "reset"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Password+updated", http.StatusSeeOther)
}

// acquirePasswordSlot bounds the expensive Argon2 work used by login and
// token-backed account flows. The per-IP window slows repeated attempts while
// the shared semaphore prevents a distributed source from exhausting CPU.
func (a *App) acquirePasswordSlot(w http.ResponseWriter, r *http.Request, limiter *fixedWindowLimiter, retryAfter int, message string) (string, func(), bool) {
	clientIP := a.clientIP(r)
	if limiter != nil && !limiter.Allow(clientIP) {
		rateLimited(w, retryAfter, message)
		return clientIP, func() {}, false
	}
	if a.loginSlots == nil {
		return clientIP, func() {}, true
	}
	select {
	case a.loginSlots <- struct{}{}:
		return clientIP, func() { <-a.loginSlots }, true
	default:
		rateLimited(w, 5, "password service is busy; try again shortly")
		return clientIP, func() {}, false
	}
}
func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if !a.cfg.TrustProxy || peer == nil || !a.trustedProxy(peer) {
		return host
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	// Walk from the nearest proxy toward the original client. Ignore entries
	// that are themselves trusted proxies and return the first untrusted IP;
	// this prevents a direct client from spoofing a throttling key.
	for i := len(forwarded) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if candidate == nil {
			continue
		}
		if !a.trustedProxy(candidate) {
			return candidate.String()
		}
	}
	// If every forwarded entry is trusted (or malformed), there is no
	// trustworthy client identity in the header. Use the direct peer instead
	// of accepting a caller-controlled leftmost value as a throttling key.
	return host
}

// loginThrottleKeys applies both the peer identity and an account identity.
// The peer key protects a single source while the account key keeps failures
// throttled when an attacker rotates addresses through a trusted proxy.
func loginThrottleKeys(clientIP, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	return []string{clientIP + "|" + email, "account:" + email}
}

func (a *App) trustedProxy(ip net.IP) bool {
	for _, network := range a.cfg.TrustedProxyNetworks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
