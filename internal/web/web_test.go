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
	app, err := New(cfg, database, fakeCatalog{searchErr: io.ErrUnexpectedEOF}, nil,
		fakeSender{}, cipher, fakeArtwork{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	loginBody, _ := io.ReadAll(loginPage.Body)
	loginPage.Body.Close()
	if !strings.Contains(string(loginBody), "/static/logo-full.png") ||
		!strings.Contains(string(loginBody), "/static/favicon.ico") ||
		!strings.Contains(string(loginBody), "/static/theme.js") ||
		!strings.Contains(string(loginBody), "v"+version.Current) ||
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
		io.Copy(io.Discard, staticResponse.Body)
		staticResponse.Body.Close()
		if staticResponse.StatusCode != http.StatusOK ||
			!strings.HasPrefix(staticResponse.Header.Get("Content-Type"), asset.contentType) {
			t.Fatalf("static asset %s status/type = %d, %q", asset.path,
				staticResponse.StatusCode, staticResponse.Header.Get("Content-Type"))
		}
	}

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
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Never miss the next record") ||
		!strings.Contains(string(body), "/static/logo-mark.png") ||
		!strings.Contains(string(body), "ArtistTrackarr") ||
		strings.Contains(string(body), "Artist Trackarr") ||
		strings.Contains(string(body), "Artist Tracker") ||
		!strings.Contains(string(body), `href="/artists"`) {
		t.Fatalf("dashboard status/body = %d, %q", response.StatusCode, body)
	}
	if strings.Contains(string(body), "Reminder settings") || strings.Contains(string(body), `action="/profile"`) {
		t.Fatalf("dashboard still contains reminder settings: %q", body)
	}
	response, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Reminder settings") ||
		!strings.Contains(string(body), `action="/admin/profile"`) {
		t.Fatalf("admin reminder settings status/body = %d, %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/profile", url.Values{
		"_csrf": {csrf}, "timezone": {"America/New_York"}, "reminder_time": {"08:30"},
	})
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
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
	response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "MusicBrainz is temporarily unavailable") ||
		strings.Contains(string(body), "EOF") {
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
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Spotify Result") ||
		!strings.Contains(string(body), "/artists/follow/spotify") {
		t.Fatalf("Spotify search status/body=%d %q", response.StatusCode, body)
	}
	if spotify.searchCalls != 1 || mb.calls != 0 {
		t.Fatalf("Spotify calls=%d MusicBrainz calls=%d", spotify.searchCalls, mb.calls)
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
			response.Body.Close()
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
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("Spotify follow without CSRF status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	for range 2 {
		response = postForm(t, client, server.URL+"/artists/follow/spotify", url.Values{
			"_csrf": {csrf}, "spotify_id": {spotifyID},
		})
		response.Body.Close()
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
	response.Body.Close()
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
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("batch follow without CSRF status=%d", response.StatusCode)
	}

	csrf := getCSRF(t, client, server.URL+"/artists/search")
	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", url.Values{
		"_csrf":       {csrf},
		"spotify_ids": {firstID, firstID, secondID, "invalid"},
	})
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
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
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty selection status=%d", response.StatusCode)
	}
	tooMany := url.Values{"_csrf": {csrf}}
	for i := range 11 {
		tooMany.Add("spotify_ids", fmt.Sprintf("%022d", i))
	}
	response = postForm(t, client, server.URL+"/artists/follow/spotify/batch", tooMany)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized selection status=%d", response.StatusCode)
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
	response.Body.Close()
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
	response.Body.Close()
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
	response.Body.Close()
	if !strings.Contains(string(body), "<strong>2</strong>") || strings.Contains(string(body), "First Artist") {
		t.Fatalf("dashboard count/list body=%q", body)
	}

	response = postForm(t, client, server.URL+"/artists/follow/batch", url.Values{
		"_csrf": {csrf}, "mbids": {firstMBID, secondMBID},
	})
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "0 added") || !strings.Contains(string(body), "2 already followed") {
		t.Fatalf("duplicate batch body=%q", body)
	}
}

func TestArtistSearchAndOwnerScopedCSVExport(t *testing.T) {
	mb := &searchCatalog{}
	database, server, client := authenticatedTestServer(t, mb, nil, nil)
	ctx := context.Background()
	user, _ := database.UserByEmail(ctx, "member@example.com")
	otherID, _ := database.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
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
	database.Follow(ctx, user.ID, followed.ID)
	database.Follow(ctx, otherID, notFollowed.ID)

	response, err := client.Get(server.URL + "/artists/search")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(page, "<h1>Add artists</h1>") ||
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
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed import endpoint status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	response, err = noRedirect.PostForm(server.URL+"/imports", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed import POST endpoint status=%d", response.StatusCode)
	}

	response, err = client.Get(server.URL + "/artists/export")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
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
	if len(records) != 2 || len(records[0]) != 6 ||
		records[0][0] != "artist" ||
		records[1][0] != "https://musicbrainz.org/artist/11111111-1111-4111-8111-111111111111" ||
		records[1][1] != "Comma, Artist" ||
		records[1][2] != "11111111-1111-4111-8111-111111111111" ||
		records[1][4] != "0OdUWJ0sBjDrqHygGUXeCF" ||
		strings.Contains(string(body), "Other User Artist") {
		t.Fatalf("unexpected owner-scoped CSV records=%#v body=%q", records, body)
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
	_, _ = io.WriteString(part, "artist,display_name,musicbrainz_id,musicbrainz_url,spotify_id,spotify_url\n"+
		"https://musicbrainz.org/artist/"+mbid+",Imported Artist,"+mbid+",https://musicbrainz.org/artist/"+mbid+",,\n"+
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
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Import results") ||
		!strings.Contains(string(body), "Imported Artist") || !strings.Contains(string(body), ">added<") || !strings.Contains(string(body), ">invalid<") {
		t.Fatalf("import response status/body=%d %q", response.StatusCode, body)
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	if count, err := database.FollowedArtistCount(context.Background(), user.ID); err != nil || count != 1 {
		t.Fatalf("imported follow count=%d err=%v", count, err)
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("import response CSP=%q", response.Header.Get("Content-Security-Policy"))
	}
}

func TestAdminDeliveryAuditAndAuthorization(t *testing.T) {
	mb := &searchCatalog{}
	database, server, client := authenticatedTestServer(t, mb, nil, nil)
	targetID, err := database.CreateUser(context.Background(), "delete-me@example.com", "unused", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member admin status=%d", response.StatusCode)
	}
	csrf := getCSRF(t, client, server.URL+"/")
	response = postForm(t, client, server.URL+"/admin/profile", url.Values{
		"_csrf": {csrf}, "timezone": {"America/New_York"}, "reminder_time": {"08:30"},
	})
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member admin profile status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(targetID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	response.Body.Close()
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
	response.Body.Close()
	if !strings.Contains(string(dashboardBody), "delivery-scroll") ||
		strings.Contains(string(dashboardBody), "provider rejected secret detail") {
		t.Fatalf("dashboard delivery log is not compact: %q", dashboardBody)
	}
	response, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, expected := range []string{
		"member@example.com", "Audited Album", "Kitchen display", "ntfy", "failed", "5 attempts",
		"provider rejected secret detail", "Household accounts", "delete-me@example.com",
		"/admin/users/" + strconv.FormatInt(targetID, 10) + "/delete", "Current account",
	} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("admin audit missing %q in %q", expected, body)
		}
	}
	if strings.Contains(string(body), "encrypted-secret") {
		t.Fatalf("admin audit exposed encrypted destination: %q", body)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(user.ID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	selfDeleteBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(selfDeleteBody), "cannot delete your own account") {
		t.Fatalf("self deletion status/body=%d %q", response.StatusCode, selfDeleteBody)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/users/"+strconv.FormatInt(targetID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	deleteBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
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
	resolution, _, err := database.CreateArtistResolution(
		context.Background(), user.ID, "spotify-id", "Example",
		"https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example",
	)
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
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Choose the MusicBrainz artist") {
		t.Fatalf("review status/body=%d %q", response.StatusCode, body)
	}
	csrf := getCSRF(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10))
	response = postForm(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10), url.Values{
		"_csrf": {csrf}, "mbid": {"candidate-mbid"},
	})
	response.Body.Close()
	followed, err := database.FollowedArtists(context.Background(), user.ID)
	if err != nil || len(followed) != 1 || followed[0].MBID != "candidate-mbid" {
		t.Fatalf("reviewed follow=%#v err=%v", followed, err)
	}

	otherID, _ := database.CreateUser(context.Background(), "other@example.com", "unused", "member", "UTC")
	otherResolution, _, _ := database.CreateArtistResolution(
		context.Background(), otherID, "other-spotify", "Other",
		"https://open.spotify.com/artist/other-spotify", "",
	)
	response, err = client.Get(server.URL + "/artist-resolutions/" + strconv.FormatInt(otherResolution.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user review status=%d", response.StatusCode)
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
	response.Body.Close()
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
	defer response.Body.Close()
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
	response.Body.Close()
	page := string(body)
	if !strings.Contains(page, "quota limited") || !strings.Contains(page, "Last failure") ||
		!strings.Contains(page, "Next check") || !strings.Contains(page, `data-refresh-url="/admin/provider-health"`) ||
		strings.Contains(page, "retry after 20m") {
		t.Fatalf("provider health admin rendering missing live details: %q", page)
	}
}

func authenticatedTestServer(
	t *testing.T,
	mb catalog.CatalogProvider,
	spotify catalog.SpotifyProvider,
	runnerFactory func(*store.Store) *jobs.Runner,
) (*store.Store, *httptest.Server, *http.Client) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "authenticated.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, _ := security.NewCipher(cfg.EncryptionKey)
	userID, err := database.CreateUser(context.Background(), "member@example.com", "unused", "member", "UTC")
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
		slog.New(slog.NewTextHandler(io.Discard, nil)),
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
