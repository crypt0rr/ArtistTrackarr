package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Provider contract tests run real, captured upstream responses through the
// production parsing code.
//
// Every provider defect this project has hit was the same shape: a struct
// decoded field names the upstream never sends, and the accompanying fixture
// was hand-written with the same wrong names, so the test proved the struct
// matched itself rather than the API. Unit fixtures cannot catch that, because
// the author writes both sides. These fixtures were captured verbatim from the
// live endpoints, so a wrong tag shows up as an empty parse and fails here.
//
// Recapture with `go test ./internal/catalog -run TestProviderContract` after
// refreshing testdata/providers; see field_coverage_test.go for the companion
// check that every declared tag actually appears in a captured payload.

func providerFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "providers", name))
	if err != nil {
		t.Fatalf("read captured payload: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("captured payload %s is not valid JSON", name)
	}
	return data
}

// fixtureServer serves one captured payload for the expected path. A browse
// request that pages past the captured window gets an empty page so the
// production paging loop terminates instead of replaying the fixture.
func fixtureServer(t *testing.T, wantPath, name string, requests *atomic.Int32, emptyBeyondOffsetZero bool) *httptest.Server {
	t.Helper()
	payload := providerFixture(t, name)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantPath != "" && !strings.HasPrefix(r.URL.Path, wantPath) {
			http.NotFound(w, r)
			return
		}
		if requests != nil {
			requests.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		if emptyBeyondOffsetZero {
			if offset := r.URL.Query().Get("offset"); offset != "" && offset != "0" {
				_, _ = w.Write([]byte(`{"release-group-count":0,"release-groups":[]}`))
				return
			}
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func contractMusicBrainz(t *testing.T, server *httptest.Server) *MusicBrainz {
	t.Helper()
	mb := NewMusicBrainz("contract@example.test")
	mb.baseURL, mb.client, mb.interval, mb.retryBase = server.URL, server.Client(), 0, 0
	return mb
}

func contractITunes(t *testing.T, server *httptest.Server) *ITunes {
	t.Helper()
	itunes := NewITunes("US")
	itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
	return itunes
}

func TestProviderContractMusicBrainzRecordingSearchProjectsDatedCredits(t *testing.T) {
	server := fixtureServer(t, "/ws/2/recording", "musicbrainz_recording_search_credits.json", nil, false)
	mb := contractMusicBrainz(t, server)
	// Calvin Harris, whose recording search carries multi-artist credits.
	credits, err := mb.ArtistReleaseCredits(context.Background(), "8dd98bdc-80ec-4e93-8509-2f46bafc09a7", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(credits) == 0 {
		t.Fatal("no guest credits projected from a real recording search carrying multi-artist credits")
	}
	for _, release := range credits {
		if release.ArtistCreditRole != "featured" {
			t.Fatalf("credit role=%q, want featured: %#v", release.ArtistCreditRole, release)
		}
		// A credit with no usable date is skipped, so anything projected must
		// carry a real date at a real precision.
		if strings.TrimSpace(release.FirstReleaseDate) == "" || release.DatePrecision == 0 {
			t.Fatalf("projected credit is undated and would be invisible everywhere: %#v", release)
		}
		if len(release.Credits) == 0 {
			t.Fatalf("projected credit carries no recording credit: %#v", release)
		}
	}
}

func TestProviderContractMusicBrainzRecordingSearchIgnoresSoloRecordings(t *testing.T) {
	// A real search for an artist whose recordings are not collaborations must
	// yield nothing: only explicit multi-artist credits are guest credits.
	server := fixtureServer(t, "/ws/2/recording", "musicbrainz_recording_search_dated.json", nil, false)
	mb := contractMusicBrainz(t, server)
	credits, err := mb.ArtistReleaseCredits(context.Background(), "83d91898-7763-47d7-b03b-b92132375c47", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(credits) != 0 {
		t.Fatalf("solo recordings produced %d guest credits: %#v", len(credits), credits)
	}
}

func TestProviderContractMusicBrainzArtistSearchCarriesNoGenres(t *testing.T) {
	server := fixtureServer(t, "/ws/2/artist", "musicbrainz_artist_search.json", nil, false)
	mb := contractMusicBrainz(t, server)
	results, err := mb.SearchArtists(context.Background(), "Radiohead", 3)
	if err != nil || len(results) == 0 {
		t.Fatalf("results=%d err=%v", len(results), err)
	}
	for _, result := range results {
		if strings.TrimSpace(result.MBID) == "" || strings.TrimSpace(result.Name) == "" {
			t.Fatalf("search result did not parse: %#v", result)
		}
		// The search document carries "tags", never "genres". Decoding a genres
		// key here silently yielded nothing and made every resolved artist cost
		// an extra lookup on every sync.
		if len(result.Genres) != 0 {
			t.Fatalf("search result reported genres the search document cannot carry: %#v", result)
		}
	}
}

func TestProviderContractMusicBrainzArtistLookupCarriesGenresAndAliases(t *testing.T) {
	server := fixtureServer(t, "/ws/2/artist/", "musicbrainz_artist_lookup.json", nil, false)
	mb := contractMusicBrainz(t, server)
	result, err := mb.ResolveArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.TrimSpace(result.Name) == "" {
		t.Fatalf("lookup did not parse: %#v", result)
	}
	// The lookup endpoint is where inc=genres actually applies; this is the one
	// place genres can come from.
	if len(result.Genres) == 0 {
		t.Fatalf("lookup produced no genres: %#v", result)
	}
	// inc=aliases is requested, so the payload must not be fetched and dropped.
	if len(result.Aliases) == 0 {
		t.Fatalf("lookup requested aliases and discarded them: %#v", result)
	}
}

func TestProviderContractMusicBrainzReleaseGroupBrowse(t *testing.T) {
	server := fixtureServer(t, "/ws/2/release-group", "musicbrainz_release_group_browse.json", nil, true)
	mb := contractMusicBrainz(t, server)
	releases, err := mb.ArtistReleases(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(releases) == 0 {
		t.Fatal("browse produced no releases")
	}
	for _, release := range releases {
		if strings.TrimSpace(release.MBID) == "" || strings.TrimSpace(release.Title) == "" ||
			strings.TrimSpace(release.PrimaryType) == "" {
			t.Fatalf("browse release did not parse: %#v", release)
		}
	}
}

func TestProviderContractITunesArtistAlbums(t *testing.T) {
	var requests atomic.Int32
	server := fixtureServer(t, "/lookup", "itunes_artist_albums.json", &requests, false)
	itunes := contractITunes(t, server)
	// Jack Johnson, artist id 909253, as captured.
	releases, providerID, _, err := itunes.ArtistReleasesForCanonical(context.Background(),
		"11111111-1111-4111-8111-111111111111", "Jack Johnson", "909253")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(releases) == 0 {
		t.Fatal("lookup produced no releases")
	}
	if providerID != "909253" {
		t.Fatalf("provider id=%q", providerID)
	}
	for _, release := range releases {
		if strings.TrimSpace(release.ITunesID) == "" || strings.TrimSpace(release.Title) == "" ||
			release.DatePrecision == 0 {
			t.Fatalf("lookup release did not parse: %#v", release)
		}
	}
	// Apple ignores offset on this endpoint, so paging it replays the same page.
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests=%d, want exactly 1", got)
	}
}

func TestProviderContractITunesSongSearchRejectsSameNameTracks(t *testing.T) {
	// The captured search returns only tracks credited to the artist alone.
	// Those are already covered by ArtistReleases and must not be reported as
	// guest credits.
	server := fixtureServer(t, "/search", "itunes_song_search.json", nil, false)
	itunes := contractITunes(t, server)
	credits, err := itunes.ArtistReleaseCredits(context.Background(), "Radiohead", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(credits) != 0 {
		t.Fatalf("same-name tracks were reported as guest credits: %#v", credits)
	}
}

func TestProviderContractITunesArtistSearch(t *testing.T) {
	server := fixtureServer(t, "/search", "itunes_artist_search.json", nil, false)
	itunes := contractITunes(t, server)
	artists, err := itunes.SearchArtists(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(artists) == 0 {
		t.Fatal("artist search produced no artists")
	}
	for _, artist := range artists {
		if strings.TrimSpace(artist.ID) == "" || strings.TrimSpace(artist.Name) == "" {
			t.Fatalf("artist did not parse: %#v", artist)
		}
	}
}
