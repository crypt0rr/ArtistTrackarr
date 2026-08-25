package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

// ITunes is a deliberately small client for Apple's public iTunes Search API.
// It has no credentials, so the client is conservative about request volume
// and never retains raw provider responses.
type ITunes struct {
	client  *http.Client
	baseURL string
	country string

	requestMu       sync.Mutex
	lastRequest     time.Time
	blockedUntil    time.Time
	blockedReason   string
	requestInterval time.Duration
	wait            func(context.Context, time.Duration) error

	cacheMu         sync.Mutex
	searchCache     map[string]itunesSearchCache
	releaseCache    map[string]itunesReleaseCache
	artistCache     map[string]itunesArtistCache
	searchCalls     map[string]*itunesSearchCall
	releaseCalls    map[string]*itunesReleaseCall
	artistCalls     map[string]*itunesArtistCall
	searchTTL       time.Duration
	emptySearchTTL  time.Duration
	releaseTTL      time.Duration
	artistTTL       time.Duration
	maxSearchCache  int
	maxReleaseCache int
	maxArtistCache  int
}

type ITunesArtist struct {
	ID   string
	Name string
	URL  string
}

type ITunesRateLimitError struct {
	Operation      string
	Status         int
	Reason         string
	RetryAfter     time.Duration
	AlreadyBlocked bool
}

func (e *ITunesRateLimitError) Error() string {
	message := fmt.Sprintf("%s returned 429 Too Many Requests", e.Operation)
	if e.Reason != "" {
		message += " (" + e.Reason + ")"
	}
	if e.RetryAfter > 0 {
		message += "; retry after " + e.RetryAfter.Round(time.Second).String()
	}
	return message
}

type ITunesAPIError struct {
	Operation  string
	Status     int
	StatusText string
}

func (e *ITunesAPIError) Error() string {
	return fmt.Sprintf("%s returned %s", e.Operation, e.StatusText)
}

// ITunesArtistNotFoundError indicates that the iTunes API was reachable but
// did not contain an exact artist identity. This is a negative catalog result,
// not a provider outage, so callers may safely continue with their fallback
// source without marking iTunes degraded.
type ITunesArtistNotFoundError struct {
	Name string
	ID   string
}

func (e *ITunesArtistNotFoundError) Error() string {
	if e == nil {
		return "iTunes artist was not found"
	}
	if strings.TrimSpace(e.ID) != "" {
		return fmt.Sprintf("iTunes artist %q was not found", e.ID)
	}
	if strings.TrimSpace(e.Name) != "" {
		return fmt.Sprintf("iTunes artist %q was not found", e.Name)
	}
	return "iTunes artist was not found"
}

type itunesSearchCache struct {
	observedAt time.Time
	expiresAt  time.Time
	results    []ITunesArtist
}

type itunesReleaseCache struct {
	observedAt time.Time
	expiresAt  time.Time
	results    []store.Release
}

type itunesArtistCache struct {
	observedAt time.Time
	expiresAt  time.Time
	artist     ITunesArtist
}

type itunesSearchCall struct {
	done    chan struct{}
	results []ITunesArtist
	err     error
}

type itunesReleaseCall struct {
	done    chan struct{}
	results []store.Release
	err     error
}

type itunesArtistCall struct {
	done   chan struct{}
	artist ITunesArtist
	err    error
}

type itunesResult struct {
	WrapperType          string `json:"wrapperType"`
	CollectionType       string `json:"collectionType"`
	ArtistID             int64  `json:"artistId"`
	ArtistName           string `json:"artistName"`
	CollectionArtistName string `json:"collectionArtistName"`
	ArtistViewURL        string `json:"artistViewUrl"`
	CollectionID         int64  `json:"collectionId"`
	CollectionName       string `json:"collectionName"`
	CollectionViewURL    string `json:"collectionViewUrl"`
	TrackID              int64  `json:"trackId"`
	TrackName            string `json:"trackName"`
	ArtworkURL100        string `json:"artworkUrl100"`
	ArtworkURL60         string `json:"artworkUrl60"`
	TrackCount           int    `json:"trackCount"`
	ReleaseDate          string `json:"releaseDate"`
}

type itunesResponse struct {
	ResultCount int            `json:"resultCount"`
	Results     []itunesResult `json:"results"`
}

func NewITunes(country string) *ITunes {
	country = strings.ToUpper(strings.TrimSpace(country))
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		country = "US"
	}
	return &ITunes{
		client:          &http.Client{Timeout: 15 * time.Second},
		baseURL:         "https://itunes.apple.com",
		country:         country,
		requestInterval: 3 * time.Second,
		wait:            waitContext,
		searchCache:     make(map[string]itunesSearchCache),
		releaseCache:    make(map[string]itunesReleaseCache),
		artistCache:     make(map[string]itunesArtistCache),
		searchCalls:     make(map[string]*itunesSearchCall),
		releaseCalls:    make(map[string]*itunesReleaseCall),
		artistCalls:     make(map[string]*itunesArtistCall),
		searchTTL:       10 * time.Minute,
		emptySearchTTL:  2 * time.Minute,
		releaseTTL:      24 * time.Hour,
		artistTTL:       24 * time.Hour,
		maxSearchCache:  256,
		maxReleaseCache: 512,
		maxArtistCache:  512,
	}
}

// RestoreCooldown rehydrates a persisted provider cooldown after a restart.
func (i *ITunes) RestoreCooldown(until time.Time, reason string) {
	if i == nil || !until.After(time.Now()) {
		return
	}
	i.requestMu.Lock()
	if until.After(i.blockedUntil) {
		i.blockedUntil = until
		i.blockedReason = strings.TrimSpace(reason)
	}
	i.requestMu.Unlock()
}

func (i *ITunes) CooldownUntil() time.Time {
	if i == nil {
		return time.Time{}
	}
	i.requestMu.Lock()
	defer i.requestMu.Unlock()
	return i.blockedUntil
}

func (i *ITunes) SearchArtists(ctx context.Context, query string) ([]ITunesArtist, error) {
	if i == nil {
		return nil, errors.New("iTunes is not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("search query is required")
	}
	key := normalizeITunesQuery(query)
	if cached, ok := i.cachedSearch(key); ok {
		return cached, nil
	}

	i.cacheMu.Lock()
	if call, ok := i.searchCalls[key]; ok {
		i.cacheMu.Unlock()
		select {
		case <-call.done:
			return cloneITunesArtists(call.results), coalescedRequestError(ctx, call.err)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &itunesSearchCall{done: make(chan struct{})}
	i.searchCalls[key] = call
	i.cacheMu.Unlock()

	result, err := i.searchArtists(ctx, query)
	i.cacheMu.Lock()
	delete(i.searchCalls, key)
	call.results = cloneITunesArtists(result)
	call.err = err
	if err == nil {
		i.cacheSearchLocked(key, result)
	}
	close(call.done)
	i.cacheMu.Unlock()
	return result, err
}

func (i *ITunes) searchArtists(ctx context.Context, query string) ([]ITunesArtist, error) {
	endpoint := i.baseURL + "/search?term=" + url.QueryEscape(query) +
		"&country=" + url.QueryEscape(i.country) +
		"&media=music&entity=musicArtist&attribute=artistTerm&limit=10"
	var response itunesResponse
	if err := i.getJSON(ctx, "iTunes artist search", endpoint, &response); err != nil {
		return nil, err
	}
	result := make([]ITunesArtist, 0, len(response.Results))
	seen := make(map[string]bool)
	for _, item := range response.Results {
		if item.WrapperType != "artist" || item.ArtistID <= 0 || strings.TrimSpace(item.ArtistName) == "" {
			continue
		}
		id := strconv.FormatInt(item.ArtistID, 10)
		if seen[id] {
			continue
		}
		seen[id] = true
		artistURL := strings.TrimSpace(item.ArtistViewURL)
		if artistURL == "" {
			artistURL = "https://itunes.apple.com/artist/id" + id
		}
		result = append(result, ITunesArtist{ID: id, Name: strings.TrimSpace(item.ArtistName), URL: artistURL})
	}
	return result, nil
}

func (i *ITunes) Artist(ctx context.Context, id string) (ITunesArtist, error) {
	if i == nil {
		return ITunesArtist{}, errors.New("iTunes is not configured")
	}
	id = strings.TrimSpace(id)
	if !validITunesID(id) {
		return ITunesArtist{}, errors.New("invalid iTunes artist ID")
	}
	if cached, ok := i.cachedArtist(id); ok {
		return cached, nil
	}
	i.cacheMu.Lock()
	if call, ok := i.artistCalls[id]; ok {
		i.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.artist, coalescedRequestError(ctx, call.err)
		case <-ctx.Done():
			return ITunesArtist{}, ctx.Err()
		}
	}
	call := &itunesArtistCall{done: make(chan struct{})}
	i.artistCalls[id] = call
	i.cacheMu.Unlock()

	artist, err := i.artistByID(ctx, id)
	i.cacheMu.Lock()
	delete(i.artistCalls, id)
	call.artist, call.err = artist, err
	if err == nil {
		i.cacheArtistLocked(artist)
	}
	close(call.done)
	i.cacheMu.Unlock()
	return artist, err
}

func (i *ITunes) artistByID(ctx context.Context, id string) (ITunesArtist, error) {
	endpoint := i.baseURL + "/lookup?id=" + url.QueryEscape(id) +
		"&country=" + url.QueryEscape(i.country) + "&entity=musicArtist&limit=1"
	var response itunesResponse
	if err := i.getJSON(ctx, "iTunes artist lookup", endpoint, &response); err != nil {
		return ITunesArtist{}, err
	}
	for _, item := range response.Results {
		if item.WrapperType == "artist" && strconv.FormatInt(item.ArtistID, 10) == id && strings.TrimSpace(item.ArtistName) != "" {
			artistURL := strings.TrimSpace(item.ArtistViewURL)
			if artistURL == "" {
				artistURL = "https://itunes.apple.com/artist/id" + id
			}
			return ITunesArtist{ID: id, Name: strings.TrimSpace(item.ArtistName), URL: artistURL}, nil
		}
	}
	return ITunesArtist{}, &ITunesArtistNotFoundError{ID: id}
}

func (i *ITunes) ArtistReleases(ctx context.Context, artistName string) ([]store.Release, error) {
	if i == nil {
		return nil, errors.New("iTunes is not configured")
	}
	artistName = strings.TrimSpace(artistName)
	if artistName == "" {
		return nil, errors.New("artist name is required")
	}
	artist, err := i.resolveExactArtist(ctx, artistName)
	if err != nil {
		return nil, err
	}
	return i.artistReleasesCached(ctx, "name:"+artist.ID, artistName, artist.ID)
}

// ArtistReleasesForCanonical resolves or uses the persisted iTunes identity
// for one canonical MusicBrainz artist. A canonical key is deliberately part
// of the release cache key so two same-name canonical artists can never share
// an otherwise valid provider response.
func (i *ITunes) ArtistReleasesForCanonical(ctx context.Context, canonicalID, artistName, providerID string) ([]store.Release, string, string, error) {
	if i == nil {
		return nil, "", "", errors.New("iTunes is not configured")
	}
	canonicalID, artistName, providerID = strings.TrimSpace(canonicalID), strings.TrimSpace(artistName), strings.TrimSpace(providerID)
	if canonicalID == "" {
		return nil, "", "", errors.New("canonical artist ID is required")
	}
	if artistName == "" {
		return nil, "", "", errors.New("artist name is required")
	}
	var artist ITunesArtist
	var err error
	if providerID != "" {
		if !validITunesID(providerID) {
			return nil, "", "", errors.New("invalid iTunes artist ID")
		}
		artist = ITunesArtist{ID: providerID, URL: "https://itunes.apple.com/artist/id" + providerID}
	} else {
		artist, err = i.resolveExactArtist(ctx, artistName)
		if err != nil {
			return nil, "", "", err
		}
	}
	key := "canonical:" + canonicalID + "\x00" + artist.ID
	releases, err := i.artistReleasesCached(ctx, key, artistName, artist.ID)
	// A truncation signal accompanies usable releases rather than replacing
	// them, so it must not discard the catalogue on its way out. Every other
	// error still does.
	truncated := &ITunesCatalogTruncatedError{}
	if err != nil && !errors.As(err, &truncated) {
		return nil, "", "", err
	}
	return releases, artist.ID, artist.URL, err
}

func (i *ITunes) resolveExactArtist(ctx context.Context, artistName string) (ITunesArtist, error) {
	artists, err := i.SearchArtists(ctx, artistName)
	if err != nil {
		return ITunesArtist{}, err
	}
	matches := make([]ITunesArtist, 0, len(artists))
	seen := make(map[string]struct{})
	for _, artist := range artists {
		if !strings.EqualFold(strings.TrimSpace(artist.Name), artistName) || !validITunesID(strings.TrimSpace(artist.ID)) {
			continue
		}
		if _, ok := seen[artist.ID]; ok {
			continue
		}
		seen[artist.ID] = struct{}{}
		matches = append(matches, artist)
	}
	if len(matches) == 0 {
		return ITunesArtist{}, &ITunesArtistNotFoundError{Name: artistName}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return ITunesArtist{}, &ITunesAmbiguousArtistError{Name: artistName, IDs: ids}
	}
	return matches[0], nil
}

// ITunesAmbiguousArtistError is returned when a name-only lookup has more
// than one exact provider identity. Callers should fall back or ask for an
// explicit reviewed mapping instead of picking the first result.
type ITunesAmbiguousArtistError struct {
	Name string
	IDs  []string
}

func (e *ITunesAmbiguousArtistError) Error() string {
	return fmt.Sprintf("iTunes artist %q has %d exact matches", e.Name, len(e.IDs))
}

func (i *ITunes) artistReleasesCached(ctx context.Context, key, artistName, artistID string) ([]store.Release, error) {
	if cached, ok := i.cachedReleases(key); ok {
		return cached, nil
	}

	i.cacheMu.Lock()
	if call, ok := i.releaseCalls[key]; ok {
		i.cacheMu.Unlock()
		select {
		case <-call.done:
			return cloneITunesReleases(call.results), coalescedRequestError(ctx, call.err)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &itunesReleaseCall{done: make(chan struct{})}
	i.releaseCalls[key] = call
	i.cacheMu.Unlock()

	result, err := i.artistReleasesByID(ctx, artistName, artistID)
	i.cacheMu.Lock()
	delete(i.releaseCalls, key)
	call.results = cloneITunesReleases(result)
	call.err = err
	if err == nil {
		i.cacheReleasesLocked(key, result)
	}
	close(call.done)
	i.cacheMu.Unlock()
	return result, err
}

// ITunesCatalogTruncatedError reports that Apple's lookup endpoint returned its
// hard maximum of collections, so the catalogue it produced is an arbitrary
// subset of the artist's real catalogue rather than all of it.
//
// It is returned ALONGSIDE the releases that were found, not instead of them:
// the partial catalogue is still worth importing, but the caller must know not
// to treat the result as a complete healthy answer. Verified live against the
// endpoint on 2026-08-24 - limit=150 returns 150 collections, while limit=200,
// 250 and 300 all return exactly 200, so this is a server-side ceiling and not
// an effect of the limit value. The endpoint also ignores offset, which is why
// paging it was removed and cannot simply be restored.
type ITunesCatalogTruncatedError struct {
	ArtistID string
	Limit    int
}

func (e *ITunesCatalogTruncatedError) Error() string {
	return fmt.Sprintf("iTunes returned its maximum of %d collections for artist %s, so the catalogue is incomplete", e.Limit, e.ArtistID)
}

func (i *ITunes) artistReleasesByID(ctx context.Context, artistName, artistID string) ([]store.Release, error) {
	// Apple's lookup endpoint ignores offset: every request returns the same
	// first page. Paging it meant that any artist with a full page of
	// collections never satisfied the short-page terminator, so the loop ran its
	// whole cap, burned that many upstream requests, and then threw the results
	// away with a CatalogLimitError - which cooled the provider down on every
	// sync of a prolific artist. One request is therefore the whole budget, and
	// lookupPageSize is the documented maximum the endpoint honours.
	const lookupPageSize = 200
	result := make([]store.Release, 0)
	seen := make(map[string]bool)
	collections := 0
	{
		endpoint := i.baseURL + "/lookup?id=" + url.QueryEscape(artistID) +
			"&country=" + url.QueryEscape(i.country) + "&entity=album&limit=" + strconv.Itoa(lookupPageSize)
		var response itunesResponse
		if err := i.getJSON(ctx, "iTunes artist albums", endpoint, &response); err != nil {
			return nil, err
		}
		// A lookup by artist ID returns that artist's own row alongside the
		// collections. If Apple's name for the looked-up ID is not the artist
		// that was asked for, the persisted identity is stale or mis-mapped:
		// surface that for review instead of silently importing another
		// artist's discography, and instead of silently dropping every row.
		for _, item := range response.Results {
			if item.WrapperType != "artist" {
				continue
			}
			owner := strings.TrimSpace(item.ArtistName)
			if owner != "" && !strings.EqualFold(owner, artistName) {
				return nil, &ITunesAmbiguousArtistError{Name: artistName, IDs: []string{artistID}}
			}
		}
		for _, item := range response.Results {
			// Count every collection row the endpoint returned, not just the
			// ones that survive the filters: the cap applies to what Apple sends,
			// so a page filtered down to fewer usable releases is still a page
			// that hit the ceiling.
			if item.WrapperType == "collection" {
				collections++
			}
			if item.WrapperType != "collection" || item.CollectionType != "Album" ||
				item.CollectionID <= 0 || strings.TrimSpace(item.CollectionName) == "" ||
				strings.TrimSpace(item.ReleaseDate) == "" {
				continue
			}
			// Apple sends collectionArtistName only on wrapperType "track" rows
			// for compilations, never on the "collection" rows kept here, so
			// keying the guard off it left it dead: the value was always empty
			// and the != "" test skipped the comparison entirely. artistName is
			// the field collection rows actually carry, and exact-name-modulo-
			// case is already how resolveExactArtist establishes an iTunes
			// identity, so the same standard applies here.
			owner := strings.TrimSpace(item.ArtistName)
			if owner == "" {
				owner = strings.TrimSpace(item.CollectionArtistName)
			}
			if owner != "" && !strings.EqualFold(owner, artistName) {
				continue
			}
			id := strconv.FormatInt(item.CollectionID, 10)
			if seen[id] {
				continue
			}
			date, precision := iTunesDate(item.ReleaseDate)
			if date == "" {
				continue
			}
			primaryType, secondaryTypes := iTunesReleaseType(item.CollectionName, item.TrackCount, item.ArtistName)
			releaseURL := strings.TrimSpace(item.CollectionViewURL)
			if releaseURL == "" {
				releaseURL = "https://itunes.apple.com/album/id" + id
			}
			seen[id] = true
			result = append(result, store.Release{
				MBID:             "itunes:" + id,
				Title:            strings.TrimSpace(item.CollectionName),
				PrimaryType:      primaryType,
				SecondaryTypes:   secondaryTypes,
				FirstReleaseDate: date,
				DatePrecision:    precision,
				ITunesID:         id,
				ITunesURL:        releaseURL,
				ITunesArtworkURL: normalizeITunesArtworkURL(firstNonEmpty(item.ArtworkURL100, item.ArtworkURL60)),
				Source:           "itunes",
			})
		}
	}
	// The endpoint cannot return more than one page, so a full page is the
	// natural bound rather than an error: returning what was found keeps a
	// prolific artist's catalogue usable instead of discarding it. But a full
	// page also means the catalogue is incomplete, and that must not be reported
	// as a clean healthy check - it suppressed the MusicBrainz fallback that
	// would have supplied the missing releases.
	if collections >= lookupPageSize {
		return result, &ITunesCatalogTruncatedError{ArtistID: artistID, Limit: lookupPageSize}
	}
	return result, nil
}

// ArtistReleaseCredits performs one bounded song search to find iTunes tracks
// whose credited artist string includes the followed artist alongside another
// artist. It intentionally does not treat an exact artist-name match as a
// guest credit because those releases are already returned by ArtistReleases.
func (i *ITunes) ArtistReleaseCredits(ctx context.Context, artistName string, known []store.Release) ([]store.Release, error) {
	if i == nil {
		return nil, errors.New("iTunes is not configured")
	}
	artistName = strings.TrimSpace(artistName)
	if artistName == "" {
		return nil, errors.New("artist name is required")
	}
	endpoint := i.baseURL + "/search?term=" + url.QueryEscape(artistName) +
		"&country=" + url.QueryEscape(i.country) +
		"&media=music&entity=song&attribute=artistTerm&limit=50"
	var response itunesResponse
	if err := i.getJSON(ctx, "iTunes artist credits", endpoint, &response); err != nil {
		return nil, err
	}
	knownByID := make(map[string]store.Release, len(known))
	for _, release := range known {
		if strings.TrimSpace(release.ITunesID) != "" {
			knownByID[strings.TrimSpace(release.ITunesID)] = release
		}
	}
	result := make([]store.Release, 0)
	seen := make(map[string]bool)
	for _, item := range response.Results {
		if item.WrapperType != "track" || item.CollectionID <= 0 || item.TrackID <= 0 ||
			strings.TrimSpace(item.TrackName) == "" || strings.TrimSpace(item.CollectionName) == "" ||
			strings.TrimSpace(item.ReleaseDate) == "" ||
			!creditsFollowedArtist(item.ArtistName, item.TrackName, artistName) {
			continue
		}
		collectionID := strconv.FormatInt(item.CollectionID, 10)
		trackID := strconv.FormatInt(item.TrackID, 10)
		key := collectionID + "\x00" + trackID
		if seen[key] {
			continue
		}
		seen[key] = true
		release, ok := knownByID[collectionID]
		if !ok {
			date, precision := iTunesDate(item.ReleaseDate)
			if date == "" {
				continue
			}
			releaseURL := strings.TrimSpace(item.CollectionViewURL)
			if releaseURL == "" {
				releaseURL = "https://itunes.apple.com/album/id" + collectionID
			}
			// Song rows carry collectionArtistName, which names the album's
			// artist rather than the track's - the cleaner compilation signal,
			// and the only place Apple exposes it. Fall back to the track's
			// artist when it is absent.
			collectionArtist := strings.TrimSpace(item.CollectionArtistName)
			if collectionArtist == "" {
				collectionArtist = item.ArtistName
			}
			creditPrimaryType, creditSecondaryTypes := iTunesReleaseType(item.CollectionName, item.TrackCount, collectionArtist)
			release = store.Release{
				MBID: "itunes:" + collectionID, Title: strings.TrimSpace(item.CollectionName),
				PrimaryType:      creditPrimaryType,
				SecondaryTypes:   creditSecondaryTypes,
				FirstReleaseDate: date, DatePrecision: precision, ITunesID: collectionID, ITunesURL: releaseURL,
				ITunesArtworkURL: normalizeITunesArtworkURL(firstNonEmpty(item.ArtworkURL100, item.ArtworkURL60)),
			}
		}
		release.ArtistCreditRole = "featured"
		release.Credits = append(release.Credits, store.ReleaseCredit{
			Provider: "itunes", ProviderID: trackID, Role: "guest", TrackTitle: strings.TrimSpace(item.TrackName),
			CreditName: strings.TrimSpace(item.ArtistName), ProviderURL: strings.TrimSpace(release.ITunesURL), Confidence: "probable",
		})
		result = append(result, release)
	}
	return result, nil
}

// creditsFollowedArtist reports whether an iTunes song row is a GUEST credit for
// the followed artist.
//
// Apple expresses a feature two ways, and only the first was ever checked:
// inside artistName ("Ariana Grande, Normani & Nicki Minaj"), or - far more
// commonly - by leaving artistName as the lead artist and putting the feature in
// trackName ("Side To Side (feat. Nicki Minaj)"). Measured against the live
// endpoint on 2026-08-24 for one artist, 50 returned rows contained 31 whose
// credit appears only in trackName; every one of them was discarded even though
// the search had already paid for it.
//
// A row whose artistName IS the followed artist is still excluded: those are the
// artist's own releases and ArtistReleases already returns them, so admitting
// them here would duplicate the catalogue rather than extend it.
func creditsFollowedArtist(artistNameField, trackName, artist string) bool {
	if strings.EqualFold(strings.TrimSpace(artistNameField), strings.TrimSpace(artist)) {
		return false
	}
	return creditIncludesArtist(artistNameField, artist) || creditIncludesArtist(trackName, artist)
}

func creditIncludesArtist(credit, artist string) bool {
	credit = normalizeCreditText(credit)
	artist = normalizeCreditText(artist)
	if credit == "" || artist == "" || len([]rune(artist)) < 3 {
		return false
	}
	return strings.Contains(" "+credit+" ", " "+artist+" ")
}

func normalizeCreditText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	space := true
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// normalizeITunesArtworkURL keeps only Apple-hosted artwork metadata. The
// image itself is loaded by the browser and is never downloaded or cached by
// ArtistTrackarr.
func normalizeITunesArtworkURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || parsed.Path == "" || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "mzstatic.com" && !strings.HasSuffix(host, ".mzstatic.com") &&
		host != "itunes.apple.com" && !strings.HasSuffix(host, ".itunes.apple.com") {
		return ""
	}
	parsed.Scheme = "https"
	parsed.User = nil
	// iTunes artwork paths commonly contain a 60x60 or 100x100 size token.
	// Request a larger rendition when the provider exposes that convention,
	// while keeping the original URL for other valid Apple paths.
	for _, size := range []string{"100x100", "60x60"} {
		if strings.Contains(parsed.Path, size) {
			parsed.Path = strings.Replace(parsed.Path, size, "250x250", 1)
			break
		}
	}
	return parsed.String()
}

func (i *ITunes) getJSON(ctx context.Context, operation, endpoint string, target any) error {
	i.requestMu.Lock()
	defer i.requestMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if err := i.waitUntilReady(ctx, operation); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := i.client.Do(req)
		i.lastRequest = time.Now()
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, target); err != nil {
				return err
			}
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := iTunesRetryAfter(resp.Header.Get("Retry-After"))
			i.blockedUntil = time.Now().Add(retryAfter)
			i.blockedReason = "rate limited"
			if retryAfter <= 10*time.Second && attempt == 0 {
				if err := i.wait(ctx, retryAfter); err != nil {
					return err
				}
				continue
			}
			return &ITunesRateLimitError{Operation: operation, Status: resp.StatusCode, Reason: "RATE_LIMITED", RetryAfter: retryAfter}
		}
		return &ITunesAPIError{Operation: operation, Status: resp.StatusCode, StatusText: resp.Status}
	}
	return errors.New(operation + " failed after retries")
}

func (i *ITunes) waitUntilReady(ctx context.Context, operation string) error {
	if remaining := time.Until(i.blockedUntil); remaining > 0 {
		return &ITunesRateLimitError{
			Operation: operation, Status: http.StatusTooManyRequests, Reason: "RATE_LIMITED", RetryAfter: remaining, AlreadyBlocked: true,
		}
	}
	if remaining := i.requestInterval - time.Since(i.lastRequest); remaining > 0 {
		if err := i.wait(ctx, remaining); err != nil {
			return err
		}
	}
	return nil
}

func iTunesRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	seconds, err := strconv.Atoi(value)
	if err != nil {
		if at, dateErr := http.ParseTime(value); dateErr == nil {
			if delay := time.Until(at); delay > 0 {
				return min(delay, 6*time.Hour)
			}
		}
		return time.Minute
	}
	if seconds <= 0 {
		return time.Minute
	}
	delay := time.Duration(seconds) * time.Second
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	return delay
}

func iTunesDate(value string) (string, int) {
	value = strings.TrimSpace(value)
	if len(value) >= 10 {
		if _, err := time.Parse("2006-01-02", value[:10]); err == nil {
			return value[:10], 3
		}
	}
	if len(value) >= 7 {
		if _, err := time.Parse("2006-01", value[:7]); err == nil {
			return value[:7], 2
		}
	}
	if len(value) >= 4 {
		if _, err := time.Parse("2006", value[:4]); err == nil {
			return value[:4], 1
		}
	}
	return "", 0
}

// variousArtistsLabels are the storefront-localised names Apple uses for the
// compilation pseudo-artist, in the form normalizeCreditText produces. That
// normaliser keeps only ASCII letters and digits, so accented labels appear here
// already mangled ("Multi-interprètes" -> "multi interpr tes"); the values are
// taken from its actual output rather than written by hand.
//
// The album-lookup payload carries no compilation flag - no collectionType
// distinction, no collectionArtistName on collection rows - so the artist label
// is the only signal it offers, and it is localised per storefront. Verified
// live on 2026-08-24 against one compilation across five storefronts: US and GB
// return "Various Artists", DE and FR "Multi-interprètes", JP the katakana form
// below.
//
// This is best-effort by construction: a storefront whose label is absent here
// classifies its compilations as plain albums, which is what every storefront
// did before. It cannot produce a false positive, because no real artist is
// named any of these.
var variousArtistsLabels = map[string]struct{}{
	"various artists":          {},
	"various artist":           {},
	"multi interpr tes":        {},
	"verschiedene interpreten": {},
	"varios artistas":          {},
	"artisti vari":             {},
	"diversos artistas":        {},
	"olika artister":           {},
	"forskellige kunstnere":    {},
	"eri esitt ji":             {},
}

// variousArtistsRawMarkers cover labels the ASCII-only normaliser reduces to the
// empty string, so they must be matched before normalisation.
var variousArtistsRawMarkers = []string{"ヴァリアス"}

// isVariousArtists reports whether an iTunes artist label denotes a compilation.
func isVariousArtists(name string) bool {
	for _, marker := range variousArtistsRawMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}
	normalized := normalizeCreditText(name)
	if normalized == "" {
		return false
	}
	_, ok := variousArtistsLabels[normalized]
	return ok
}

// iTunesReleaseType classifies an iTunes collection, returning any secondary
// types alongside the primary one.
//
// The secondary types used to be discarded, and the kind was hard-coded to
// "album", so the single branch that emits "Compilation" could never fire for an
// iTunes release. store.Release.SecondaryTypes was therefore always nil and the
// per-follow Compilations toggle had nothing to match: it worked on a Spotify
// deployment and silently did nothing on an iTunes one.
func iTunesReleaseType(title string, trackCount int, artistName string) (string, []string) {
	kind := "album"
	if isVariousArtists(artistName) {
		kind = "compilation"
	}
	primaryType, secondary, ok := classifyReleaseType(kind, "", title, trackCount)
	if !ok {
		return "Album", nil
	}
	return primaryType, secondary
}

func validITunesID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizeITunesQuery(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func cloneITunesArtists(input []ITunesArtist) []ITunesArtist {
	return append([]ITunesArtist(nil), input...)
}

func cloneITunesReleases(input []store.Release) []store.Release {
	return append([]store.Release(nil), input...)
}

func (i *ITunes) cachedSearch(key string) ([]ITunesArtist, bool) {
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	entry, ok := i.searchCache[key]
	if !ok || !time.Now().Before(entry.expiresAt) {
		if ok {
			delete(i.searchCache, key)
		}
		return nil, false
	}
	return cloneITunesArtists(entry.results), true
}

func (i *ITunes) cacheSearchLocked(key string, results []ITunesArtist) {
	ttl := i.searchTTL
	if len(results) == 0 && i.emptySearchTTL > 0 {
		ttl = i.emptySearchTTL
	}
	now := time.Now()
	i.searchCache[key] = itunesSearchCache{observedAt: now, expiresAt: now.Add(ttl), results: cloneITunesArtists(results)}
	maxEntries := i.maxSearchCache
	if maxEntries <= 0 {
		maxEntries = 256
	}
	for len(i.searchCache) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range i.searchCache {
			if oldestKey == "" || entry.observedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.observedAt
			}
		}
		delete(i.searchCache, oldestKey)
	}
}

// InvalidateArtistReleases drops cached release pages for one artist so a due
// scheduled check reaches the provider. The release cache exists for short
// discovery and follow bursts; at a poll interval shorter than the cache TTL a
// scheduled check would otherwise replay a stale response, and because a
// non-empty replay counts as a successful observation it also suppresses the
// MusicBrainz fallback and records provider health for a request that was never
// made. Both the canonical and the name-based key forms are covered.
func (i *ITunes) InvalidateArtistReleases(canonicalID, providerID string) {
	if i == nil {
		return
	}
	canonicalID = strings.TrimSpace(canonicalID)
	providerID = strings.TrimSpace(providerID)
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	for key := range i.releaseCache {
		if canonicalID != "" && strings.HasPrefix(key, "canonical:"+canonicalID+"\x00") {
			delete(i.releaseCache, key)
			continue
		}
		if providerID != "" && (key == "name:"+providerID || strings.HasSuffix(key, "\x00"+providerID)) {
			delete(i.releaseCache, key)
		}
	}
}

func (i *ITunes) cachedReleases(key string) ([]store.Release, bool) {
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	entry, ok := i.releaseCache[key]
	if !ok || !time.Now().Before(entry.expiresAt) {
		if ok {
			delete(i.releaseCache, key)
		}
		return nil, false
	}
	return cloneITunesReleases(entry.results), true
}

func (i *ITunes) cachedArtist(id string) (ITunesArtist, bool) {
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	entry, ok := i.artistCache[id]
	if !ok || (!entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt)) {
		if ok {
			delete(i.artistCache, id)
		}
		return ITunesArtist{}, false
	}
	return entry.artist, true
}

func (i *ITunes) cacheArtistLocked(artist ITunesArtist) {
	id := strings.TrimSpace(artist.ID)
	if id == "" {
		return
	}
	if i.artistCache == nil {
		i.artistCache = make(map[string]itunesArtistCache)
	}
	ttl := i.artistTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now()
	i.artistCache[id] = itunesArtistCache{observedAt: now, expiresAt: now.Add(ttl), artist: artist}
	maxEntries := i.maxArtistCache
	if maxEntries <= 0 {
		maxEntries = 512
	}
	for len(i.artistCache) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range i.artistCache {
			if oldestKey == "" || entry.observedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.observedAt
			}
		}
		delete(i.artistCache, oldestKey)
	}
}

func (i *ITunes) cacheReleasesLocked(key string, results []store.Release) {
	now := time.Now()
	i.releaseCache[key] = itunesReleaseCache{observedAt: now, expiresAt: now.Add(i.releaseTTL), results: cloneITunesReleases(results)}
	maxEntries := i.maxReleaseCache
	if maxEntries <= 0 {
		maxEntries = 512
	}
	for len(i.releaseCache) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range i.releaseCache {
			if oldestKey == "" || entry.observedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.observedAt
			}
		}
		delete(i.releaseCache, oldestKey)
	}
}
