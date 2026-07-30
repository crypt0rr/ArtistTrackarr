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
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type fakeSender struct{}

func (fakeSender) Validate(string) error                              { return nil }
func (fakeSender) Send(context.Context, string, string, string) error { return nil }

type fakeArtwork struct{}

func (fakeArtwork) Get(context.Context, string) artwork.Asset {
	return artwork.Asset{
		Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), ContentType: "image/svg+xml",
		Status: "test", MaxAge: time.Minute,
	}
}

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
		fakeSender{}, cipher, fakeArtwork{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	user, err := database.UserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(context.Background(), user.ID, "Phone", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	response, err = client.PostForm(server.URL+"/destinations/1/rename", url.Values{"name": {"Without CSRF"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("rename without CSRF status = %d", response.StatusCode)
	}
	response.Body.Close()

	csrf = getCSRF(t, client, server.URL+"/destinations")
	response = postForm(t, client, server.URL+"/destinations/1/rename", url.Values{
		"_csrf": {csrf}, "name": {"My phone"},
	})
	response.Body.Close()
	destination, err := database.Destination(context.Background(), user.ID, 1)
	if err != nil || destination.Name != "My phone" {
		t.Fatalf("renamed destination = %#v, %v", destination, err)
	}

	response, err = client.Get(server.URL + "/art/release-group/6e335887-60ba-38f0-95af-fae7774336bf")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Artwork-Status") != "test" ||
		response.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("artwork response status=%d headers=%v", response.StatusCode, response.Header)
	}
	response.Body.Close()
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
