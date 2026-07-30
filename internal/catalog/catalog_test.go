package catalog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMusicBrainzRetriesTransientTransportFailures(t *testing.T) {
	var requests atomic.Int32
	mb := NewMusicBrainz("test@example.com")
	mb.interval = 0
	mb.retryBase = 0
	mb.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if requests.Add(1) < 3 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"artists":[{"id":"6e335887-60ba-38f0-95af-fae7774336bf","name":"Recovered"}]}`,
			)),
			Header: make(http.Header),
		}, nil
	})}

	results, err := mb.SearchArtists(context.Background(), "recovered", 10)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 || len(results) != 1 || results[0].Name != "Recovered" {
		t.Fatalf("requests=%d results=%#v", requests.Load(), results)
	}
}

func TestMusicBrainzReturnsAfterBoundedRetries(t *testing.T) {
	var requests atomic.Int32
	mb := NewMusicBrainz("test@example.com")
	mb.interval = 0
	mb.retryBase = 0
	mb.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, io.ErrUnexpectedEOF
	})}

	_, err := mb.SearchArtists(context.Background(), "unavailable", 10)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 4 {
		t.Fatalf("requests = %d, want 4", requests.Load())
	}
}

func TestMusicBrainzResolvesExternalArtistRelationship(t *testing.T) {
	mb := NewMusicBrainz("test@example.com")
	mb.interval = 0
	mb.retryBase = 0
	mb.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/ws/2/url" ||
			request.URL.Query().Get("resource") != "https://open.spotify.com/artist/spotify-id" ||
			request.URL.Query().Get("inc") != "artist-rels" {
			t.Fatalf("unexpected request URL: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{"relations":[
				{"artist":{"id":"6e335887-60ba-38f0-95af-fae7774336bf","name":"Example","sort-name":"Example","type":"Group","country":"GB"}},
				{"artist":{"id":"6e335887-60ba-38f0-95af-fae7774336bf","name":"Duplicate"}}
			]}`)),
			Header: make(http.Header),
		}, nil
	})}
	results, err := mb.ResolveExternalArtist(context.Background(), "https://open.spotify.com/artist/spotify-id")
	if err != nil || len(results) != 1 || results[0].Name != "Example" || results[0].Country != "GB" {
		t.Fatalf("resolved artists=%#v err=%v", results, err)
	}
}

func TestMusicBrainzUnknownExternalURLReturnsNoMatches(t *testing.T) {
	mb := NewMusicBrainz("test@example.com")
	mb.interval = 0
	mb.retryBase = 0
	mb.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound, Status: "404 Not Found",
			Body: io.NopCloser(strings.NewReader(`{"error":"Not Found"}`)), Header: make(http.Header),
		}, nil
	})}
	results, err := mb.ResolveExternalArtist(context.Background(), "https://open.spotify.com/artist/unknown")
	if err != nil || len(results) != 0 {
		t.Fatalf("resolved artists=%#v err=%v", results, err)
	}
}

func TestSpotifyArtistSearchUsesCurrentResultLimit(t *testing.T) {
	var tokenRequests, searchRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			tokenRequests.Add(1)
			if request.Method != http.MethodPost {
				t.Fatalf("token method=%s", request.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/search":
			searchRequests.Add(1)
			if request.URL.Query().Get("type") != "artist" || request.URL.Query().Get("limit") != "10" ||
				request.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatalf("unexpected Spotify search request: %s headers=%v", request.URL, request.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"artists":{"items":[{
				"id":"0OdUWJ0sBjDrqHygGUXeCF","name":"Example",
				"external_urls":{"spotify":"https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF"},
				"images":[{"url":"https://i.scdn.co/large"},{"url":"https://i.scdn.co/small"}]
			}]}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	results, err := spotify.SearchArtists(context.Background(), "Example")
	if err != nil || len(results) != 1 || results[0].ID != "0OdUWJ0sBjDrqHygGUXeCF" ||
		results[0].ImageURL != "https://i.scdn.co/small" {
		t.Fatalf("Spotify results=%#v err=%v", results, err)
	}
	if tokenRequests.Load() != 1 || searchRequests.Load() != 1 {
		t.Fatalf("token requests=%d search requests=%d", tokenRequests.Load(), searchRequests.Load())
	}
}

func TestSpotifyArtistReleasesPaginatesAndFiltersAlbumsAndEPs(t *testing.T) {
	var tokenRequests, albumRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			tokenRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF/albums":
			albumRequests.Add(1)
			if request.URL.Query().Get("limit") != "10" || request.URL.Query().Get("market") != "NL" ||
				request.URL.Query().Get("include_groups") != "album,single,compilation" ||
				request.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatalf("unexpected Spotify albums request: %s headers=%v", request.URL, request.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			if request.URL.Query().Get("offset") == "0" {
				_, _ = io.WriteString(w, `{"total":4,"items":[
					{"id":"album-id","name":"Album","album_type":"album","album_group":"album","total_tracks":10,
					 "release_date":"2026-08-01","release_date_precision":"day",
					 "external_urls":{"spotify":"https://open.spotify.com/album/album-id"},
					 "images":[{"url":"https://i.scdn.co/640","width":640},{"url":"https://i.scdn.co/300","width":300},{"url":"https://i.scdn.co/64","width":64}]},
					{"id":"single-id","name":"Single","album_type":"single","album_group":"single","total_tracks":1,
					 "release_date":"2026","release_date_precision":"year","external_urls":{"spotify":"https://open.spotify.com/album/single-id"}},
					{"id":"ep-id","name":"1. KRUIS","album_type":"single","album_group":"single","total_tracks":6,
					 "release_date":"2026-07","release_date_precision":"month","external_urls":{"spotify":"https://open.spotify.com/album/ep-id"}}
				]}`)
				return
			}
			if request.URL.Query().Get("offset") != "3" {
				t.Fatalf("unexpected Spotify offset: %s", request.URL.Query().Get("offset"))
			}
			_, _ = io.WriteString(w, `{"total":4,"items":[
				{"id":"compilation-id","name":"Collected","album_type":"compilation","album_group":"compilation","total_tracks":14,
				 "release_date":"2025-01-01","release_date_precision":"day","external_urls":{"spotify":"https://open.spotify.com/album/compilation-id"}}
			]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret", "NL")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	releases, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil {
		t.Fatal(err)
	}
	if tokenRequests.Load() != 1 || albumRequests.Load() != 2 || len(releases) != 3 {
		t.Fatalf("token requests=%d album requests=%d releases=%#v",
			tokenRequests.Load(), albumRequests.Load(), releases)
	}
	if releases[0].PrimaryType != "Album" || releases[0].DatePrecision != 3 ||
		releases[0].SpotifyImageURL != "https://i.scdn.co/300" ||
		releases[1].PrimaryType != "EP" || releases[1].Title != "1. KRUIS" ||
		releases[1].DatePrecision != 2 ||
		releases[2].PrimaryType != "Album" || len(releases[2].SecondaryTypes) != 1 ||
		releases[2].SecondaryTypes[0] != "Compilation" {
		t.Fatalf("unexpected Spotify releases: %#v", releases)
	}
}

func TestAlbumEPNormalizer(t *testing.T) {
	input := []store.Release{
		{MBID: "album", PrimaryType: "Album", SecondaryTypes: []string{"Live"}},
		{MBID: "ep", PrimaryType: "EP"},
		{MBID: "single", PrimaryType: "Single"},
		{MBID: "album", PrimaryType: "Album"},
	}
	got := (AlbumEPNormalizer{}).Normalize(input)
	if len(got) != 2 || got[0].MBID != "album" || got[1].MBID != "ep" {
		t.Fatalf("unexpected normalized releases: %#v", got)
	}
}

func TestSpotifyID(t *testing.T) {
	const id = "0OdUWJ0sBjDrqHygGUXeCF"
	for _, value := range []string{id, "spotify:artist:" + id, "https://open.spotify.com/artist/" + id} {
		if got, ok := SpotifyID(value); !ok || got != id {
			t.Fatalf("SpotifyID(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := SpotifyID("https://example.com/artist/" + id); ok {
		t.Fatal("accepted non-Spotify URL")
	}
}
