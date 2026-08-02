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
	"time"

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

func TestITunesSearchAndReleaseNormalization(t *testing.T) {
	var searchRequests, releaseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/search" {
			searchRequests.Add(1)
			if request.URL.Query().Get("country") != "NL" || request.URL.Query().Get("entity") != "musicArtist" {
				t.Fatalf("unexpected iTunes search query: %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"results":[{"wrapperType":"artist","artistId":123,"artistName":"Example","artistViewUrl":"https://music.apple.com/nl/artist/example"}]}`)
			return
		}
		if request.URL.Path == "/lookup" {
			releaseRequests.Add(1)
			if request.URL.Query().Get("entity") != "album" {
				t.Fatalf("unexpected iTunes lookup query: %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"results":[
				{"wrapperType":"artist","artistId":123,"artistName":"Example"},
				{"wrapperType":"collection","collectionType":"Album","collectionId":1,"collectionName":"One","collectionArtistName":"Example","trackCount":1,"releaseDate":"2026-01-02T00:00:00Z","collectionViewUrl":"https://music.apple.com/nl/album/one"},
				{"wrapperType":"collection","collectionType":"Album","collectionId":2,"collectionName":"Short EP","collectionArtistName":"Example","trackCount":4,"releaseDate":"2025-02-01T00:00:00Z","collectionViewUrl":"https://music.apple.com/nl/album/ep"},
				{"wrapperType":"collection","collectionType":"Album","collectionId":3,"collectionName":"Long","collectionArtistName":"Example","trackCount":10,"releaseDate":"2024-01-01T00:00:00Z"},
				{"wrapperType":"collection","collectionType":"Album","collectionId":4,"collectionName":"Other Artist","collectionArtistName":"Other","trackCount":10,"releaseDate":"2024-01-01T00:00:00Z"}
			]}`)
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()
	itunes := NewITunes("NL")
	itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
	artists, err := itunes.SearchArtists(context.Background(), "Example")
	if err != nil || len(artists) != 1 || artists[0].ID != "123" {
		t.Fatalf("artists=%#v err=%v", artists, err)
	}
	first, err := itunes.ArtistReleases(context.Background(), "example")
	if err != nil || len(first) != 3 || first[0].PrimaryType != "Single" || first[1].PrimaryType != "EP" || first[2].PrimaryType != "Album" {
		t.Fatalf("releases=%#v err=%v", first, err)
	}
	second, err := itunes.ArtistReleases(context.Background(), "Example")
	if err != nil || len(second) != len(first) || searchRequests.Load() != 1 || releaseRequests.Load() != 1 {
		t.Fatalf("cached releases=%#v search=%d lookup=%d err=%v", second, searchRequests.Load(), releaseRequests.Load(), err)
	}
}

func TestITunesRateLimitHonorsRetryAfterAndSharedCooldown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	itunes := NewITunes("US")
	itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
	_, err := itunes.SearchArtists(context.Background(), "Limited")
	var rateLimit *ITunesRateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.RetryAfter < 119*time.Second || requests.Load() != 1 {
		t.Fatalf("rate limit=%#v requests=%d err=%v", rateLimit, requests.Load(), err)
	}
	_, err = itunes.SearchArtists(context.Background(), "Other")
	if !errors.As(err, &rateLimit) || !rateLimit.AlreadyBlocked || requests.Load() != 1 {
		t.Fatalf("shared cooldown=%#v requests=%d err=%v", rateLimit, requests.Load(), err)
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

func TestSpotifySearchAndArtistCachesAvoidDuplicateRequests(t *testing.T) {
	var tokenRequests, searchRequests, artistRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			tokenRequests.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/search":
			searchRequests.Add(1)
			_, _ = io.WriteString(w, `{"artists":{"items":[{"id":"0OdUWJ0sBjDrqHygGUXeCF","name":"Example","external_urls":{"spotify":"https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF"}}]}}`)
		case "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF":
			artistRequests.Add(1)
			_, _ = io.WriteString(w, `{"id":"0OdUWJ0sBjDrqHygGUXeCF","name":"Example"}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0

	first, err := spotify.SearchArtists(context.Background(), "  Example  ")
	if err != nil || len(first) != 1 {
		t.Fatalf("first search=%#v err=%v", first, err)
	}
	second, err := spotify.SearchArtists(context.Background(), "example")
	if err != nil || len(second) != 1 {
		t.Fatalf("cached search=%#v err=%v", second, err)
	}
	artist, err := spotify.Artist(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil || artist.Name != "Example" {
		t.Fatalf("cached artist=%#v err=%v", artist, err)
	}
	if tokenRequests.Load() != 1 || searchRequests.Load() != 1 || artistRequests.Load() != 0 {
		t.Fatalf("token=%d search=%d artist=%d", tokenRequests.Load(), searchRequests.Load(), artistRequests.Load())
	}
}

func TestSpotifyArtistErrorsAreNotCached(t *testing.T) {
	var artistRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF":
			if artistRequests.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = io.WriteString(w, `{"id":"0OdUWJ0sBjDrqHygGUXeCF","name":"Recovered"}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	if _, err := spotify.Artist(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF"); err == nil {
		t.Fatal("first artist lookup unexpectedly succeeded")
	}
	artist, err := spotify.Artist(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil || artist.Name != "Recovered" || artistRequests.Load() != 2 {
		t.Fatalf("artist=%#v requests=%d err=%v", artist, artistRequests.Load(), err)
	}
}

func TestSpotifyBatchArtistLookupUsesOneRequest(t *testing.T) {
	var tokenRequests, batchRequests, singleRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			tokenRequests.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/artists":
			batchRequests.Add(1)
			if request.URL.Query().Get("ids") != "0OdUWJ0sBjDrqHygGUXeCF,1OdUWJ0sBjDrqHygGUXeCG" {
				t.Fatalf("unexpected batch IDs: %q", request.URL.Query().Get("ids"))
			}
			_, _ = io.WriteString(w, `{"artists":[{"id":"0OdUWJ0sBjDrqHygGUXeCF","name":"First"},{"id":"1OdUWJ0sBjDrqHygGUXeCG","name":"Second"}]}`)
		case "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF", "/v1/artists/1OdUWJ0sBjDrqHygGUXeCG":
			singleRequests.Add(1)
			http.NotFound(w, request)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	artists, err := spotify.Artists(context.Background(), []string{
		"0OdUWJ0sBjDrqHygGUXeCF", "1OdUWJ0sBjDrqHygGUXeCG",
	})
	if err != nil || len(artists) != 2 || artists[0].Name != "First" || artists[1].Name != "Second" {
		t.Fatalf("artists=%#v err=%v", artists, err)
	}
	if tokenRequests.Load() != 1 || batchRequests.Load() != 1 || singleRequests.Load() != 0 {
		t.Fatalf("token=%d batch=%d single=%d", tokenRequests.Load(), batchRequests.Load(), singleRequests.Load())
	}
}

func TestSpotifyRetriesRateLimitAfterRetryHeader(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/search":
			if requests.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"status":429,"message":"slow down","reason":"RATE_LIMITED"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"artists":{"items":[{"id":"spotify-id","name":"Recovered"}]}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	var waits []time.Duration
	spotify.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	results, err := spotify.SearchArtists(context.Background(), "Recovered")
	if err != nil || len(results) != 1 || requests.Load() != 2 {
		t.Fatalf("results=%#v requests=%d err=%v", results, requests.Load(), err)
	}
	if len(waits) != 1 || waits[0] < 1999*time.Millisecond {
		t.Fatalf("waits=%v", waits)
	}
}

func TestSpotifyRateLimitWithoutHeaderUsesBoundedRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":429,"message":"slow down"}}`)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	spotify.retryBase = time.Second
	spotify.wait = func(context.Context, time.Duration) error { return nil }

	_, err := spotify.SearchArtists(context.Background(), "Limited")
	var rateLimit *SpotifyRateLimitError
	if !errors.As(err, &rateLimit) || requests.Load() != 3 ||
		rateLimit.RetryAfter != 4*time.Second || rateLimit.QuotaExceeded {
		t.Fatalf("requests=%d rate limit=%#v err=%v", requests.Load(), rateLimit, err)
	}
}

func TestSpotifyQuotaExceededCreatesSharedCooldown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":429,"message":"Too many requests","reason":"QUOTA_EXCEEDED"}}`)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0

	_, firstErr := spotify.SearchArtists(context.Background(), "Limited")
	_, secondErr := spotify.Artist(context.Background(), "spotify-id")
	var firstLimit, secondLimit *SpotifyRateLimitError
	if !errors.As(firstErr, &firstLimit) || !errors.As(secondErr, &secondLimit) ||
		!firstLimit.QuotaExceeded || !secondLimit.QuotaExceeded ||
		!secondLimit.AlreadyBlocked ||
		firstLimit.Reason != "QUOTA_EXCEEDED" || requests.Load() != 1 ||
		firstLimit.RetryAfter < 29*time.Minute {
		t.Fatalf("requests=%d first=%#v second=%#v", requests.Load(), firstLimit, secondLimit)
	}
}

func TestSpotifyAPIErrorIncludesSafeResponseDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"status":400,"message":"invalid artist id","reason":"INVALID_ID"}}`)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0

	_, err := spotify.SearchArtists(context.Background(), "Invalid")
	var apiErr *SpotifyAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest ||
		apiErr.Reason != "INVALID_ID" || apiErr.Message != "invalid artist id" ||
		!strings.Contains(err.Error(), "INVALID_ID: invalid artist id") {
		t.Fatalf("api error=%#v err=%v", apiErr, err)
	}
}

func TestSpotifyRestoredCooldownSuppressesSearchRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	spotify.RestoreCooldown(time.Now().Add(time.Hour), "QUOTA_EXCEEDED", true)

	_, err := spotify.SearchArtists(context.Background(), "Blocked")
	var rateLimit *SpotifyRateLimitError
	if !errors.As(err, &rateLimit) || !rateLimit.AlreadyBlocked || !rateLimit.QuotaExceeded || requests.Load() != 0 {
		t.Fatalf("err=%#v requests=%d", rateLimit, requests.Load())
	}
}

func TestSpotifyRateLimitWaitHonorsContextCancellation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		requests.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	ctx, cancel := context.WithCancel(context.Background())
	spotify.wait = func(ctx context.Context, delay time.Duration) error {
		cancel()
		return waitContext(ctx, delay)
	}

	_, err := spotify.SearchArtists(ctx, "Cancelled")
	if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}

func TestSpotifyArtistReleasesFetchesNewestPageAndFiltersAlbumsAndEPs(t *testing.T) {
	var tokenRequests, albumRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/token":
			tokenRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF/albums":
			albumRequests.Add(1)
			if request.URL.Query().Get("limit") != "50" || request.URL.Query().Get("market") != "NL" ||
				request.URL.Query().Get("include_groups") != "album,single,compilation" ||
				request.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatalf("unexpected Spotify albums request: %s headers=%v", request.URL, request.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			if request.URL.Query().Get("offset") != "0" {
				t.Fatalf("unexpected Spotify offset: %s", request.URL.Query().Get("offset"))
			}
			_, _ = io.WriteString(w, `{"items":[
					{"id":"album-id","name":"Album","album_type":"album","album_group":"album","total_tracks":10,
					 "release_date":"2026-08-01","release_date_precision":"day",
					 "external_urls":{"spotify":"https://open.spotify.com/album/album-id"},
					 "images":[{"url":"https://i.scdn.co/640","width":640},{"url":"https://i.scdn.co/300","width":300},{"url":"https://i.scdn.co/64","width":64}]},
					{"id":"single-id","name":"Single","album_type":"single","album_group":"single","total_tracks":1,
					 "release_date":"2026","release_date_precision":"year","external_urls":{"spotify":"https://open.spotify.com/album/single-id"}},
					{"id":"ep-id","name":"1. KRUIS","album_type":"single","album_group":"single","total_tracks":6,
					 "release_date":"2026-07","release_date_precision":"month","external_urls":{"spotify":"https://open.spotify.com/album/ep-id"}},
					{"id":"compilation-id","name":"Collected","album_type":"compilation","album_group":"compilation","total_tracks":14,
					 "release_date":"2025-01-01","release_date_precision":"day","external_urls":{"spotify":"https://open.spotify.com/album/compilation-id"}}
				]}`)
			return
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret", "NL")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	releases, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil {
		t.Fatal(err)
	}
	cached, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil || len(cached) != len(releases) {
		t.Fatalf("cached releases=%#v err=%v", cached, err)
	}
	if tokenRequests.Load() != 1 || albumRequests.Load() != 1 || len(releases) != 4 {
		t.Fatalf("token requests=%d album requests=%d releases=%#v",
			tokenRequests.Load(), albumRequests.Load(), releases)
	}
	if releases[0].PrimaryType != "Album" || releases[0].DatePrecision != 3 ||
		releases[0].SpotifyImageURL != "https://i.scdn.co/300" ||
		releases[1].PrimaryType != "Single" || releases[1].Title != "Single" ||
		releases[1].DatePrecision != 1 ||
		releases[2].PrimaryType != "EP" || releases[2].Title != "1. KRUIS" ||
		releases[2].DatePrecision != 2 ||
		releases[3].PrimaryType != "Album" || len(releases[3].SecondaryTypes) != 1 ||
		releases[3].SecondaryTypes[0] != "Compilation" {
		t.Fatalf("unexpected Spotify releases: %#v", releases)
	}
}

func TestSpotifyArtistReleasesSinceStopsAtLocalHistoryBoundary(t *testing.T) {
	var albumRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		if request.URL.Path != "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF/albums" {
			http.NotFound(w, request)
			return
		}
		albumRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("offset") == "0" {
			_, _ = io.WriteString(w, `{"total":2,"items":[{"id":"new","name":"New","album_type":"album","album_group":"album","total_tracks":10,"release_date":"2026-08-01","release_date_precision":"day"}]}`)
			return
		}
		if request.URL.Query().Get("offset") != "50" {
			t.Fatalf("unexpected offset: %s", request.URL.Query().Get("offset"))
		}
		_, _ = io.WriteString(w, `{"total":2,"items":[{"id":"old","name":"Old","album_type":"album","album_group":"album","total_tracks":10,"release_date":"2026-06-01","release_date_precision":"day"}]}`)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	releases, err := spotify.ArtistReleasesSince(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF", "2026-07-01")
	if err != nil || len(releases) != 2 || albumRequests.Load() != 2 {
		t.Fatalf("requests=%d releases=%#v err=%v", albumRequests.Load(), releases, err)
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
	if len(got) != 3 || got[0].MBID != "album" || got[1].MBID != "ep" || got[2].MBID != "single" {
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
