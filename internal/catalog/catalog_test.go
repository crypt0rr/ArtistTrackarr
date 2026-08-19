package catalog

import (
	"context"
	"errors"
	"fmt"
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

func TestSpotifyCachesEvictOldestEntriesAndExpire(t *testing.T) {
	now := time.Now()
	spotify := &Spotify{
		searchCache: map[string]spotifySearchCache{
			"old": {observedAt: now.Add(-2 * time.Minute), expiresAt: now.Add(time.Minute), results: []SpotifyArtist{{ID: "old"}}},
			"new": {observedAt: now.Add(-time.Minute), expiresAt: now.Add(time.Minute), results: []SpotifyArtist{{ID: "new"}}},
		},
		artistCache: map[string]spotifyArtistCache{
			"old": {observedAt: now.Add(-2 * time.Minute), expiresAt: now.Add(time.Minute), artist: SpotifyArtist{ID: "old"}},
			"new": {observedAt: now.Add(-time.Minute), expiresAt: now.Add(time.Minute), artist: SpotifyArtist{ID: "new"}},
		},
		maxSearchCache: 1, maxArtistCache: 1,
	}
	spotify.evictSearchCacheLocked()
	spotify.evictArtistCacheLocked()
	if _, ok := spotify.searchCache["old"]; ok {
		t.Fatal("oldest search cache entry was not evicted")
	}
	if _, ok := spotify.artistCache["old"]; ok {
		t.Fatal("oldest artist cache entry was not evicted")
	}

	spotify.searchCache["expired"] = spotifySearchCache{expiresAt: now.Add(-time.Second), results: []SpotifyArtist{{ID: "expired"}}}
	if _, ok := spotify.cachedSearch("expired"); ok {
		t.Fatal("expired search cache entry was returned")
	}
	spotify.artistCache["expired"] = spotifyArtistCache{expiresAt: now.Add(-time.Second), artist: SpotifyArtist{ID: "expired"}}
	if _, ok := spotify.cachedArtist("expired"); ok {
		t.Fatal("expired artist cache entry was returned")
	}
}

func TestMusicBrainzSearchValidationAndResponseErrors(t *testing.T) {
	mb := NewMusicBrainz("test@example.com")
	mb.interval = 0
	mb.retryBase = 0
	if _, err := mb.SearchArtists(context.Background(), "   ", 10); err == nil {
		t.Fatal("blank MusicBrainz query was accepted")
	}
	var requests atomic.Int32
	mb.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.URL.Query().Get("limit") != "25" {
			t.Fatalf("limit=%q, want 25", request.URL.Query().Get("limit"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"artists":[{"id":"6e335887-60ba-38f0-95af-fae7774336bf","name":"Example","aliases":[{"name":"one"},{"name":"two"},{"name":"three"},{"name":"four"}],"genres":[{"name":"pop"},{"name":"rock"},{"name":"jazz"},{"name":"metal"},{"name":"folk"},{"name":"soul"},{"name":"rap"},{"name":"blues"},{"name":"house"},{"name":"techno"},{"name":"noise"}]}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	results, err := mb.SearchArtists(context.Background(), "example", 25)
	if err != nil || len(results) != 1 || len(results[0].Aliases) != 3 || len(results[0].Genres) != 10 || requests.Load() != 1 {
		t.Fatalf("results=%#v requests=%d err=%v", results, requests.Load(), err)
	}

	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		mb.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader("error")), Header: make(http.Header)}, nil
		})}
		_, err := mb.SearchArtists(context.Background(), "example", 10)
		var statusErr *HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.Status != status {
			t.Fatalf("status=%d error=%#v err=%v", status, statusErr, err)
		}
	}

	mb.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("not json")), Header: make(http.Header)}, nil
	})}
	if _, err := mb.SearchArtists(context.Background(), "example", 10); err == nil {
		t.Fatal("malformed MusicBrainz response was accepted")
	}

	mb.interval = time.Hour
	mb.lastCall = time.Now()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mb.SearchArtists(canceled, "example", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled MusicBrainz search error=%v", err)
	}
}

func TestMusicBrainzResolveArtistAndReleasePagination(t *testing.T) {
	const mbid = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/ws/2/artist/" + mbid:
			_, _ = io.WriteString(w, `{"id":"`+mbid+`","name":"Resolved","sort-name":"Resolved","type":"Person","country":"NL","disambiguation":"singer","genres":[{"name":"pop"},{"name":""}]}`)
		case "/ws/2/release-group":
			if request.URL.Query().Get("artist") != mbid || request.URL.Query().Get("type") != "album|ep|single" {
				t.Fatalf("unexpected release-group query: %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"release-group-count":2,"release-groups":[{"id":"release-one","title":"One","primary-type":"Album","secondary-types":["Live"],"first-release-date":"2026"},{"id":"release-two","title":"Two","primary-type":"EP","first-release-date":"2026-08"}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	mb := NewMusicBrainz("test@example.com")
	mb.baseURL, mb.client, mb.interval, mb.retryBase = server.URL, server.Client(), 0, 0
	artist, err := mb.ResolveArtist(context.Background(), mbid)
	if err != nil || artist.MBID != mbid || artist.Name != "Resolved" || len(artist.Genres) != 1 {
		t.Fatalf("resolved artist=%#v err=%v", artist, err)
	}
	if _, err := mb.ResolveArtist(context.Background(), "not-an-mbid"); err == nil {
		t.Fatal("invalid MusicBrainz ID was accepted")
	}
	releases, err := mb.ArtistReleases(context.Background(), mbid)
	if err != nil || len(releases) != 2 || releases[0].DatePrecision != 1 || releases[1].DatePrecision != 2 {
		t.Fatalf("release groups=%#v err=%v", releases, err)
	}
	if got := artist.StoreArtist(); got.MBID != mbid || got.Country != "NL" {
		t.Fatalf("StoreArtist projection=%#v", got)
	}
	results := []ArtistResult{{Name: "Resolved"}, {Name: "Other", Genres: []string{"rock"}}}
	Enrich(results, []SpotifyArtist{{Name: "resolved", ID: "spotify-id", URL: "https://open.spotify.com/artist/spotify-id", Genres: []string{"pop"}}})
	if results[0].SpotifyID != "spotify-id" || len(results[0].Genres) != 1 {
		t.Fatalf("artist enrichment=%#v", results)
	}
}

func TestMusicBrainzReleaseCreditsProjectsGuestRecordings(t *testing.T) {
	const artistID = "11111111-1111-4111-8111-111111111111"
	const groupID = "22222222-2222-4222-8222-222222222222"
	const recordingID = "33333333-3333-4333-8333-333333333333"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/ws/2/recording" || !strings.Contains(request.URL.Query().Get("query"), "arid:"+artistID) {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"recordings":[{"id":"`+recordingID+`","title":"Collab track","artist-credit":[{"name":"Fridayy","artist":{"id":"`+artistID+`","name":"Fridayy"}},{"name":"Other","artist":{"id":"44444444-4444-4444-8444-444444444444","name":"Other"}}],"release-group-list":[{"id":"`+groupID+`","title":"Collab album","primary-type":"Album","first-release-date":"2026-09-01"}]}]}`)
	}))
	defer server.Close()
	mb := NewMusicBrainz("test@example.com")
	mb.baseURL, mb.client, mb.interval, mb.retryBase = server.URL, server.Client(), 0, 0
	credits, err := mb.ArtistReleaseCredits(context.Background(), artistID, nil)
	if err != nil || len(credits) != 1 || credits[0].MBID != groupID || credits[0].ArtistCreditRole != "featured" || len(credits[0].Credits) != 1 {
		t.Fatalf("credits=%#v err=%v", credits, err)
	}
	credit := credits[0].Credits[0]
	if credit.Role != "guest" || credit.ProviderID != recordingID || credit.TrackTitle != "Collab track" {
		t.Fatalf("credit=%#v", credit)
	}
}

func TestMusicBrainzReleaseCreditsPaginatesWithoutApplyingPartialResults(t *testing.T) {
	const artistID = "11111111-1111-4111-8111-111111111111"
	const groupID = "22222222-2222-4222-8222-222222222222"
	const recordingID = "33333333-3333-4333-8333-333333333333"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/ws/2/recording" || !strings.Contains(request.URL.Query().Get("query"), "arid:"+artistID) {
			http.NotFound(w, request)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("offset") == "100" {
			_, _ = io.WriteString(w, `{"recording-count":101,"recordings":[]}`)
			return
		}
		if request.URL.Query().Get("offset") != "0" {
			t.Fatalf("unexpected credit offset=%q", request.URL.Query().Get("offset"))
		}
		_, _ = io.WriteString(w, `{"recording-count":101,"recordings":[{"id":"`+recordingID+`","title":"Collab track","artist-credit":[{"name":"Fridayy","artist":{"id":"`+artistID+`","name":"Fridayy"}},{"name":"Other","artist":{"id":"44444444-4444-4444-8444-444444444444","name":"Other"}}],"release-group-list":[{"id":"`+groupID+`","title":"Collab album","primary-type":"Album","first-release-date":"2026-09-01"}]}]}`)
	}))
	defer server.Close()
	mb := NewMusicBrainz("test@example.com")
	mb.baseURL, mb.client, mb.interval, mb.retryBase = server.URL, server.Client(), 0, 0
	credits, err := mb.ArtistReleaseCredits(context.Background(), artistID, nil)
	if err != nil || len(credits) != 1 || requests.Load() != 2 {
		t.Fatalf("credits=%#v err=%v requests=%d", credits, err, requests.Load())
	}
}

func TestITunesReleaseCreditsRequireAnExplicitMultiArtistCredit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search" || request.URL.Query().Get("entity") != "song" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resultCount":2,"results":[
			{"wrapperType":"track","trackId":101,"trackName":"Solo","artistName":"Fridayy","collectionId":9,"collectionName":"Solo album","collectionViewUrl":"https://music.apple.com/us/album/solo","releaseDate":"2026-08-01T00:00:00Z","trackCount":10},
			{"wrapperType":"track","trackId":102,"trackName":"Guest","artistName":"Fridayy & Other","collectionId":10,"collectionName":"Guest album","collectionViewUrl":"https://music.apple.com/us/album/guest","releaseDate":"2026-09-01T00:00:00Z","trackCount":12}
		]}`)
	}))
	defer server.Close()
	itunes := NewITunes("US")
	itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
	credits, err := itunes.ArtistReleaseCredits(context.Background(), "Fridayy", nil)
	if err != nil || len(credits) != 1 || credits[0].ITunesID != "10" || len(credits[0].Credits) != 1 {
		t.Fatalf("credits=%#v err=%v", credits, err)
	}
	if credit := credits[0].Credits[0]; credit.Role != "guest" || credit.TrackTitle != "Guest" || credit.ProviderID != "102" {
		t.Fatalf("credit=%#v", credit)
	}
}

func TestCatalogProviderErrorsAndIdentifiers(t *testing.T) {
	if got := (&HTTPStatusError{Provider: "MusicBrainz", Status: http.StatusBadGateway, Text: "502 Bad Gateway"}).Error(); got != "MusicBrainz returned 502 Bad Gateway" {
		t.Fatalf("HTTPStatusError=%q", got)
	}
	if got := (&SpotifyRateLimitError{Operation: "search", Reason: "RATE_LIMITED", RetryAfter: time.Second}).Error(); !strings.Contains(got, "retry after 1s") {
		t.Fatalf("SpotifyRateLimitError=%q", got)
	}
	if got := (&ITunesRateLimitError{Operation: "lookup", Reason: "RATE_LIMITED", RetryAfter: time.Second}).Error(); !strings.Contains(got, "retry after 1s") {
		t.Fatalf("ITunesRateLimitError=%q", got)
	}
	if got := (&ITunesAPIError{Operation: "search", StatusText: "500 Internal Server Error"}).Error(); got != "search returned 500 Internal Server Error" {
		t.Fatalf("ITunesAPIError=%q", got)
	}
	for _, value := range []string{"1", "123456789"} {
		if !validITunesID(value) {
			t.Fatalf("valid iTunes ID %q rejected", value)
		}
	}
	for _, value := range []string{"", "1a", "-1"} {
		if validITunesID(value) {
			t.Fatalf("invalid iTunes ID %q accepted", value)
		}
	}
}

func TestProviderDatePrecisionFallbacks(t *testing.T) {
	for _, test := range []struct {
		precision, value string
		want             int
	}{
		{precision: "year", value: "2026-08-07", want: 1},
		{precision: "month", value: "2026-08-07", want: 2},
		{precision: "day", value: "2026-08-07", want: 3},
		{precision: "unknown", value: "2026", want: 1},
		{precision: "unknown", value: "2026-08", want: 2},
		{precision: "unknown", value: "2026-08-07", want: 3},
		{precision: "unknown", value: "x", want: 0},
	} {
		if got := spotifyDatePrecision(test.precision, test.value); got != test.want {
			t.Errorf("spotifyDatePrecision(%q,%q)=%d, want %d", test.precision, test.value, got, test.want)
		}
	}
	for _, test := range []struct {
		value string
		want  string
		prec  int
	}{
		{value: "2026-08-07T00:00:00Z", want: "2026-08-07", prec: 3},
		{value: "2026-08-07", want: "2026-08-07", prec: 3},
		{value: "2026-08", want: "2026-08", prec: 2},
		{value: "2026", want: "2026", prec: 1},
		{value: "not-a-date", want: "", prec: 0},
	} {
		got, precision := iTunesDate(test.value)
		if got != test.want || precision != test.prec {
			t.Errorf("iTunesDate(%q)=(%q,%d), want (%q,%d)", test.value, got, precision, test.want, test.prec)
		}
	}
}

func TestSpotifyReleaseTypeClassification(t *testing.T) {
	for _, test := range []struct {
		name, albumType, group, title string
		tracks                        int
		want                          string
		secondary                     []string
		ok                            bool
	}{
		{name: "album", albumType: "album", want: "Album", ok: true},
		{name: "compilation group", albumType: "album", group: "compilation", want: "Album", secondary: []string{"Compilation"}, ok: true},
		{name: "compilation type", albumType: "compilation", want: "Album", secondary: []string{"Compilation"}, ok: true},
		{name: "long single ep", albumType: "single", tracks: 4, want: "EP", ok: true},
		{name: "named ep", albumType: "single", title: "Live EP", tracks: 2, want: "EP", ok: true},
		{name: "single", albumType: "single", tracks: 1, want: "Single", ok: true},
		{name: "unknown", albumType: "podcast", want: "", ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, secondary, ok := spotifyReleaseType(test.albumType, test.group, test.title, test.tracks)
			if got != test.want || ok != test.ok || strings.Join(secondary, ",") != strings.Join(test.secondary, ",") {
				t.Fatalf("spotifyReleaseType()=(%q,%#v,%v), want (%q,%#v,%v)", got, secondary, ok, test.want, test.secondary, test.ok)
			}
		})
	}
}

func TestSpotifyReleaseImageSelectionAndTrustedHosts(t *testing.T) {
	if got := spotifyReleaseImage(nil); got != "" {
		t.Fatalf("empty release image=%q", got)
	}
	images := []struct {
		URL   string `json:"url"`
		Width int    `json:"width"`
	}{
		{URL: "small", Width: 64},
		{URL: "large", Width: 640},
	}
	if got := spotifyReleaseImage(images); got != "large" {
		t.Fatalf("selected release image=%q", got)
	}
	if got := spotifyReleaseImage([]struct {
		URL   string `json:"url"`
		Width int    `json:"width"`
	}{{URL: "small", Width: 64}}); got != "small" {
		t.Fatalf("small-only release image=%q", got)
	}
	for _, test := range []struct {
		host, domain string
		want         bool
	}{
		{host: "musicbrainz.org", domain: "musicbrainz.org", want: true},
		{host: "api.musicbrainz.org", domain: "musicbrainz.org", want: true},
		{host: "MUSICBRAINZ.ORG.", domain: "musicbrainz.org", want: true},
		{host: "musicbrainz.org.evil.test", domain: "musicbrainz.org", want: false},
	} {
		if got := trustedProviderHost(test.host, test.domain); got != test.want {
			t.Errorf("trustedProviderHost(%q,%q)=%v, want %v", test.host, test.domain, got, test.want)
		}
	}
}

func TestITunesRetryAfterBoundsValues(t *testing.T) {
	if got := iTunesRetryAfter("0"); got != time.Minute {
		t.Fatalf("zero Retry-After=%v", got)
	}
	if got := iTunesRetryAfter("120"); got != 2*time.Minute {
		t.Fatalf("numeric Retry-After=%v", got)
	}
	if got := iTunesRetryAfter("99999999"); got != 6*time.Hour {
		t.Fatalf("bounded Retry-After=%v", got)
	}
	if got := iTunesRetryAfter("not-a-duration"); got != time.Minute {
		t.Fatalf("invalid Retry-After=%v", got)
	}
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if got := iTunesRetryAfter(future); got < time.Minute || got > 3*time.Minute {
		t.Fatalf("HTTP-date Retry-After=%v", got)
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
			if request.URL.Query().Get("entity") == "musicArtist" {
				_, _ = io.WriteString(w, `{"results":[{"wrapperType":"artist","artistId":123,"artistName":"Example","artistViewUrl":"https://music.apple.com/nl/artist/example"}]}`)
				return
			}
			releaseRequests.Add(1)
			if request.URL.Query().Get("entity") != "album" {
				t.Fatalf("unexpected iTunes lookup query: %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"results":[
				{"wrapperType":"artist","artistId":123,"artistName":"Example"},
				{"wrapperType":"collection","collectionType":"Album","collectionId":1,"collectionName":"One","collectionArtistName":"Example","trackCount":1,"releaseDate":"2026-01-02T00:00:00Z","collectionViewUrl":"https://music.apple.com/nl/album/one","artworkUrl100":"http://is1-ssl.mzstatic.com/image/thumb/Features/100x100bb.jpg"},
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
	if first[0].ITunesArtworkURL != "https://is1-ssl.mzstatic.com/image/thumb/Features/250x250bb.jpg" {
		t.Fatalf("normalized artwork=%q", first[0].ITunesArtworkURL)
	}
	artist, err := itunes.Artist(context.Background(), "123")
	if err != nil || artist.ID != "123" || artist.Name != "Example" || artist.URL == "" {
		t.Fatalf("artist lookup=%#v err=%v", artist, err)
	}
	if _, err := itunes.Artist(context.Background(), "not-an-id"); err == nil {
		t.Fatal("invalid iTunes artist lookup was accepted")
	}
	second, err := itunes.ArtistReleases(context.Background(), "Example")
	if err != nil || len(second) != len(first) || searchRequests.Load() != 1 || releaseRequests.Load() != 1 {
		t.Fatalf("cached releases=%#v search=%d lookup=%d err=%v", second, searchRequests.Load(), releaseRequests.Load(), err)
	}
}

func TestNormalizeITunesArtworkURLRejectsUntrustedHosts(t *testing.T) {
	valid := normalizeITunesArtworkURL("http://is2.mzstatic.com/image/100x100bb.jpg")
	if valid != "https://is2.mzstatic.com/image/250x250bb.jpg" {
		t.Fatalf("valid artwork=%q", valid)
	}
	for _, value := range []string{
		"https://example.com/image/100x100.jpg",
		"https://mzstatic.com/image.jpg?token=secret",
		"https://is1.mzstatic.com:8443/image.jpg",
		"javascript:alert(1)",
		"https://itunes.apple.com/image.jpg#fragment",
	} {
		if got := normalizeITunesArtworkURL(value); got != "" {
			t.Fatalf("artwork %q normalized to untrusted %q", value, got)
		}
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

func TestITunesProviderErrorsAndContextCancellation(t *testing.T) {
	if got := NewITunes(" nl "); got.country != "NL" {
		t.Fatalf("normalized storefront=%q, want NL", got.country)
	}
	if got := NewITunes("not-a-country"); got.country != "US" {
		t.Fatalf("invalid storefront=%q, want US", got.country)
	}

	t.Run("upstream status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		itunes := NewITunes("US")
		itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
		_, err := itunes.SearchArtists(context.Background(), "status")
		var apiErr *ITunesAPIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
			t.Fatalf("API error=%#v err=%v", apiErr, err)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "not-json")
		}))
		defer server.Close()
		itunes := NewITunes("US")
		itunes.baseURL, itunes.client, itunes.requestInterval = server.URL, server.Client(), 0
		if _, err := itunes.SearchArtists(context.Background(), "malformed"); err == nil {
			t.Fatal("malformed provider response was accepted")
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		itunes := NewITunes("US")
		itunes.requestInterval = 0
		itunes.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})}
		if _, err := itunes.SearchArtists(context.Background(), "transport"); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("transport error=%v", err)
		}
	})

	t.Run("wait honors cancellation", func(t *testing.T) {
		itunes := NewITunes("US")
		itunes.requestInterval = time.Hour
		itunes.lastRequest = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := itunes.SearchArtists(ctx, "cancelled"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	})
}

func TestITunesCooldownCanBeRestored(t *testing.T) {
	itunes := NewITunes("US")
	until := time.Now().Add(time.Minute)
	itunes.RestoreCooldown(until, "quota")
	if got := itunes.CooldownUntil(); got.Before(until.Add(-time.Second)) {
		t.Fatalf("restored cooldown=%v, want around %v", got, until)
	}
	// A nil provider is safe for lifecycle code that conditionally restores
	// persisted health state.
	(*ITunes)(nil).RestoreCooldown(until, "ignored")
	if got := (*ITunes)(nil).CooldownUntil(); !got.IsZero() {
		t.Fatalf("nil cooldown=%v", got)
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

func TestSpotifyCoalescedLookupsHonorResultsAndCancellation(t *testing.T) {
	spotify := NewSpotify("client-id", "client-secret")
	searchKey := normalizeSpotifySearchQuery("same query")
	searchCall := &spotifySearchCall{
		done:    make(chan struct{}),
		results: []SpotifyArtist{{ID: "search-id", Name: "Search result"}},
	}
	close(searchCall.done)
	spotify.searchCalls[searchKey] = searchCall
	results, err := spotify.SearchArtists(context.Background(), "  same   query ")
	if err != nil || len(results) != 1 || results[0].ID != "search-id" {
		t.Fatalf("coalesced search results=%#v err=%v", results, err)
	}

	cancelledSearch := &spotifySearchCall{done: make(chan struct{})}
	spotify.searchCalls[normalizeSpotifySearchQuery("cancelled")] = cancelledSearch
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := spotify.SearchArtists(ctx, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("coalesced search cancellation=%v", err)
	}

	artistID := "artist-coalesced"
	artistCall := &spotifyArtistCall{
		done:   make(chan struct{}),
		artist: SpotifyArtist{ID: artistID, Name: "Artist result"},
	}
	close(artistCall.done)
	spotify.artistCalls[artistID] = artistCall
	artist, err := spotify.Artist(context.Background(), artistID)
	if err != nil || artist.Name != "Artist result" {
		t.Fatalf("coalesced artist=%#v err=%v", artist, err)
	}

	cancelledArtistID := "artist-cancelled"
	spotify.artistCalls[cancelledArtistID] = &spotifyArtistCall{done: make(chan struct{})}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := spotify.Artist(ctx, cancelledArtistID); !errors.Is(err, context.Canceled) {
		t.Fatalf("coalesced artist cancellation=%v", err)
	}
}

func TestSpotifyCachesExpireAndEvictOldEntries(t *testing.T) {
	spotify := NewSpotify("client-id", "client-secret")
	now := time.Now()
	spotify.searchCache = map[string]spotifySearchCache{
		"expired": {expiresAt: now.Add(-time.Second)},
		"old":     {observedAt: now.Add(-time.Hour), expiresAt: now.Add(time.Hour)},
	}
	if _, ok := spotify.cachedSearch("expired"); ok {
		t.Fatal("expired search cache entry was returned")
	}
	spotify.maxSearchCache = 1
	spotify.cacheSearchLocked("new", []SpotifyArtist{{ID: "new"}})
	if _, ok := spotify.searchCache["old"]; ok {
		t.Fatal("old search cache entry was not evicted")
	}

	spotify.artistCache = map[string]spotifyArtistCache{
		"expired": {expiresAt: now.Add(-time.Second)},
		"old":     {observedAt: now.Add(-time.Hour), expiresAt: now.Add(time.Hour)},
	}
	if _, ok := spotify.cachedArtist("expired"); ok {
		t.Fatal("expired artist cache entry was returned")
	}
	spotify.maxArtistCache = 1
	spotify.cacheArtistLocked(SpotifyArtist{ID: "new"})
	if _, ok := spotify.artistCache["old"]; ok {
		t.Fatal("old artist cache entry was not evicted")
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
			if request.URL.Query().Get("limit") != "10" || request.URL.Query().Get("market") != "NL" ||
				request.URL.Query().Get("include_groups") != "album,single,compilation,appears_on" ||
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
					,{"id":"featured-id","name":"Guest Album","album_type":"album","album_group":"appears_on","total_tracks":10,
					 "release_date":"2026-09-01","release_date_precision":"day","external_urls":{"spotify":"https://open.spotify.com/album/featured-id"}},
					{"id":"album-id","name":"Album","album_type":"album","album_group":"appears_on","total_tracks":10,
					 "release_date":"2026-08-01","release_date_precision":"day","external_urls":{"spotify":"https://open.spotify.com/album/album-id"}}
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
	spotify.InvalidateArtistReleases("0OdUWJ0sBjDrqHygGUXeCF")
	fresh, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil || len(fresh) != len(releases) {
		t.Fatalf("fresh releases=%#v err=%v", fresh, err)
	}
	if tokenRequests.Load() != 1 || albumRequests.Load() != 2 || len(releases) != 5 {
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
		releases[3].SecondaryTypes[0] != "Compilation" ||
		releases[4].ArtistCreditRole != "featured" || releases[0].ArtistCreditRole != "primary" || len(releases[0].Credits) != 2 {
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
		groups := request.URL.Query().Get("include_groups")
		if groups != "album,single,compilation" && groups != "appears_on" {
			t.Fatalf("unexpected incremental include_groups=%q", groups)
		}
		if groups == "appears_on" {
			_, _ = io.WriteString(w, `{"total":1,"items":[{"id":"guest","name":"Guest","album_type":"album","album_group":"appears_on","total_tracks":10,"release_date":"2026-08-02","release_date_precision":"day"}]}`)
			return
		}
		if request.URL.Query().Get("offset") == "0" {
			_, _ = io.WriteString(w, `{"total":2,"items":[{"id":"new","name":"New","album_type":"album","album_group":"album","total_tracks":10,"release_date":"2026-08-01","release_date_precision":"day"}]}`)
			return
		}
		if request.URL.Query().Get("offset") != "10" {
			t.Fatalf("unexpected offset: %s", request.URL.Query().Get("offset"))
		}
		_, _ = io.WriteString(w, `{"total":2,"items":[{"id":"old","name":"Old","album_type":"album","album_group":"album","total_tracks":10,"release_date":"2026-06-01","release_date_precision":"day"}]}`)
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	releases, err := spotify.ArtistReleasesSince(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF", "2026-07-01")
	if err != nil || len(releases) != 3 || albumRequests.Load() != 3 {
		t.Fatalf("requests=%d releases=%#v err=%v", albumRequests.Load(), releases, err)
	}
	for _, release := range releases {
		if release.SpotifyID == "guest" && release.ArtistCreditRole != "featured" {
			t.Fatalf("appears_on release role=%q, want featured", release.ArtistCreditRole)
		}
	}
}

func TestSpotifyArtistReleasesPagesCompleteCatalogAndDeduplicateCredits(t *testing.T) {
	var albumRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		if request.URL.Path != "/v1/artists/0OdUWJ0sBjDrqHygGUXeCF/albums" {
			http.NotFound(w, request)
			return
		}
		if request.URL.Query().Get("include_groups") != "album,single,compilation,appears_on" {
			t.Fatalf("unexpected include_groups=%q", request.URL.Query().Get("include_groups"))
		}
		albumRequests.Add(1)
		offset := 0
		if _, err := fmt.Sscanf(request.URL.Query().Get("offset"), "%d", &offset); err != nil {
			t.Fatalf("invalid offset: %v", err)
		}
		count := 10
		if offset == 20 {
			count = 5
		}
		items := make([]string, count)
		for index := range items {
			id := fmt.Sprintf("release-%02d", offset+index)
			group := "album"
			if offset == 0 && index == 0 {
				id = "shared"
			}
			if offset == 20 && index == 0 {
				id, group = "shared", "appears_on"
			}
			if offset == 20 && index == 1 {
				id, group = "featured-late", "appears_on"
			}
			items[index] = fmt.Sprintf(`{"id":%q,"name":%q,"album_type":"album","album_group":%q,"total_tracks":10,"release_date":"2026-08-%02d","release_date_precision":"day"}`,
				id, id, group, (offset+index)%28+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"total":25,"items":[%s]}`, strings.Join(items, ","))
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	releases, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil {
		t.Fatal(err)
	}
	if albumRequests.Load() != 3 || len(releases) != 24 {
		t.Fatalf("requests=%d releases=%d, want 3 requests and 24 deduplicated releases", albumRequests.Load(), len(releases))
	}
	var shared, featured bool
	for _, release := range releases {
		switch release.SpotifyID {
		case "shared":
			shared = true
			if release.ArtistCreditRole != "primary" || len(release.Credits) != 2 {
				t.Fatalf("shared release=%#v, want primary role and both credits", release)
			}
		case "featured-late":
			featured = release.ArtistCreditRole == "featured"
		}
	}
	if !shared || !featured {
		t.Fatalf("missing shared or late featured release: %#v", releases)
	}
}

func TestSpotifyArtistReleasesFailsAtCatalogPageSafetyCap(t *testing.T) {
	var albumRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/token" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
			return
		}
		albumRequests.Add(1)
		offset := 0
		if _, err := fmt.Sscanf(request.URL.Query().Get("offset"), "%d", &offset); err != nil {
			t.Fatalf("invalid offset: %v", err)
		}
		items := make([]string, 10)
		for index := range items {
			id := fmt.Sprintf("capped-%d", offset+index)
			items[index] = fmt.Sprintf(`{"id":%q,"name":%q,"album_type":"album","album_group":"album","total_tracks":10,"release_date":"2026-08-01","release_date_precision":"day"}`, id, id)
		}
		_, _ = fmt.Fprintf(w, `{"total":1001,"items":[%s]}`, strings.Join(items, ","))
	}))
	defer server.Close()
	spotify := NewSpotify("client-id", "client-secret")
	spotify.accountsURL, spotify.apiURL, spotify.client = server.URL, server.URL, server.Client()
	spotify.requestInterval = 0
	_, err := spotify.ArtistReleases(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	var limitErr *CatalogLimitError
	if !errors.As(err, &limitErr) || limitErr.Pages != 100 || albumRequests.Load() != 100 {
		t.Fatalf("requests=%d limit=%#v err=%v", albumRequests.Load(), limitErr, err)
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

func TestAlbumEPNormalizerMergesCreditsForOneRelease(t *testing.T) {
	input := []store.Release{
		{MBID: "album", PrimaryType: "Album", ArtistCreditRole: "featured", Credits: []store.ReleaseCredit{{Provider: "musicbrainz", ProviderID: "recording-1", Role: "guest", TrackTitle: "First"}}},
		{MBID: "album", PrimaryType: "Album", ArtistCreditRole: "featured", Credits: []store.ReleaseCredit{{Provider: "musicbrainz", ProviderID: "recording-2", Role: "guest", TrackTitle: "Second"}}},
	}
	got := (AlbumEPNormalizer{}).Normalize(input)
	if len(got) != 1 || len(got[0].Credits) != 2 {
		t.Fatalf("normalized release=%#v", got)
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
	for _, value := range []string{
		"https://spotify.com.evil.example/artist/" + id,
		"https://evilspotify.com/artist/" + id,
		"http://open.spotify.com/artist/" + id,
	} {
		if _, ok := SpotifyID(value); ok {
			t.Fatalf("accepted spoofed or insecure Spotify URL %q", value)
		}
	}
}

func TestExtractMBIDRejectsSpoofedHosts(t *testing.T) {
	const mbid = "11111111-1111-4111-8111-111111111111"
	if got := extractMBID("https://musicbrainz.org/artist/" + mbid); got != mbid {
		t.Fatalf("extractMBID() = %q, want %q", got, mbid)
	}
	for _, value := range []string{
		"https://musicbrainz.org.evil.example/artist/" + mbid,
		"https://evil-musicbrainz.org/artist/" + mbid,
		"http://musicbrainz.org/artist/" + mbid,
	} {
		if got := extractMBID(value); got != value {
			t.Fatalf("extractMBID accepted untrusted URL %q as %q", value, got)
		}
	}
}
