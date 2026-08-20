package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/crypt0rr/artist-tracker/internal/version"
)

type fakeSender struct{}

func (fakeSender) Validate(string) error                              { return nil }
func (fakeSender) Send(context.Context, string, string, string) error { return nil }

type fakeCatalog struct {
	searchErr error
}

func (f fakeCatalog) SearchArtists(context.Context, string, int) ([]catalog.ArtistResult, error) {
	return nil, f.searchErr
}

func (fakeCatalog) ResolveArtist(context.Context, string) (catalog.ArtistResult, error) {
	return catalog.ArtistResult{}, errors.New("not implemented")
}

func (fakeCatalog) ResolveExternalArtist(context.Context, string) ([]catalog.ArtistResult, error) {
	return nil, errors.New("not implemented")
}

func (fakeCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	return nil, nil
}

type searchCatalog struct {
	results      []catalog.ArtistResult
	resolved     map[string]catalog.ArtistResult
	resolveError map[string]error
	err          error
	calls        int
}

func (f *searchCatalog) SearchArtists(context.Context, string, int) ([]catalog.ArtistResult, error) {
	f.calls++
	return f.results, f.err
}

func (f *searchCatalog) ResolveArtist(_ context.Context, mbid string) (catalog.ArtistResult, error) {
	if err := f.resolveError[mbid]; err != nil {
		return catalog.ArtistResult{}, err
	}
	if result, ok := f.resolved[mbid]; ok {
		return result, nil
	}
	return catalog.ArtistResult{}, errors.New("not implemented")
}

func (*searchCatalog) ResolveExternalArtist(context.Context, string) ([]catalog.ArtistResult, error) {
	return nil, nil
}

func (*searchCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	return nil, nil
}

type fakeSpotify struct {
	results     []catalog.SpotifyArtist
	searchErr   error
	artist      catalog.SpotifyArtist
	artists     map[string]catalog.SpotifyArtist
	artistErr   error
	batchErr    error
	searchCalls int
}

func (f *fakeSpotify) SearchArtists(context.Context, string) ([]catalog.SpotifyArtist, error) {
	f.searchCalls++
	return f.results, f.searchErr
}

func (f *fakeSpotify) Artist(_ context.Context, id string) (catalog.SpotifyArtist, error) {
	if artist, ok := f.artists[id]; ok {
		return artist, nil
	}
	return f.artist, f.artistErr
}

func (f *fakeSpotify) Artists(_ context.Context, ids []string) ([]catalog.SpotifyArtist, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	result := make([]catalog.SpotifyArtist, 0, len(ids))
	for _, id := range ids {
		if artist, ok := f.artists[id]; ok {
			result = append(result, artist)
		}
	}
	return result, nil
}

type fakeArtwork struct{}

func (fakeArtwork) Get(context.Context, string) artwork.Asset {
	return artwork.Asset{
		Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), ContentType: "image/svg+xml",
		Status: "test", MaxAge: time.Minute,
	}
}

func TestSecurityHeadersAndDebugRequestLog(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "headers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	public, _ := url.Parse("https://example.test")
	cfg := config.Config{
		PublicURL: public, SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, _ := security.NewCipher(cfg.EncryptionKey)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app, err := New(cfg, database, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("health status=%d", response.Code)
	}
	if response.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("HSTS=%q", response.Header().Get("Strict-Transport-Security"))
	}
	if response.Header().Get("Permissions-Policy") != "camera=(), geolocation=(), microphone=(), payment=()" {
		t.Fatalf("Permissions-Policy=%q", response.Header().Get("Permissions-Policy"))
	}
	logText := logs.String()
	if !strings.Contains(logText, "http request completed") || !strings.Contains(logText, "route=/healthz") || !strings.Contains(logText, "status=204") {
		t.Fatalf("debug request log=%q", logText)
	}
	if strings.Contains(logText, "example.test") || strings.Contains(logText, "password") || strings.Contains(logText, "body") {
		t.Fatalf("request log leaked sensitive/path data: %q", logText)
	}
}

func TestReadyRejectsReadOnlyDatabase(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "ready-read-only.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := &store.Store{DB: database.Reader}
	app, err := New(cfg, readOnly, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("read-only readiness status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := response.Header().Get("X-ArtistTrackarr-Database"); got != string(store.DatabaseReadOnly) {
		t.Fatalf("read-only readiness database header=%q, want %q", got, store.DatabaseReadOnly)
	}
}

func TestSetupLoginAndDashboard(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SetupToken: "the setup token has more than 32 bytes",
		SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, _ := security.NewCipher(cfg.EncryptionKey)
	app, err := New(cfg, database, fakeCatalog{searchErr: io.ErrUnexpectedEOF}, nil,
		fakeSender{}, cipher, fakeArtwork{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	loginPage, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	if loginPage.Header.Get("Cache-Control") != "private, no-store" || loginPage.Header.Get("Vary") != "Cookie" {
		t.Fatalf("login cache headers=%q vary=%q", loginPage.Header.Get("Cache-Control"), loginPage.Header.Get("Vary"))
	}
	loginBody, _ := io.ReadAll(loginPage.Body)
	_ = loginPage.Body.Close()
	if !strings.Contains(string(loginBody), "/static/logo-full.png") ||
		!strings.Contains(string(loginBody), "/static/favicon.ico") ||
		!strings.Contains(string(loginBody), "/static/theme.js") ||
		!strings.Contains(string(loginBody), "v"+version.Current) ||
		!strings.Contains(string(loginBody), "v="+version.Current+"-") ||
		!strings.Contains(string(loginBody), "https://github.com/crypt0rr/ArtistTrackarr") ||
		!strings.Contains(string(loginBody), `data-theme-toggle`) ||
		strings.Contains(string(loginBody), `data-theme-select`) ||
		!strings.Contains(string(loginBody), "ArtistTrackarr") ||
		strings.Contains(string(loginBody), "Artist Trackarr") ||
		strings.Contains(string(loginBody), "Artist Tracker") {
		t.Fatalf("login branding missing or stale: %q", loginBody)
	}
	for _, asset := range []struct {
		path        string
		contentType string
	}{
		{"/static/favicon.ico", "image/"},
		{"/static/favicon-32.png", "image/png"},
		{"/static/apple-touch-icon.png", "image/png"},
		{"/static/logo-full.png", "image/png"},
		{"/static/theme.js", "text/javascript"},
		{"/static/logo-mark.png", "image/png"},
	} {
		staticResponse, err := client.Get(server.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, staticResponse.Body); err != nil {
			t.Fatal(err)
		}
		_ = staticResponse.Body.Close()
		if staticResponse.StatusCode != http.StatusOK ||
			!strings.HasPrefix(staticResponse.Header.Get("Content-Type"), asset.contentType) {
			t.Fatalf("static asset %s status/type = %d, %q", asset.path,
				staticResponse.StatusCode, staticResponse.Header.Get("Content-Type"))
		}
	}
	rangeRequest, err := http.NewRequest(http.MethodGet, server.URL+"/static/app.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	rangeRequest.Header.Set("Range", "bytes=0-15")
	rangeRequest.Header.Set("Accept-Encoding", "gzip")
	rangeResponse, err := client.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rangeResponse.Body)
	_ = rangeResponse.Body.Close()
	if rangeResponse.StatusCode != http.StatusOK || rangeResponse.Header.Get("Accept-Ranges") != "none" || rangeResponse.Header.Get("Content-Range") != "" {
		t.Fatalf("static range request status/headers=%d accept-ranges=%q content-range=%q", rangeResponse.StatusCode, rangeResponse.Header.Get("Accept-Ranges"), rangeResponse.Header.Get("Content-Range"))
	}
	if rangeResponse.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("static range request content-encoding=%q, want gzip", rangeResponse.Header.Get("Content-Encoding"))
	}

	csrf := getCSRF(t, client, server.URL+"/setup")
	response := postForm(t, client, server.URL+"/setup", url.Values{
		"_csrf": {csrf}, "setup_token": {"incorrect setup token"}, "email": {"admin@example.com"},
		"username": {"administrator"}, "password": {"a secure test password"}, "timezone": {"Europe/Amsterdam"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid setup token status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/setup")
	response = postForm(t, client, server.URL+"/setup", url.Values{
		"_csrf": {csrf}, "setup_token": {cfg.SetupToken}, "email": {"admin@example.com"},
		"username": {"x"}, "password": {"a secure test password"}, "timezone": {"Europe/Amsterdam"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid setup username status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/setup")
	setupResponse := postForm(t, client, server.URL+"/setup", url.Values{
		"_csrf": {csrf}, "setup_token": {cfg.SetupToken}, "email": {"admin@example.com"},
		"password": {"a secure test password"}, "timezone": {"Europe/Amsterdam"},
	})
	if _, err := io.Copy(io.Discard, setupResponse.Body); err != nil {
		t.Fatal(err)
	}
	_ = setupResponse.Body.Close()
	setupRedirectClient := noRedirectClient(client)
	setupPage, err := setupRedirectClient.Get(server.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	_ = setupPage.Body.Close()
	if setupPage.StatusCode != http.StatusSeeOther || setupPage.Header.Get("Location") != "/login" {
		t.Fatalf("completed setup GET status=%d location=%q", setupPage.StatusCode, setupPage.Header.Get("Location"))
	}
	csrf = getCSRF(t, client, server.URL+"/login")
	setupConflict, err := setupRedirectClient.PostForm(server.URL+"/setup", url.Values{
		"_csrf": {csrf}, "setup_token": {cfg.SetupToken}, "email": {"second-admin@example.com"},
		"password": {"another secure test password"}, "timezone": {"UTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = setupConflict.Body.Close()
	if setupConflict.StatusCode != http.StatusConflict {
		t.Fatalf("completed setup POST status=%d", setupConflict.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/login")
	response = postForm(t, client, server.URL+"/login", url.Values{
		"_csrf": {csrf}, "email": {"admin@example.com"}, "password": {"a secure test password"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Never miss the next record") ||
		!strings.Contains(string(body), "/static/logo-mark.png") ||
		!strings.Contains(string(body), "Manage your followed artists") ||
		!strings.Contains(string(body), `href="/artists"`) ||
		!strings.Contains(string(body), "ArtistTrackarr") ||
		strings.Contains(string(body), "Artist Trackarr") ||
		strings.Contains(string(body), "Artist Tracker") ||
		!strings.Contains(string(body), `href="/artists"`) ||
		!strings.Contains(string(body), `data-mobile-menu-toggle`) ||
		!strings.Contains(string(body), `id="site-navigation"`) {
		t.Fatalf("dashboard status/body = %d, %q", response.StatusCode, body)
	}
	if strings.Contains(string(body), "Reminder settings") || strings.Contains(string(body), `action="/profile"`) {
		t.Fatalf("dashboard still contains reminder settings: %q", body)
	}
	response, err = client.Get(server.URL + "/coverage")
	if err != nil {
		t.Fatal(err)
	}
	coverageBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(coverageBody), "Release Trust Center") ||
		!strings.Contains(string(coverageBody), "Follow an artist to see provider coverage here") {
		t.Fatalf("coverage status/body = %d, %q", response.StatusCode, coverageBody)
	}
	response, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Reminder settings") ||
		!strings.Contains(string(body), `action="/admin/profile"`) {
		t.Fatalf("admin reminder settings status/body = %d, %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/profile", url.Values{
		"_csrf": {csrf}, "timezone": {"America/New_York"}, "reminder_time": {"08:30"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Reminder settings updated") {
		t.Fatalf("admin profile update status/body = %d, %q", response.StatusCode, body)
	}
	updatedUser, err := database.UserByEmail(context.Background(), "admin@example.com")
	if err != nil || updatedUser.Timezone != "America/New_York" || updatedUser.ReminderTime != "08:30" {
		t.Fatalf("updated admin profile = %#v, %v", updatedUser, err)
	}
	response, err = client.Get(server.URL + "/artists/search?q=Laura+pausini")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "MusicBrainz is temporarily unavailable") ||
		strings.Contains(string(body), ">EOF<") {
		t.Fatalf("search failure status/body = %d, %q", response.StatusCode, body)
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
	_ = response.Body.Close()

	csrf = getCSRF(t, client, server.URL+"/destinations")
	response = postForm(t, client, server.URL+"/destinations/1/rename", url.Values{
		"_csrf": {csrf}, "name": {"My phone"},
	})
	_ = response.Body.Close()
	destination, err := database.Destination(context.Background(), user.ID, 1)
	if err != nil || destination.Name != "My phone" {
		t.Fatalf("renamed destination = %#v, %v", destination, err)
	}
	art, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "artwork-test-artist", Name: "Artwork Test Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, art.ID); err != nil {
		t.Fatal(err)
	}
	const artworkMBID = "6e335887-60ba-38f0-95af-fae7774336bf"
	if _, err := database.DB.ExecContext(context.Background(), `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, artworkMBID, art.ID, "Artwork Test Release", "Album", "[]", "2026-08-01", 3,
		"https://musicbrainz.org/release-group/"+artworkMBID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	response, err = client.Get(server.URL + "/art/release-group/" + artworkMBID)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Artwork-Status") != "test" ||
		response.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("artwork response status=%d headers=%v", response.StatusCode, response.Header)
	}
	_ = response.Body.Close()
}

func TestSearchUsesSpotifyBeforeMusicBrainz(t *testing.T) {
	mb := &searchCatalog{results: []catalog.ArtistResult{{MBID: "should-not-appear", Name: "MusicBrainz"}}}
	spotify := &fakeSpotify{results: []catalog.SpotifyArtist{{
		ID: "0OdUWJ0sBjDrqHygGUXeCF", Name: "Spotify Result",
		URL: "https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF", ImageURL: "https://i.scdn.co/example",
	}}}
	_, server, client := authenticatedTestServer(t, mb, spotify, nil)
	response, err := client.Get(server.URL + "/artists/search?q=example")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Spotify Result") ||
		!strings.Contains(string(body), "/artists/follow/spotify") {
		t.Fatalf("Spotify search status/body=%d %q", response.StatusCode, body)
	}
	if spotify.searchCalls != 1 || mb.calls != 0 {
		t.Fatalf("Spotify calls=%d MusicBrainz calls=%d", spotify.searchCalls, mb.calls)
	}
}

func TestReleaseGroupArtworkIsOwnerScopedAndRateLimited(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	member, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "artwork-owner-artist", Name: "Artwork Owner Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, member.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	const ownedMBID = "11111111-1111-4111-8111-111111111111"
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, ownedMBID, artist.ID, "Owned artwork", "Album", "2026-08-01", 3,
		"https://musicbrainz.org/release-group/"+ownedMBID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/art/release-group/" + ownedMBID)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owned artwork status=%d", response.StatusCode)
	}

	response, err = client.Get(server.URL + "/art/release-group/not-a-mbid")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid artwork status=%d, want 404", response.StatusCode)
	}

	otherID, err := database.CreateUser(ctx, "other@example.com", "other-user", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	otherArtist, err := database.UpsertArtist(ctx, store.Artist{MBID: "artwork-foreign-artist", Name: "Artwork Foreign Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, otherID, otherArtist.ID); err != nil {
		t.Fatal(err)
	}
	const foreignMBID = "22222222-2222-4222-8222-222222222222"
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, foreignMBID, otherArtist.ID, "Foreign artwork", "Album", "2026-08-01", 3,
		"https://musicbrainz.org/release-group/"+foreignMBID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(server.URL + "/art/release-group/" + foreignMBID)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign artwork status=%d, want 404", response.StatusCode)
	}

	for request := 0; request < 119; request++ {
		response, err = client.Get(server.URL + "/art/release-group/" + ownedMBID)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("artwork request %d status=%d", request+1, response.StatusCode)
		}
	}
	response, err = client.Get(server.URL + "/art/release-group/" + ownedMBID)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited artwork status=%d, want 429", response.StatusCode)
	}
}

func TestLoginRejectsInvalidAndUnknownCredentials(t *testing.T) {
	_, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/")
	response := postForm(t, client, server.URL+"/logout", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	csrf = getCSRF(t, client, server.URL+"/login")
	response = postForm(t, client, server.URL+"/login", url.Values{
		"_csrf": {csrf}, "email": {"member@example.com"}, "password": {"wrong password"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Email or password is incorrect") {
		t.Fatalf("invalid password status/body=%d %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/login")
	response = postForm(t, client, server.URL+"/login", url.Values{
		"_csrf": {csrf}, "email": {"missing@example.com"}, "password": {"wrong password"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Email or password is incorrect") {
		t.Fatalf("unknown email status/body=%d %q", response.StatusCode, body)
	}
}

func TestLoginThrottleRendersRetryResponseAfterRepeatedFailures(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/")
	response := postForm(t, client, server.URL+"/logout", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	for attempt := 0; attempt < 5; attempt++ {
		csrf = getCSRF(t, client, server.URL+"/login")
		response = postForm(t, client, server.URL+"/login", url.Values{
			"_csrf": {csrf}, "email": {"member@example.com"}, "password": {"wrong password"},
		})
		if response.StatusCode != http.StatusUnauthorized {
			_ = response.Body.Close()
			t.Fatalf("failed login attempt %d status=%d", attempt+1, response.StatusCode)
		}
		_ = response.Body.Close()
	}
	csrf = getCSRF(t, client, server.URL+"/login")
	response = postForm(t, client, server.URL+"/login", url.Values{
		"_csrf": {csrf}, "email": {"member@example.com"}, "password": {"wrong password"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "Too many attempts") {
		t.Fatalf("throttled login status/body=%d %q", response.StatusCode, body)
	}
	var keys int
	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM login_attempts`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 2 {
		t.Fatalf("login failures created %d throttle keys, want peer and account keys", keys)
	}
}

func TestSearchFailureLogDoesNotContainQuery(t *testing.T) {
	var logs bytes.Buffer
	app := &App{
		mb:     &searchCatalog{err: errors.New(`Get "https://musicbrainz.org/ws/2/artist?query=private-artist": EOF`)},
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	d := PageData{PageDiscovery: PageDiscovery{Query: "private-artist"}}
	app.populateSearch(context.Background(), &d)
	if strings.Contains(logs.String(), "private-artist") || strings.Contains(logs.String(), "musicbrainz.org") {
		t.Fatalf("search failure log leaked query or URL: %q", logs.String())
	}
}

func TestSettingsOwnsUsernameAndNotificationManagement(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	response, err := client.Get(server.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(page, `name="username"`) ||
		!strings.Contains(page, "Your destinations") || !strings.Contains(page, "Notification preferences") ||
		!strings.Contains(page, `name="digest_enabled"`) ||
		!strings.Contains(page, `name="digest_frequency"`) || strings.Contains(page, `href="/destinations"`) {
		t.Fatalf("settings page missing consolidated account controls: %q", page)
	}

	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err = noFollow.Get(server.URL + "/destinations")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/settings" {
		t.Fatalf("destinations compatibility redirect status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}

	csrf := getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/profile", url.Values{
		"_csrf": {csrf}, "username": {"Listener"}, "timezone": {"UTC"}, "reminder_time": {"09:00"},
	})
	_ = response.Body.Close()
	user, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil || user.Username != "Listener" {
		t.Fatalf("updated username=%q err=%v", user.Username, err)
	}

	response = postForm(t, client, server.URL+"/destinations", url.Values{
		"_csrf": {csrf}, "name": {"Phone"}, "service": {"ntfy"}, "host": {"ntfy.sh"}, "topic": {"artisttrackarr"},
	})
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("destination add status=%d", response.StatusCode)
	}
	pageBytes, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(pageBytes), "Phone") {
		t.Fatalf("destination missing from settings response: %q", pageBytes)
	}
	response = postForm(t, client, server.URL+"/settings/preferences", url.Values{
		"_csrf": {csrf}, "albums": {"on"}, "eps": {"on"}, "singles": {"on"},
		"announcements": {"on"}, "release_day": {"on"}, "hold_conflicting_notifications": {"on"},
	})
	_ = response.Body.Close()
	preferences, err := database.NotificationPreferences(context.Background(), user.ID)
	if err != nil || !preferences.HoldConflictingNotifications {
		t.Fatalf("conflict hold preference=%v err=%v", preferences.HoldConflictingNotifications, err)
	}
}

func TestSettingsPasswordChangeRevokesSessions(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	currentHash, err := security.HashPassword("current password long enough")
	if err != nil {
		t.Fatal(err)
	}
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, currentHash, user.ID); err != nil {
		t.Fatal(err)
	}

	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, server.URL+"/settings/password", url.Values{
		"_csrf": {csrf}, "current_password": {"wrong password"},
		"new_password": {"new password long enough"}, "confirm_password": {"new password long enough"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "current password is incorrect") {
		t.Fatalf("wrong current password status/body=%d %q", response.StatusCode, body)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	noFollow := noRedirectClient(client)
	response, err = noFollow.PostForm(server.URL+"/settings/password", url.Values{
		"_csrf": {csrf}, "current_password": {"current password long enough"},
		"new_password": {"new password long enough"}, "confirm_password": {"new password long enough"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	wantLocation := "/login?message=" + url.QueryEscape(security.SignedToken("the session secret has more than 32 bytes", "Password updated"))
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != wantLocation {
		t.Fatalf("password change status/location=%d %q", response.StatusCode, response.Header.Get("Location"))
	}
	updated, err := database.UserByID(ctx, user.ID)
	if err != nil || !security.CheckPassword(updated.PasswordHash, "new password long enough") {
		t.Fatalf("password was not updated: err=%v", err)
	}
	response, err = noFollow.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login" {
		t.Fatalf("revoked session status/location=%d %q", response.StatusCode, response.Header.Get("Location"))
	}
}

func TestSettingsPreferencesRedirectsOnStoreFailure(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "settings-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	userID, err := database.CreateUser(context.Background(), "settings-error@example.com", "hash", "member", "UTC", "settings-error")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/settings/preferences", strings.NewReader(url.Values{
		"albums": {"on"}, "digest_frequency": {"daily"},
	}.Encode())).WithContext(context.WithValue(ctx, sessionKey, store.Session{User: store.User{ID: userID}}))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	app.settingsPreferences(response, request)
	if response.Code != http.StatusSeeOther || strings.Contains(response.Header().Get("Location"), "context+canceled") ||
		!strings.Contains(response.Header().Get("Location"), "Notification+preferences+could+not+be+saved") {
		t.Fatalf("settings preferences failure status=%d location=%q", response.Code, response.Header().Get("Location"))
	}

	compatCtx, compatCancel := context.WithCancel(context.Background())
	compatCancel()
	compatRequest := httptest.NewRequest(http.MethodPost, "/preferences", strings.NewReader("albums=on"))
	compatRequest = compatRequest.WithContext(context.WithValue(compatCtx, sessionKey, store.Session{User: store.User{ID: userID}}))
	compatRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	compatResponse := httptest.NewRecorder()
	app.updatePreferences(compatResponse, compatRequest)
	if compatResponse.Code != http.StatusSeeOther || strings.Contains(compatResponse.Header().Get("Location"), "context+canceled") ||
		!strings.Contains(compatResponse.Header().Get("Location"), "Notification+preferences+could+not+be+saved") {
		t.Fatalf("compatibility preferences failure status=%d location=%q", compatResponse.Code, compatResponse.Header().Get("Location"))
	}
}

func TestCalendarPageAndICSExportAreOwnerScoped(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "88888888-8888-4888-8888-888888888888", Name: "Calendar Web Artist", SortName: "Calendar Web Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	releaseDate := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:calendar-web", ITunesID: "calendar-web", Title: "Calendar Web Release", PrimaryType: "Album",
			FirstReleaseDate: releaseDate, DatePrecision: 3, ITunesURL: "https://music.apple.com/us/album/calendar-web-release",
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	month := releaseDate[:7]
	response, err := client.Get(server.URL + "/calendar?month=" + month)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(page, "Release calendar") ||
		!strings.Contains(page, "Calendar Web Release") || !strings.Contains(page, "Download ICS") ||
		!strings.Contains(page, `href="/releases/1"`) {
		t.Fatalf("calendar status/body=%d %q", response.StatusCode, body)
	}
	response, err = client.Get(server.URL + "/calendar.ics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	ics := string(body)
	unfolded := strings.ReplaceAll(ics, "\r\n ", "")
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/calendar; charset=utf-8" ||
		response.Header.Get("Content-Disposition") != `attachment; filename="artisttrackarr-releases.ics"` ||
		response.Header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(unfolded, "BEGIN:VCALENDAR") || !strings.Contains(unfolded, "SUMMARY:Calendar Web Release — Calendar Web Artist") ||
		!strings.Contains(unfolded, "DTSTART;VALUE=DATE:"+strings.ReplaceAll(releaseDate, "-", "")) ||
		!strings.Contains(unfolded, "https://music.apple.com/us/album/calendar-web-release") {
		t.Fatalf("calendar ICS status/headers/body=%d %q %v", response.StatusCode, response.Header, body)
	}
}

func TestSearchFallsBackToMusicBrainz(t *testing.T) {
	tests := []struct {
		name    string
		spotify *fakeSpotify
		notice  string
	}{
		{name: "provider failure", spotify: &fakeSpotify{searchErr: io.ErrUnexpectedEOF}, notice: "Spotify is temporarily unavailable"},
		{name: "no results", spotify: &fakeSpotify{}, notice: "No Spotify matches were found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mb := &searchCatalog{results: []catalog.ArtistResult{{MBID: "musicbrainz-id", Name: "Fallback Result"}}}
			_, server, client := authenticatedTestServer(t, mb, test.spotify, nil)
			response, err := client.Get(server.URL + "/artists/search?q=example")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Fallback Result") ||
				!strings.Contains(string(body), test.notice) {
				t.Fatalf("fallback status/body=%d %q", response.StatusCode, body)
			}
			if test.spotify.searchCalls != 1 || mb.calls != 1 {
				t.Fatalf("Spotify calls=%d MusicBrainz calls=%d", test.spotify.searchCalls, mb.calls)
			}
		})
	}
}

func TestSearchUsesITunesBeforeMusicBrainzWhenSpotifyIsUnavailable(t *testing.T) {
	mb := &searchCatalog{results: []catalog.ArtistResult{{MBID: "musicbrainz-fallback", Name: "MusicBrainz fallback"}}}
	itunes := &actionITunes{artists: map[string]catalog.ITunesArtist{
		"123": {ID: "123", Name: "Apple result", URL: "https://music.apple.com/us/artist/apple-result/123"},
	}}
	spotify := &fakeSpotify{searchErr: errors.New("Spotify unavailable")}
	_, server, client := authenticatedTestServerWithITunes(t, mb, spotify, itunes, nil)
	response, err := client.Get(server.URL + "/artists/search?q=apple")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Apple result") ||
		!strings.Contains(string(body), "Apple/iTunes discovery") || mb.calls != 0 {
		t.Fatalf("iTunes search status/body=%d %q mb calls=%d", response.StatusCode, body, mb.calls)
	}

	itunes.searchErr = errors.New("iTunes unavailable")
	response, err = client.Get(server.URL + "/artists/search?q=apple")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "MusicBrainz fallback") ||
		!strings.Contains(string(body), "Spotify and Apple/iTunes discovery are unavailable") || mb.calls != 1 {
		t.Fatalf("MusicBrainz fallback status/body=%d %q mb calls=%d", response.StatusCode, body, mb.calls)
	}
}

func TestSpotifyFollowCreatesOnePendingResolution(t *testing.T) {
	const spotifyID = "0OdUWJ0sBjDrqHygGUXeCF"
	mb := &searchCatalog{}
	spotify := &fakeSpotify{artist: catalog.SpotifyArtist{
		ID: spotifyID, Name: "Pending Artist",
		URL: "https://open.spotify.com/artist/" + spotifyID, ImageURL: "https://i.scdn.co/example",
	}}
	database, server, client := authenticatedTestServer(t, mb, spotify, func(database *store.Store) *jobs.Runner {
		return jobs.New(
			database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	})
	response, err := client.PostForm(server.URL+"/artists/follow/spotify", url.Values{"spotify_id": {spotifyID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("Spotify follow without CSRF status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	for range 2 {
		response = postForm(t, client, server.URL+"/artists/follow/spotify", url.Values{
			"_csrf": {csrf}, "spotify_id": {spotifyID},
		})
		_ = response.Body.Close()
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	resolutions, err := database.ArtistResolutions(context.Background(), user.ID)
	if err != nil || len(resolutions) != 1 || resolutions[0].DisplayName != "Pending Artist" {
		t.Fatalf("pending resolutions=%#v err=%v", resolutions, err)
	}
}

func TestSpotifyBatchFollowSelection(t *testing.T) {
	const firstID = "0OdUWJ0sBjDrqHygGUXeCF"
	const secondID = "1OdUWJ0sBjDrqHygGUXeCG"
	mb := &searchCatalog{}
	spotify := &fakeSpotify{
		results: []catalog.SpotifyArtist{
			{ID: firstID, Name: "First", URL: "https://open.spotify.com/artist/" + firstID},
			{ID: secondID, Name: "Second", URL: "https://open.spotify.com/artist/" + secondID},
		},
		artists: map[string]catalog.SpotifyArtist{
			firstID:  {ID: firstID, Name: "First", URL: "https://open.spotify.com/artist/" + firstID},
			secondID: {ID: secondID, Name: "Second", URL: "https://open.spotify.com/artist/" + secondID},
		},
	}
	database, server, client := authenticatedTestServer(t, mb, spotify, nil)
	response, err := client.Get(server.URL + "/artists/search?q=artists")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), `name="spotify_ids"`) ||
		!strings.Contains(string(body), `data-select-all`) ||
		!strings.Contains(string(body), `/artists/follow/spotify/batch`) {
		t.Fatalf("Spotify multi-select markup missing: %q", body)
	}
	response, err = client.PostForm(server.URL+"/artists/follow/spotify/batch", url.Values{
		"spotify_ids": {firstID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("batch follow without CSRF status=%d", response.StatusCode)
	}

	csrf := getCSRF(t, client, server.URL+"/artists/search")
	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", url.Values{
		"_csrf":       {csrf},
		"spotify_ids": {firstID, firstID, secondID, "invalid"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "2 queued") ||
		!strings.Contains(string(body), "1 failed") {
		t.Fatalf("batch follow status/body=%d %q", response.StatusCode, body)
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	resolutions, err := database.ArtistResolutions(context.Background(), user.ID)
	if err != nil || len(resolutions) != 2 {
		t.Fatalf("resolutions=%#v err=%v", resolutions, err)
	}

	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty selection status=%d", response.StatusCode)
	}
	tooMany := url.Values{"_csrf": {csrf}}
	for i := range 11 {
		tooMany.Add("spotify_ids", fmt.Sprintf("%022d", i))
	}
	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", tooMany)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized selection status=%d", response.StatusCode)
	}
}

func TestSpotifyBatchLookupHandlesMissingAndProviderErrors(t *testing.T) {
	const knownID = "0OdUWJ0sBjDrqHygGUXeCF"
	const missingID = "1OdUWJ0sBjDrqHygGUXeCG"
	spotify := &fakeSpotify{artists: map[string]catalog.SpotifyArtist{
		knownID: {ID: knownID, Name: "Known Artist", URL: "https://open.spotify.com/artist/" + knownID},
	}}
	_, server, client := authenticatedTestServer(t, &searchCatalog{}, spotify, nil)
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, server.URL+"/artists/follow/spotify/batch", url.Values{
		"_csrf": {csrf}, "spotify_ids": {knownID, missingID},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "1 queued") ||
		!strings.Contains(string(body), "1 failed") {
		t.Fatalf("missing batch artist status/body=%d %q", response.StatusCode, body)
	}

	spotify.batchErr = errors.New("Spotify batch unavailable")
	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", url.Values{
		"_csrf": {csrf}, "spotify_ids": {knownID},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "0 queued") ||
		!strings.Contains(string(body), "1 failed") {
		t.Fatalf("batch provider error status/body=%d %q", response.StatusCode, body)
	}
}

func TestMusicBrainzBatchFollowAndArtistsPage(t *testing.T) {
	const firstMBID = "11111111-1111-4111-8111-111111111111"
	const secondMBID = "22222222-2222-4222-8222-222222222222"
	mb := &searchCatalog{
		results: []catalog.ArtistResult{
			{MBID: firstMBID, Name: "First Artist"},
			{MBID: secondMBID, Name: "Second Artist"},
		},
		resolved: map[string]catalog.ArtistResult{
			firstMBID:  {MBID: firstMBID, Name: "First Artist", SortName: "First Artist"},
			secondMBID: {MBID: secondMBID, Name: "Second Artist", SortName: "Second Artist"},
		},
		resolveError: map[string]error{"broken": errors.New("provider failure")},
	}
	database, server, client := authenticatedTestServer(t, mb, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/artists/search?q=artists")
	response := postForm(t, client, server.URL+"/artists/follow/batch", url.Values{
		"_csrf": {csrf},
		"mbids": {firstMBID, firstMBID, secondMBID, "broken"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "2 added") ||
		!strings.Contains(string(body), "1 failed") {
		t.Fatalf("MusicBrainz batch status/body=%d %q", response.StatusCode, body)
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	count, err := database.FollowedArtistCount(context.Background(), user.ID)
	if err != nil || count != 2 {
		t.Fatalf("follow count=%d err=%v", count, err)
	}

	response, err = client.Get(server.URL + "/artists")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "First Artist") ||
		!strings.Contains(string(body), "Second Artist") ||
		!strings.Contains(string(body), "Initial release synchronization pending") {
		t.Fatalf("artists page status/body=%d %q", response.StatusCode, body)
	}
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "<strong>2</strong>") || !strings.Contains(string(body), "Watchlist assurance") || !strings.Contains(string(body), "First Artist") {
		t.Fatalf("dashboard count/list body=%q", body)
	}

	response = postForm(t, client, server.URL+"/artists/follow/batch", url.Values{
		"_csrf": {csrf}, "mbids": {firstMBID, secondMBID},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "0 added") || !strings.Contains(string(body), "2 already followed") {
		t.Fatalf("duplicate batch body=%q", body)
	}
}

func TestArtistSearchAndOwnerScopedCSVExport(t *testing.T) {
	mb := &searchCatalog{}
	database, server, client := authenticatedTestServer(t, mb, nil, nil)
	ctx := context.Background()
	user, _ := database.UserByEmail(ctx, "member@example.com")
	otherID, _ := database.CreateUser(ctx, "other@example.com", "unused", "member", "UTC", "")
	followed, err := database.UpsertArtist(ctx, store.Artist{
		MBID: "11111111-1111-4111-8111-111111111111", Name: "Comma, Artist", SortName: "Artist, Comma",
		SpotifyID:  "0OdUWJ0sBjDrqHygGUXeCF",
		SpotifyURL: "https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF",
	})
	if err != nil {
		t.Fatal(err)
	}
	notFollowed, err := database.UpsertArtist(ctx, store.Artist{
		MBID: "22222222-2222-4222-8222-222222222222", Name: "Other User Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, followed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, otherID, notFollowed.ID); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/artists/search")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(page, "<h1>Artists</h1>") ||
		!strings.Contains(page, `href="/artists/export"`) ||
		!strings.Contains(page, "Export CSV") ||
		strings.Contains(page, `action="/imports"`) ||
		strings.Contains(page, "Review and import") {
		t.Fatalf("combined artist tools status/body=%d %q", response.StatusCode, body)
	}

	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err = noRedirect.Get(server.URL + "/imports")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed import endpoint status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	response, err = noRedirect.PostForm(server.URL+"/imports", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed import POST endpoint status=%d", response.StatusCode)
	}

	response, err = client.Get(server.URL + "/artists/export")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("Content-Type") != "text/csv; charset=utf-8" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), "artist-trackarr-watched-artists-") ||
		response.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("export headers status=%d headers=%v", response.StatusCode, response.Header)
	}
	records, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || len(records[0]) != 11 ||
		records[0][0] != "artist" ||
		records[1][0] != "https://musicbrainz.org/artist/11111111-1111-4111-8111-111111111111" ||
		records[1][1] != "Comma, Artist" ||
		records[1][2] != "Artist, Comma" ||
		records[1][3] != "" ||
		records[1][6] != "11111111-1111-4111-8111-111111111111" ||
		records[1][8] != "0OdUWJ0sBjDrqHygGUXeCF" ||
		strings.Contains(string(body), "Other User Artist") {
		t.Fatalf("unexpected owner-scoped CSV records=%#v body=%q", records, body)
	}
}

func TestNeutralizeCSVCellPreventsFormulaInterpretation(t *testing.T) {
	for _, value := range []string{"=HYPERLINK(\"https://example.test\")", "+1", "-1", "@cmd", "  =formula"} {
		got := neutralizeCSVCell(value)
		if !strings.HasPrefix(got, "'") {
			t.Fatalf("formula-leading value %q was not neutralized: %q", value, got)
		}
	}
	if got := neutralizeCSVCell("safe"); got != "safe" {
		t.Fatalf("safe CSV value changed to %q", got)
	}
}

func TestArtistsPagePaginatesAndPreservesFilters(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 51 {
		artist, err := database.UpsertArtist(ctx, store.Artist{
			MBID: fmt.Sprintf("paged-web-%02d", i), Name: fmt.Sprintf("Paged Artist %02d", i),
			SortName: fmt.Sprintf("Paged Artist %02d", i), Type: "Person", Country: "NL", Genres: []string{"Pop"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}

	response, err := client.Get(server.URL + "/artists?q=Paged&genre=Pop&country=NL&type=Person&page=2")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(page, "Paged Artist 50") ||
		strings.Contains(page, "Paged Artist 00") || !strings.Contains(page, "Showing 51 artists") ||
		!strings.Contains(page, "Showing 51–51 of 51") {
		t.Fatalf("paginated artists status/body=%d %q", response.StatusCode, body)
	}
	for _, value := range []string{"page=1", "country=NL", "genre=Pop", "q=Paged", "type=Person"} {
		if !strings.Contains(page, value) {
			t.Fatalf("pagination link lost %q: %q", value, body)
		}
	}
}

func TestArtistCSVImportProcessesRowsAndScopesResults(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	mbid := "11111111-1111-4111-8111-111111111111"
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	_ = writer.WriteField("_csrf", csrf)
	part, err := writer.CreateFormFile("file", "artists.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "artist,display_name,sort_name,artist_type,country,disambiguation,musicbrainz_id,musicbrainz_url,spotify_id,spotify_url,spotify_image_url\n"+
		"https://musicbrainz.org/artist/"+mbid+",Imported Artist,Artist Imported,Group,NL,aka Import,"+mbid+",https://musicbrainz.org/artist/"+mbid+",0OdUWJ0sBjDrqHygGUXeCF,https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF,https://i.scdn.co/image\n"+
		"bad,Broken,,bad,,\n")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/artists/import", &payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Import results") ||
		!strings.Contains(string(body), "Imported Artist") || !strings.Contains(string(body), ">added<") || !strings.Contains(string(body), ">invalid<") {
		t.Fatalf("import response status/body=%d %q", response.StatusCode, body)
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	if count, err := database.FollowedArtistCount(context.Background(), user.ID); err != nil || count != 1 {
		t.Fatalf("imported follow count=%d err=%v", count, err)
	}
	var sortName, artistType, country, disambiguation, image string
	if err := database.DB.QueryRow(`SELECT sort_name,artist_type,country,disambiguation,spotify_image_url FROM artists WHERE mbid=?`, mbid).
		Scan(&sortName, &artistType, &country, &disambiguation, &image); err != nil {
		t.Fatal(err)
	}
	if sortName != "Artist Imported" || artistType != "Group" || country != "NL" ||
		disambiguation != "aka Import" || image != "https://i.scdn.co/image" {
		t.Fatalf("imported metadata lost: sort=%q type=%q country=%q disambiguation=%q image=%q",
			sortName, artistType, country, disambiguation, image)
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		!strings.Contains(response.Header.Get("Content-Security-Policy"), "https://*.mzstatic.com") ||
		!strings.Contains(response.Header.Get("Content-Security-Policy"), "https://*.itunes.apple.com") {
		t.Fatalf("import response CSP=%q", response.Header.Get("Content-Security-Policy"))
	}
}

func TestAdminDeliveryAuditAndAuthorization(t *testing.T) {
	mb := &searchCatalog{}
	database, server, client := authenticatedTestServer(t, mb, nil, nil)
	targetID, err := database.CreateUser(context.Background(), "delete-me@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member admin status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/")
	response = postForm(t, client, server.URL+"/admin/profile", url.Values{
		"_csrf": {csrf}, "timezone": {"America/New_York"}, "reminder_time": {"08:30"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member admin profile status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(targetID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member user deletion status=%d", response.StatusCode)
	}
	if _, err := database.UserByID(context.Background(), targetID); err != nil {
		t.Fatalf("member deleted another user: %v", err)
	}

	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	if _, err := database.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(context.Background(), user.ID, "Kitchen display", "ntfy", []byte("encrypted-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE destinations SET created_at=? WHERE user_id=?`, time.Date(2026, 7, 30, 7, 0, 0, 0, time.UTC).Format(time.RFC3339Nano), user.ID); err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "33333333-3333-4333-8333-333333333333", Name: "Audit Artist", SortName: "Audit Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyReleaseSync(context.Background(), artist, []store.Release{{
		MBID:             "44444444-4444-4444-8444-444444444444",
		Title:            "Audited Album",
		PrimaryType:      "Album",
		FirstReleaseDate: "2026-07-30",
		DatePrecision:    3,
		MusicBrainzURL:   "https://musicbrainz.org/release-group/44444444-4444-4444-8444-444444444444",
	}}, time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	deliveries, err := database.DueDeliveries(context.Background(), time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC), 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("due deliveries=%#v err=%v", deliveries, err)
	}
	if err := database.MarkDeliveryFailed(
		context.Background(), deliveries[0].ID, 5, "provider rejected secret detail",
		time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	dashboardBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(dashboardBody), "delivery-scroll") ||
		strings.Contains(string(dashboardBody), "provider rejected secret detail") {
		t.Fatalf("dashboard delivery log is not compact: %q", dashboardBody)
	}
	response, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	for _, expected := range []string{
		"member@example.com", "Audited Album", "Kitchen display", "ntfy", "failed", "5 attempts",
		"View details", "Household accounts", "delete-me@example.com", "Retention governance",
		"/admin/users/" + strconv.FormatInt(targetID, 10) + "/delete", "Current account",
	} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("admin audit missing %q in %q", expected, body)
		}
	}
	if strings.Contains(string(body), "encrypted-secret") {
		t.Fatalf("admin audit exposed encrypted destination: %q", body)
	}
	if strings.Contains(string(body), "provider rejected secret detail") {
		t.Fatalf("admin summary exposed notification error without explicit access: %q", body)
	}
	detailResponse, err := client.Get(server.URL + "/admin/deliveries/" + strconv.FormatInt(deliveries[0].ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	detailBody, _ := io.ReadAll(detailResponse.Body)
	_ = detailResponse.Body.Close()
	if detailResponse.StatusCode != http.StatusOK || !strings.Contains(string(detailBody), "provider rejected secret detail") {
		t.Fatalf("admin delivery detail status/body=%d %q", detailResponse.StatusCode, detailBody)
	}
	bad := noRedirectClient(client)
	response, err = bad.Get(server.URL + "/admin/deliveries/not-an-id")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid admin delivery ID status=%d", response.StatusCode)
	}
	response, err = bad.Get(server.URL + "/admin/deliveries/999999")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing admin delivery status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(user.ID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	selfDeleteBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(selfDeleteBody), "cannot delete your own account") {
		t.Fatalf("self deletion status/body=%d %q", response.StatusCode, selfDeleteBody)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(targetID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	deleteBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(deleteBody), "User deleted") {
		t.Fatalf("admin deletion status/body=%d %q", response.StatusCode, deleteBody)
	}
	if _, err := database.UserByID(context.Background(), targetID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted user lookup error=%v", err)
	}
}

func TestArtistResolutionReviewAndOwnerScope(t *testing.T) {
	mb := &searchCatalog{}
	database, server, client := authenticatedTestServer(t, mb, nil, func(database *store.Store) *jobs.Runner {
		return jobs.New(
			database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	})
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	resolution, _, err := database.CreateArtistResolution(context.Background(), user.ID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkArtistResolutionReview(context.Background(), user.ID, resolution.ID, []store.ResolutionCandidate{{
		MBID: "candidate-mbid", Name: "Example", SortName: "Example", Type: "Group",
	}}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/artist-resolutions/" + strconv.FormatInt(resolution.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Choose the MusicBrainz artist") {
		t.Fatalf("review status/body=%d %q", response.StatusCode, body)
	}
	csrf := getCSRF(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10))
	response = postForm(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10), url.Values{
		"_csrf": {csrf}, "mbid": {"candidate-mbid"},
	})
	_ = response.Body.Close()
	followed, err := database.FollowedArtists(context.Background(), user.ID)
	if err != nil || len(followed) != 1 || followed[0].MBID != "candidate-mbid" {
		t.Fatalf("reviewed follow=%#v err=%v", followed, err)
	}

	otherID, _ := database.CreateUser(context.Background(), "other@example.com", "unused", "member", "UTC", "")
	otherResolution, _, _ := database.CreateArtistResolution(context.Background(), otherID, "spotify", "other-spotify", "Other", "https://open.spotify.com/artist/other-spotify", "")
	response, err = client.Get(server.URL + "/artist-resolutions/" + strconv.FormatInt(otherResolution.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user review status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(otherResolution.ID, 10)+"/cancel", url.Values{
		"_csrf": {csrf},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user resolution cancel status=%d", response.StatusCode)
	}

	// Pending retry times are stored in UTC but must be rendered in the
	// signed-in member's configured timezone, just like the other authenticated
	// operational timestamps.
	pending, _, err := database.CreateArtistResolution(context.Background(), user.ID, "spotify", "pending-spotify", "Pending Example", "https://open.spotify.com/artist/pending-spotify", "")
	if err != nil {
		t.Fatal(err)
	}
	nextAttempt := time.Date(2026, time.July, 1, 1, 30, 0, 0, time.UTC)
	if _, err := database.DB.Exec(`UPDATE users SET timezone=? WHERE id=?`, "America/Los_Angeles", user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE artist_resolutions SET next_attempt_at=? WHERE id=?`, nextAttempt.Format(time.RFC3339Nano), pending.ID); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(server.URL + "/artist-resolutions/" + strconv.FormatInt(pending.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	pendingBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(pendingBody), "2026-06-30 18:30:00 PDT") {
		t.Fatalf("pending resolution timezone status/body=%d %q", response.StatusCode, pendingBody)
	}
}

func TestCoverageSyncQueuesOnlyOwnedArtists(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	user, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{MBID: "coverage-web-artist", Name: "Coverage Web Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	csrf := getCSRF(t, client, server.URL+"/coverage")
	response := postForm(t, client, server.URL+"/coverage/artists/"+strconv.FormatInt(artist.ID, 10)+"/sync", url.Values{
		"_csrf": {csrf}, "page": {"1"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Synchronization queued") {
		t.Fatalf("coverage sync status=%d body=%q", response.StatusCode, body)
	}
	requests, err := database.ManualSyncRequests(context.Background(), 10)
	if err != nil || len(requests) != 1 || requests[0].ArtistID == nil || *requests[0].ArtistID != artist.ID {
		t.Fatalf("manual sync requests=%#v err=%v", requests, err)
	}
	otherArtist, err := database.UpsertArtist(context.Background(), store.Artist{MBID: "coverage-web-other", Name: "Other Artist"})
	if err != nil {
		t.Fatal(err)
	}
	response = postForm(t, client, server.URL+"/coverage/artists/"+strconv.FormatInt(otherArtist.ID, 10)+"/sync", url.Values{
		"_csrf": {csrf},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unowned coverage sync status=%d", response.StatusCode)
	}
}

func TestCoverageManageArtistsUsesSharedButtonStyles(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	user, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{MBID: "coverage-button-artist", Name: "Coverage Button Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/coverage")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `<a class="button secondary button-link" href="/artists">Manage artists</a>`) {
		t.Fatalf("coverage Manage artists button status/body=%d %q", response.StatusCode, body)
	}
}

func TestReleaseTruthDeskReviewsProviderConflicts(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{MBID: "truth-desk-artist", Name: "Truth Desk Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := store.Release{MBID: "truth-desk-release", Title: "Truth Desk", PrimaryType: "Album", FirstReleaseDate: "2026-09-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/truth-desk-release"}
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{Provider: "musicbrainz", Releases: []store.Release{base}}}, now); err != nil {
		t.Fatal(err)
	}
	spotify := base
	spotify.SpotifyID = "truth-desk-spotify"
	spotify.SpotifyURL = "https://open.spotify.com/album/truth-desk-spotify"
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{Provider: "spotify", Releases: []store.Release{spotify}}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	spotify.FirstReleaseDate = "2026-09-08"
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{Provider: "spotify", Releases: []store.Release{spotify}}}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/coverage/issues")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Release Truth Desk") || !strings.Contains(string(body), "Date conflict") {
		t.Fatalf("truth desk status/body=%d %q", response.StatusCode, body)
	}
	issues, err := database.EvidenceIssues(context.Background(), user.ID, "open", "unread", "date_conflict", "", 50, 0, now.Add(2*time.Minute))
	if err != nil || len(issues) != 1 {
		t.Fatalf("truth desk issues=%#v err=%v", issues, err)
	}
	var releaseID int64
	if err := database.DB.QueryRow(`SELECT id FROM release_groups WHERE mbid=?`, "truth-desk-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(server.URL + "/releases/" + strconv.FormatInt(releaseID, 10))
	if err != nil {
		t.Fatal(err)
	}
	detailBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(detailBody), "Provider evidence") {
		t.Fatalf("truth desk detail status/body=%d %q", response.StatusCode, detailBody)
	}
	csrf := getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/coverage/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/confirm", url.Values{
		"_csrf": {csrf}, "return": {"/coverage/issues"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther && response.StatusCode != http.StatusOK {
		t.Fatalf("truth desk confirm status=%d", response.StatusCode)
	}
	if count, err := database.EvidenceIssueUnreadCount(context.Background(), user.ID, now.Add(2*time.Minute)); err != nil || count != 0 {
		t.Fatalf("truth desk unread after confirm=%d err=%v", count, err)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/releases/"+strconv.FormatInt(releaseID, 10)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"confirm"}, "provider": {"spotify"}, "reason": {"current listing"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("confirm release truth action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/releases/"+strconv.FormatInt(releaseID, 10)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"unknown"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid truth action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/releases/"+strconv.FormatInt(releaseID, 10)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"clear"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("clear truth action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/coverage/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/restore", url.Values{
		"_csrf": {csrf}, "return": {"/coverage/issues"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("truth desk restore status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/coverage/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/snooze", url.Values{
		"_csrf": {csrf}, "duration": {"1d"}, "return": {"/coverage/issues"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("truth desk snooze status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/coverage/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/snooze", url.Values{
		"_csrf": {csrf}, "duration": {"invalid"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("truth desk invalid snooze status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, client, server.URL+"/coverage/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/dismiss", url.Values{
		"_csrf": {csrf}, "return": {"/coverage/issues"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("truth desk dismiss status=%d", response.StatusCode)
	}
}

func TestDashboardRendersSpotifyReleaseObservation(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, &fakeSpotify{}, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "33333333-3333-4333-8333-333333333333", Name: "Pjotr", SortName: "Pjotr",
		SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	futureDate := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "spotify",
		Releases: []store.Release{{
			MBID: "spotify:album-id", SpotifyID: "album-id", Title: "1. KRUIS", PrimaryType: "EP",
			FirstReleaseDate: futureDate, DatePrecision: 3,
			SpotifyURL:      "https://open.spotify.com/album/album-id",
			SpotifyImageURL: "https://i.scdn.co/image/album-art", Source: "spotify",
		}},
	}}, time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(page, "MusicBrainz and Spotify are checked") ||
		!strings.Contains(page, "Upcoming releases") ||
		!strings.Contains(page, `href="/releases/1"`) ||
		!strings.Contains(page, `src="https://i.scdn.co/image/album-art"`) ||
		!strings.Contains(page, "1. KRUIS") || !strings.Contains(page, "EP · Spotify") ||
		strings.Contains(page, `href="/artists/search" target="_blank"`) {
		t.Fatalf("Spotify release dashboard status/body=%d %q", response.StatusCode, body)
	}
}

func TestDashboardRendersITunesArtworkWithAttribution(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "44444444-4444-4444-8444-444444444444", Name: "Example iTunes", SortName: "Example iTunes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:987", ITunesID: "987", Title: "Apple Release", PrimaryType: "Album",
			FirstReleaseDate: date, DatePrecision: 3, ITunesURL: "https://music.apple.com/us/album/apple-release",
			ITunesArtworkURL: "https://is1.mzstatic.com/image/250x250bb.jpg",
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(page, `src="https://is1.mzstatic.com/image/250x250bb.jpg"`) ||
		!strings.Contains(page, "Artwork provided courtesy of iTunes") ||
		!strings.Contains(page, `href="https://music.apple.com/us/album/apple-release" target="_blank" rel="noopener noreferrer"`) ||
		!strings.Contains(page, `data-artwork-fallback="/art/release-group/itunes:987"`) {
		t.Fatalf("iTunes artwork dashboard status/body=%d %q", response.StatusCode, body)
	}
}

func TestReleaseDetailRendersITunesReleaseWithProviderLinks(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "55555555-5555-4555-8555-555555555555", Name: "Detail Artist", SortName: "Detail Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().AddDate(-1, 0, 0).Format("2006-01-02")
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:detail-123", ITunesID: "detail-123", Title: "Detail Album", PrimaryType: "Album",
			FirstReleaseDate: date, DatePrecision: 3, ITunesURL: "https://music.apple.com/us/album/detail-album",
			ITunesArtworkURL: "https://is1.mzstatic.com/image/250x250bb.jpg",
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var releaseID int64
	if err := database.DB.QueryRow(`SELECT id FROM release_groups WHERE mbid=?`, "itunes:detail-123").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/releases/" + strconv.FormatInt(releaseID, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	checks := map[string]bool{
		"status":    response.StatusCode == http.StatusOK,
		"details":   strings.Contains(page, "Release details"),
		"layout":    strings.Contains(page, `class="grid two release-layout"`),
		"summary":   strings.Contains(page, `class="release-detail-summary"`),
		"history":   strings.Contains(page, `release-history-panel`),
		"timeline":  strings.Contains(page, "Release assurance") && strings.Contains(page, "What happened"),
		"title":     strings.Contains(page, "Detail Album"),
		"source":    strings.Contains(page, "Source: iTunes"),
		"art":       strings.Contains(page, `src="https://is1.mzstatic.com/image/250x250bb.jpg"`),
		"link":      strings.Contains(page, `href="https://music.apple.com/us/album/detail-album" target="_blank" rel="noopener noreferrer"`),
		"available": !strings.Contains(page, "Release unavailable"),
	}
	for name, ok := range checks {
		if !ok {
			t.Fatalf("release detail check %s failed status/body=%d %q", name, response.StatusCode, body)
		}
	}
}

func TestReleaseDetailHandlesNullableProviderURLs(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "66666666-6666-4666-8666-666666666666", Name: "Nullable Artist", SortName: "Nullable Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:nullable-123", ITunesID: "nullable-123", Title: "Nullable Album", PrimaryType: "Album",
			FirstReleaseDate: "2024-01-01", DatePrecision: 3,
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var releaseID int64
	if err := database.DB.QueryRow(`SELECT id FROM release_groups WHERE mbid=?`, "itunes:nullable-123").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE release_groups SET spotify_url=NULL,itunes_url=NULL WHERE id=?`, releaseID); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/releases/" + strconv.FormatInt(releaseID, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Nullable Album") {
		t.Fatalf("nullable release detail status/body=%d %q", response.StatusCode, body)
	}
}

func TestReleaseDetailUnavailableIsStyledAndOwnerScoped(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	artist, err := database.UpsertArtist(context.Background(), store.Artist{
		MBID: "77777777-7777-4777-8777-777777777777", Name: "Private Artist", SortName: "Private Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(context.Background(), user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyReleaseBatches(context.Background(), artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:private-123", ITunesID: "private-123", Title: "Private Album", PrimaryType: "Album",
			FirstReleaseDate: "2024-01-01", DatePrecision: 3, ITunesURL: "https://music.apple.com/us/album/private-album",
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var releaseID int64
	if err := database.DB.QueryRow(`SELECT id FROM release_groups WHERE mbid=?`, "itunes:private-123").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	otherID, err := database.CreateUser(context.Background(), "other-detail@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, _, err := database.CreateSession(context.Background(), otherID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	otherJar.SetCookies(serverURL, []*http.Cookie{{
		Name: "artist_session", Value: security.SignedToken("the session secret has more than 32 bytes", otherRaw), Path: "/",
	}})
	otherClient := &http.Client{Jar: otherJar}

	response, err := otherClient.Get(server.URL + "/releases/" + strconv.FormatInt(releaseID, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusNotFound || !strings.Contains(page, "Release unavailable") ||
		strings.Contains(page, "Private Album") || strings.Contains(page, "private-album") {
		t.Fatalf("cross-user release detail status/body=%d %q", response.StatusCode, body)
	}

	response, err = client.Get(server.URL + "/releases/not-an-id")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "Release unavailable") {
		t.Fatalf("invalid release detail status/body=%d %q", response.StatusCode, body)
	}
}

func TestAdminProviderHealthRefreshUsesLatestFailureAndLiveRetryData(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	if _, err := database.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	raw, _, err := database.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, _ := url.Parse(server.URL)
	client.Jar.SetCookies(serverURL, []*http.Cookie{{
		Name: "artist_session", Value: security.SignedToken("the session secret has more than 32 bytes", raw), Path: "/",
	}})
	success := time.Now().UTC().Add(-10 * time.Minute)
	failure := time.Now().UTC().Add(-time.Minute)
	next := time.Now().UTC().Add(20 * time.Minute)
	if err := database.UpsertProviderHealth(context.Background(), "spotify", true, nil, false, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE provider_health SET last_success_at=?, last_failure_at=? WHERE provider=?`, success.Format(time.RFC3339Nano), failure.Format(time.RFC3339Nano), "spotify"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertProviderHealth(context.Background(), "spotify", false, &next, true, true,
		"Spotify artist albums returned 429 Too Many Requests (QUOTA_EXCEEDED); retry after 20m"); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/admin/provider-health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var payload []providerHealthPayload
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(payload) != 1 || payload[0].Status != "quota limited" ||
		payload[0].StatusClass != "ambiguous" || payload[0].NextCheckAt == nil ||
		strings.Contains(payload[0].LastError, "retry after") || payload[0].LastFailureAt == nil {
		t.Fatalf("provider health response status=%d payload=%#v", response.StatusCode, payload)
	}

	response, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if !strings.Contains(page, "quota limited") || !strings.Contains(page, "Last failure") ||
		!strings.Contains(page, "Next check") || !strings.Contains(page, `data-refresh-url="/admin/provider-health"`) ||
		strings.Contains(page, "retry after 20m") {
		t.Fatalf("provider health admin rendering missing live details: %q", page)
	}
	response, err = client.Get(server.URL + "/admin/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	diagnosticBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(diagnosticBody), "ArtistTrackarr release assurance report") ||
		strings.Contains(string(diagnosticBody), "retry after 20m") {
		t.Fatalf("diagnostic report status/body=%d %q", response.StatusCode, diagnosticBody)
	}
}

func TestCompactCount(t *testing.T) {
	tests := map[int64]string{
		0:         "0",
		999:       "999",
		1000:      "1k",
		1234:      "1.2k",
		100000:    "100k",
		143570475: "143.6M",
		51965204:  "52M",
		-1234:     "-1.2k",
	}
	for value, want := range tests {
		if got := compactCount(value); got != want {
			t.Errorf("compactCount(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestCSRFRejectsInvalidCookieSignature(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar, ok := client.Jar.(*cookiejar.Jar)
	if !ok {
		t.Fatal("authenticated test client does not use a cookie jar")
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "artist_csrf", Value: "forged.invalid-signature", Path: "/"}})

	response := postForm(t, client, server.URL+"/logout", url.Values{"_csrf": {"forged"}})
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("forged CSRF cookie status=%d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func authenticatedTestServer(
	t *testing.T,
	mb catalog.CatalogProvider,
	spotify catalog.SpotifyProvider,
	runnerFactory func(*store.Store) *jobs.Runner,
) (*store.Store, *httptest.Server, *http.Client) {
	return authenticatedTestServerWithITunes(t, mb, spotify, nil, runnerFactory)
}

func authenticatedTestServerWithITunes(
	t *testing.T,
	mb catalog.CatalogProvider,
	spotify catalog.SpotifyProvider,
	itunes catalog.ITunesProvider,
	runnerFactory func(*store.Store) *jobs.Runner,
) (*store.Store, *httptest.Server, *http.Client) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "authenticated.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, _ := security.NewCipher(cfg.EncryptionKey)
	userID, err := database.CreateUser(context.Background(), "member@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := database.CreateSession(context.Background(), userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var runner *jobs.Runner
	if runnerFactory != nil {
		runner = runnerFactory(database)
	}
	app, err := New(
		cfg, database, mb, spotify, fakeSender{}, cipher, fakeArtwork{}, runner,
		slog.New(slog.NewTextHandler(io.Discard, nil)), itunes,
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{{
		Name: "artist_session", Value: security.SignedToken(cfg.SessionSecret, raw), Path: "/",
	}})
	return database, server, client
}

var csrfPattern = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func getCSRF(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
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
