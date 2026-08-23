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
