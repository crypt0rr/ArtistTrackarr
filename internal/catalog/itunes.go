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
	if err != nil {
		return nil, "", "", err
	}
	return releases, artist.ID, artist.URL, nil
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

func (i *ITunes) artistReleasesByID(ctx context.Context, artistName, artistID string) ([]store.Release, error) {
	const pageSize = 200
	const maxPages = 100
	result := make([]store.Release, 0)
	seen := make(map[string]bool)
	for page := 0; page < maxPages; page++ {
		offset := page * pageSize
		endpoint := i.baseURL + "/lookup?id=" + url.QueryEscape(artistID) +
			"&country=" + url.QueryEscape(i.country) + "&entity=album&limit=" + strconv.Itoa(pageSize) + "&offset=" + strconv.Itoa(offset)
		var response itunesResponse
		if err := i.getJSON(ctx, "iTunes artist albums", endpoint, &response); err != nil {
			return nil, err
		}
		collectionCount := 0
		for _, item := range response.Results {
			// Lookup responses may include the artist object alongside the
			// collection rows. Count only collections when deciding whether a
			// full page was returned; otherwise a page of 199 albums plus the
			// artist row would cause an unnecessary extra request.
			if item.WrapperType == "collection" {
				collectionCount++
			}
			if item.WrapperType != "collection" || item.CollectionType != "Album" ||
				item.CollectionID <= 0 || strings.TrimSpace(item.CollectionName) == "" ||
				strings.TrimSpace(item.ReleaseDate) == "" {
				continue
			}
			if item.CollectionArtistName != "" && !strings.EqualFold(strings.TrimSpace(item.CollectionArtistName), artistName) {
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
			primaryType := iTunesReleaseType(item.CollectionName, item.TrackCount)
			releaseURL := strings.TrimSpace(item.CollectionViewURL)
			if releaseURL == "" {
				releaseURL = "https://itunes.apple.com/album/id" + id
			}
			seen[id] = true
			result = append(result, store.Release{
				MBID:             "itunes:" + id,
				Title:            strings.TrimSpace(item.CollectionName),
				PrimaryType:      primaryType,
				FirstReleaseDate: date,
				DatePrecision:    precision,
				ITunesID:         id,
				ITunesURL:        releaseURL,
				ITunesArtworkURL: normalizeITunesArtworkURL(firstNonEmpty(item.ArtworkURL100, item.ArtworkURL60)),
				Source:           "itunes",
			})
		}
		if collectionCount < pageSize {
			return result, nil
		}
	}
	return nil, &CatalogLimitError{Provider: "iTunes", Pages: maxPages}
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
			strings.TrimSpace(item.ReleaseDate) == "" || !creditIncludesArtist(item.ArtistName, artistName) ||
			strings.EqualFold(strings.TrimSpace(item.ArtistName), artistName) {
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
			release = store.Release{
				MBID: "itunes:" + collectionID, Title: strings.TrimSpace(item.CollectionName),
				PrimaryType:      iTunesReleaseType(item.CollectionName, item.TrackCount),
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

func iTunesReleaseType(title string, trackCount int) string {
	primaryType, _, ok := classifyReleaseType("album", "", title, trackCount)
	if !ok {
		return "Album"
	}
	return primaryType
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
