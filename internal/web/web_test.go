package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type fakeSender struct{}

func (fakeSender) Validate(string) error                              { return nil }
func (fakeSender) Send(context.Context, string, string, string) error { return nil }

func TestSetupLoginAndDashboard(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SetupToken: "the setup token has more than 32 bytes",
		SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, _ := security.NewCipher(cfg.EncryptionKey)
	app, err := New(cfg, database, catalog.NewMusicBrainz("test@example.com"), nil,
		fakeSender{}, cipher, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	csrf := getCSRF(t, client, server.URL+"/setup")
	setupResponse := postForm(t, client, server.URL+"/setup", url.Values{
		"_csrf": {csrf}, "setup_token": {cfg.SetupToken}, "email": {"admin@example.com"},
		"password": {"a secure test password"}, "timezone": {"Europe/Amsterdam"},
	})
	io.Copy(io.Discard, setupResponse.Body)
	setupResponse.Body.Close()
	csrf = getCSRF(t, client, server.URL+"/login")
	response := postForm(t, client, server.URL+"/login", url.Values{
		"_csrf": {csrf}, "email": {"admin@example.com"}, "password": {"a secure test password"},
	})
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Never miss the next record") {
		t.Fatalf("dashboard status/body = %d, %q", response.StatusCode, body)
	}
}

var csrfPattern = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func getCSRF(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	match := csrfPattern.FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in %s", body)
	}
	return string(match[1])
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()
	response, err := client.PostForm(target, values)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
