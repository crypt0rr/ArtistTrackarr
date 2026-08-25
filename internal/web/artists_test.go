package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

// TestArtistActionsReturnToTheSameListView pins the followed list's position
// through an action. The list is paginated and filtered, and every action used
// to redirect to a bare "/artists" - so a member working down page 3 of a genre
// filter, pausing artists one by one, was thrown back to page 1 of the whole
// watchlist on each one and had to navigate back every time.
func TestArtistActionsReturnToTheSameListView(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "list-context-artist", Name: "List Context Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}

	// The page must hand the action a description of the view it is on.
	response, err := client.Get(server.URL + "/artists?genre=rock&page=2")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(raw)
	if !strings.Contains(page, `name="list"`) {
		t.Fatalf("the artists page carries no list context for its action forms: %s", page)
	}

	// And the action must send it back.
	csrf := getCSRF(t, client, server.URL+"/artists")
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response = postForm(t, &noRedirect, server.URL+"/artists/"+fmt.Sprint(artist.ID)+"/notification-rule/pause", url.Values{
		"_csrf": {csrf}, "days": {"7"}, "list": {"genre=rock&page=2"},
	})
	location := response.Header.Get("Location")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("pause status=%d, want 303", response.StatusCode)
	}
	target, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if got := target.Query().Get("genre"); got != "rock" {
		t.Fatalf("redirect dropped the genre filter: %q", location)
	}
	if got := target.Query().Get("page"); got != "2" {
		t.Fatalf("redirect dropped the page: %q", location)
	}
	if target.Query().Get("message") == "" {
		t.Fatalf("redirect dropped the status message: %q", location)
	}
}

// TestArtistActionRedirectIgnoresUnknownListKeys keeps the echoed value to the
// parameters the artists page actually reads. The field is caller-submitted and
// its contents reach a Location header, so it is rebuilt from a known key set
// rather than passed through.
func TestArtistActionRedirectIgnoresUnknownListKeys(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/artists/1/sync",
		strings.NewReader("list="+url.QueryEscape("genre=rock&evil=%0d%0aSet-Cookie:+x&page=3")))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := artistListQuery(request)
	values, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("genre") != "rock" || values.Get("page") != "3" {
		t.Fatalf("known keys were not preserved: %q", got)
	}
	if _, ok := values["evil"]; ok {
		t.Fatalf("an unknown key survived into the redirect: %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("the encoded value carries a line break: %q", got)
	}
}

// TestArtistActionsDoNotReplayTheDiscoveryQuery pins the #236 list context to
// parameters the followed list actually reads. It preserved "q" as well, but the
// list query never consults it - it feeds provider discovery alone - so every
// sync, remove, pause, resume and rule save replayed a Spotify, Apple/iTunes and
// MusicBrainz search for a purely local change. Those calls consume the shared
// per-second MusicBrainz budget, and thirty actions in five minutes exhausted
// the search limiter, showing a red "search is temporarily rate limited" banner
// on a page where the member never searched.
func TestArtistActionsDoNotReplayTheDiscoveryQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/artists/1/sync",
		strings.NewReader("list="+url.QueryEscape("q=radiohead&genre=rock&page=3")))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := artistListQuery(request)
	values, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("q") != "" {
		t.Fatalf("the discovery term survived into an action redirect: %q", got)
	}
	// The genuine list parameters must still be preserved.
	if values.Get("genre") != "rock" || values.Get("page") != "3" {
		t.Fatalf("list parameters were lost: %q", got)
	}
}

// TestArtistsPageListContextOmitsTheSearchTerm covers the other half: the value
// the page hands to its action forms.
func TestArtistsPageListContextOmitsTheSearchTerm(t *testing.T) {
	d := &PageData{}
	d.Query = "radiohead"
	d.GenreFilter = "rock"
	got := currentArtistListQuery(d, 2)
	values, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("q") != "" {
		t.Fatalf("the page hands the discovery term to its action forms: %q", got)
	}
	if values.Get("genre") != "rock" || values.Get("page") != "2" {
		t.Fatalf("list parameters missing from the page context: %q", got)
	}
}

// TestManualSyncRedirectShowsAQueuedMessage backs the container smoke test's new
// assertion. syncArtist answers 303 on both branches - queued and "could not be
// queued" - so the smoke test's `curl --fail` without -L treated a failed sync
// as a pass and then printed that manual sync passed. It now follows the
// redirect and greps for this text, which only holds if the text really renders.
func TestManualSyncRedirectShowsAQueuedMessage(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "smoke-sync-artist", Name: "Smoke Sync Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, fmt.Sprintf("%s/artists/%d/sync", server.URL, artist.ID), url.Values{"_csrf": {csrf}})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "Synchronization queued") {
		t.Fatalf("the followed redirect does not render the queued message the smoke test greps for: %s", body)
	}
}
