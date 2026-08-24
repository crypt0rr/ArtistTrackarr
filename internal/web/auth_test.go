package web

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
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
