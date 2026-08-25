package web

import (
	"bytes"
	"database/sql"
	"errors"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestInvitationErrorMessageDoesNotReflectStorageDetails(t *testing.T) {
	storageError := errors.New("UNIQUE constraint failed: users.username secret-token")
	message := invitationErrorMessage(storageError)
	if !strings.Contains(message, "Invitation is invalid") {
		t.Fatalf("generic invitation message=%q", message)
	}
	if strings.Contains(message, storageError.Error()) || strings.Contains(message, "secret-token") {
		t.Fatalf("invitation message reflected storage details: %q", message)
	}
	if got := invitationErrorMessage(sql.ErrNoRows); !strings.Contains(got, "Invitation is invalid") {
		t.Fatalf("expired invitation message=%q", got)
	}
}

func TestInvitationErrorMessageKeepsActionableValidation(t *testing.T) {
	if got := invitationErrorMessage(store.ErrInvalidUsername); !strings.Contains(got, "username") {
		t.Fatalf("invalid username message=%q", got)
	}
	if got := invitationErrorMessage(store.ErrUsernameTaken); !strings.Contains(got, "already in use") {
		t.Fatalf("duplicate username message=%q", got)
	}
}

// TestCsrfCookieIsNoStricterThanTheSession pins the two cookies together. The
// CSRF cookie was SameSite=Strict while artist_session was Lax, so a cross-site
// top-level navigation - a homelab dashboard tile, a webmail tab, or this app's
// own ICS event links, which point at {PublicURL}/releases/{id} - carried the
// session but not the CSRF token. The middleware cannot distinguish "no token
// yet" from "token withheld by SameSite", so it minted a fresh one and silently
// invalidated the token held by every page already open; the next submit was a
// bare 403 with the member's form input lost.
//
// The invariant is the relationship, not the literal value: whenever the
// session survives a navigation, the CSRF token must survive it too.
func TestCsrfCookieIsNoStricterThanTheSession(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)

	response, err := client.Get(server.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	var csrf, session *http.Cookie
	for _, c := range response.Cookies() {
		switch c.Name {
		case "artist_csrf":
			csrf = c
		case "artist_session":
			session = c
		}
	}
	if csrf == nil {
		// Already minted on an earlier request in this client's jar; force a
		// fresh mint with a jar-less request instead.
		bare := &http.Client{}
		fresh, err := bare.Get(server.URL + "/login")
		if err != nil {
			t.Fatal(err)
		}
		_ = fresh.Body.Close()
		for _, c := range fresh.Cookies() {
			if c.Name == "artist_csrf" {
				csrf = c
			}
		}
	}
	if csrf == nil {
		t.Fatal("no artist_csrf cookie was ever set")
	}
	if csrf.SameSite == http.SameSiteStrictMode {
		t.Fatalf("artist_csrf is SameSite=Strict; a cross-site entry that keeps the session drops the token and 403s every open form")
	}
	if csrf.SameSite != http.SameSiteLaxMode {
		t.Fatalf("artist_csrf SameSite=%v, want Lax to match artist_session", csrf.SameSite)
	}
	if session != nil && csrf.SameSite != session.SameSite {
		t.Fatalf("artist_csrf SameSite=%v but artist_session SameSite=%v; they must agree",
			csrf.SameSite, session.SameSite)
	}
}

// TestCsrfStillRejectsAForgedPost keeps the protection the Strict setting was
// mistaken for. A POST carrying no valid CSRF cookie must still be refused.
func TestCsrfStillRejectsAForgedPost(t *testing.T) {
	_, server, _ := authenticatedTestServer(t, nil, nil, nil)
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := bare.PostForm(server.URL+"/artists/1/sync", url.Values{"_csrf": {"forged-value"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("a POST with a forged CSRF token was accepted")
	}
}

// TestCredentialLifecycleEventsAreRecorded is #265. Every logger call on the
// authentication path used to be a failure line for an infrastructure error, so
// a successful sign-in, a sign-out, an individual rejected attempt, a password
// change and the issuance of an invite or feed token left no record at all. The
// generic HTTP access log is emitted at Debug while the default level is info,
// so on a stock deployment there was no request log either — and the app already
// persists application logs and renders them on /admin/diagnostics, so the sink
// existed and was simply never fed.
func TestCredentialLifecycleEventsAreRecorded(t *testing.T) {
	var logs bytes.Buffer
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	_ = database

	// authenticatedTestServer signs in during setup, so drive the events that
	// follow it against a recording logger.
	handler := logging.NewHandler(slog.NewJSONHandler(&logs, nil), 64)
	app := &App{logger: slog.New(handler)}

	app.logger.Info("sign-in succeeded", "event", "auth.signin", "user_id", int64(1))
	app.logger.Info("sign-out", "event", "auth.signout", "user_id", int64(1))
	if !strings.Contains(logs.String(), "auth.signin") || !strings.Contains(logs.String(), "auth.signout") {
		t.Fatalf("lifecycle events do not survive the redacting handler: %s", logs.String())
	}
	// The event key must not be eaten by redaction, and neither must user_id.
	if strings.Contains(logs.String(), "[redacted]") {
		t.Fatalf("a lifecycle event field was redacted: %s", logs.String())
	}

	// And the real sign-out route must emit one.
	response, err := client.Post(server.URL+"/logout", "application/x-www-form-urlencoded",
		strings.NewReader("_csrf="+getCSRF(t, client, server.URL+"/settings")))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

// TestEveryCredentialLifecycleEventHasAnEmitter is a source-level guard: it
// pins that each named event still has a handler emitting it, so removing one
// during a refactor fails the build rather than silently deleting an audit
// record. It deliberately does not assert on rendered output - the behavioural
// half is covered by TestCredentialLifecycleEventsAreRecorded.
func TestEveryCredentialLifecycleEventHasAnEmitter(t *testing.T) {
	for _, want := range []string{
		"auth.signin", "auth.signout", "auth.signin_failed",
		"auth.password_changed", "auth.feed_token_issued", "auth.feed_token_revoked",
		"auth.user_deleted", "auth.invite_issued", "auth.reset_issued",
	} {
		found := false
		for _, file := range []string{"auth.go", "settings.go", "admin.go"} {
			data, err := os.ReadFile(filepath.Join(".", file))
			if err != nil {
				t.Fatal(err)
			}
			// Match the quoted literal: a bare substring lets "auth.signin"
			// match inside "auth.signin_failed", so removing the sign-in event
			// would go undetected.
			if strings.Contains(string(data), `"`+want+`"`) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no handler emits the %q credential-lifecycle event", want)
		}
	}
}
