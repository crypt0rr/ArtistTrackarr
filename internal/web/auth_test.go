package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCrossOriginProtectionRejectsCrossSitePosts(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	for _, fetchSite := range []string{"same-site", "cross-site"} {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://attacker.invalid")
		request.Header.Set("Sec-Fetch-Site", fetchSite)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("POST with Sec-Fetch-Site %q returned %d, want %d", fetchSite, response.StatusCode, http.StatusForbidden)
		}
	}
}

func TestCrossOriginProtectionRejectsUntrustedOriginWithoutFetchMetadata(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	request, err := http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://attacker.invalid")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with untrusted Origin and no Sec-Fetch-Site returned %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestCrossOriginProtectionAcceptsSameOriginPost(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	request, err := http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", request.URL.Scheme+"://"+request.URL.Host)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	noRedirect := &http.Client{
		Jar: client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := noRedirect.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin POST was rejected: status=%d", response.StatusCode)
	}
}

func TestCrossOriginProtectionTrustsConfiguredPublicOriginBehindProxy(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	request, err := http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.test")
	noRedirect := &http.Client{
		Jar: client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := noRedirect.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("trusted public origin was rejected behind the proxy: status=%d body=%s", response.StatusCode, body)
	}
}

func TestTokenRouteErrorsLogRoutePatternsWithoutBearerTokens(t *testing.T) {
	var stdout bytes.Buffer
	database, server, client := authenticatedTestServerLogging(t, &stdout, nil, nil, nil, nil)
	var userID int64
	if err := database.DB.QueryRow(`SELECT id FROM users WHERE email='member@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	inviteToken, err := database.CreateAuthToken(context.Background(), "invite", "invitee@example.com", nil, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resetToken, err := database.CreateAuthToken(context.Background(), "reset", "member@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	csrf := getCSRF(t, client, server.URL+"/settings")

	// Break only the optional inbox count lookup after the authenticated session
	// and token routes are ready. The pages continue rendering while their error
	// logs exercise the paths that carry bearer tokens.
	if _, err := database.DB.Exec(`DROP TABLE release_groups`); err != nil {
		t.Fatalf("force inbox count lookup failure: %v", err)
	}
	for _, route := range []struct {
		name  string
		token string
	}{
		{name: "invite", token: inviteToken},
		{name: "reset", token: resetToken},
	} {
		response, err := client.Get(server.URL + "/" + route.name + "/" + route.token)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()

		response, err = client.PostForm(server.URL+"/"+route.name+"/"+route.token, url.Values{
			"_csrf":    {csrf},
			"password": {"short"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	entries, err := database.ApplicationLogs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var persisted strings.Builder
	for _, entry := range entries {
		persisted.WriteString(entry.Message)
		for _, attribute := range entry.Attributes {
			persisted.WriteString(attribute.Key)
			persisted.WriteString(attribute.Value)
		}
	}
	for label, logs := range map[string]string{"stdout": stdout.String(), "persisted": persisted.String()} {
		for _, token := range []string{inviteToken, resetToken} {
			if strings.Contains(logs, token) {
				t.Errorf("%s logs contained a raw bearer token", label)
			}
		}
		for _, pattern := range []string{"/invite/{token}", "/reset/{token}"} {
			if !strings.Contains(logs, pattern) {
				t.Errorf("%s logs did not retain safe route pattern %q: %s", label, pattern, logs)
			}
		}
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
	_, server, client := authenticatedTestServerLogging(t, &logs, nil, nil, nil, nil)

	// Drive the real routes and read the server's own log. The previous
	// version of this test built its own App, called logger.Info with the two
	// events itself, and asserted they came back - which exercises
	// logging.NewHandler and never touches auth.go. Deleting the emitter from
	// the sign-out handler left it green.
	response, err := client.PostForm(server.URL+"/settings/calendar-feed", url.Values{
		"_csrf":  {getCSRF(t, client, server.URL+"/settings")},
		"action": {"generate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	response, err = client.PostForm(server.URL+"/logout", url.Values{
		"_csrf": {getCSRF(t, client, server.URL+"/settings")},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	recorded := logs.String()
	for _, event := range []string{"auth.feed_token_issued", "auth.signout"} {
		if !strings.Contains(recorded, event) {
			t.Errorf("the route that should emit %q did not: %s", event, recorded)
		}
	}

	// The event key and user_id must survive redaction, or the audit record
	// reaches the operator with nothing identifying in it.
	if strings.Contains(recorded, "[redacted]") {
		t.Errorf("a lifecycle event field was redacted: %s", recorded)
	}
	if !strings.Contains(recorded, `"user_id"`) {
		t.Errorf("lifecycle events carry no user_id: %s", recorded)
	}
}

// TestEveryCredentialLifecycleEventHasAnEmitter is a source-level guard: it
// pins that each named event still has a handler emitting it, so removing one
// during a refactor fails the build rather than silently deleting an audit
// record. It deliberately does not assert on rendered output.
//
// A source grep cannot tell a live emitter from an unreachable one, so it is
// only half the story; TestCredentialLifecycleEventsAreRecorded supplies the
// behavioural half for two events a member session can actually reach.
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

// TestUnauthenticatedDeepLinkReturnsAfterSignIn is #290. requireUser redirected
// to a bare "/login" and login always went to "/", so the application had no
// post-authentication return path. That matters because it publishes an external
// deep link: every ICS event carries a URL: property resolving to
// {PublicURL}/releases/{id}, and the point of the revocable feed token is that
// the calendar is read on devices separate from the browser session - so
// following one of those links landed on the dashboard with no route back.
func TestUnauthenticatedDeepLinkReturnsAfterSignIn(t *testing.T) {
	_, server, _ := authenticatedTestServer(t, nil, nil, nil)
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	response, err := bare.Get(server.URL + "/releases/7")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	location := response.Header.Get("Location")
	if !strings.HasPrefix(location, "/login") {
		t.Fatalf("unauthenticated request went to %q, want the login page", location)
	}
	target, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if got := target.Query().Get("next"); got != "/releases/7" {
		t.Fatalf("the requested path was not carried to login: next=%q", got)
	}
}

// TestReturnPathRejectsOffsiteAndUnsafeTargets keeps the new parameter from
// becoming an open redirect. It goes through the same allowlisting helper the
// rest of the application uses.
func TestReturnPathRejectsOffsiteAndUnsafeTargets(t *testing.T) {
	for _, hostile := range []string{
		"https://evil.example/steal", "//evil.example/steal", "/\\evil.example",
		"http://evil.example", "\r\nSet-Cookie: x=1", "",
	} {
		if got := localReturnPath(hostile, "", "/"); got != "/" {
			t.Fatalf("localReturnPath(%q)=%q, want the safe fallback", hostile, got)
		}
	}
	if got := localReturnPath("/releases/7?tab=evidence", "", "/"); got != "/releases/7?tab=evidence" {
		t.Fatalf("a legitimate local path was rejected: %q", got)
	}
}
