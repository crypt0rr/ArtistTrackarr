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

	cacheMu        sync.Mutex
	searchCache    map[string]itunesSearchCache
	releaseCache   map[string]itunesReleaseCache
	searchCalls    map[string]*itunesSearchCall
	releaseCalls   map[string]*itunesReleaseCall
	searchTTL      time.Duration
	emptySearchTTL time.Duration
	releaseTTL     time.Duration
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

type itunesSearchCache struct {
	expiresAt time.Time
	results   []ITunesArtist
}

type itunesReleaseCache struct {
	expiresAt time.Time
	results   []store.Release
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
		searchCalls:     make(map[string]*itunesSearchCall),
		releaseCalls:    make(map[string]*itunesReleaseCall),
		searchTTL:       10 * time.Minute,
		emptySearchTTL:  2 * time.Minute,
		releaseTTL:      24 * time.Hour,
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
			return cloneITunesArtists(call.results), call.err
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
	return ITunesArtist{}, errors.New("iTunes artist was not found")
}

func (i *ITunes) ArtistReleases(ctx context.Context, artistName string) ([]store.Release, error) {
	if i == nil {
		return nil, errors.New("iTunes is not configured")
	}
	artistName = strings.TrimSpace(artistName)
	if artistName == "" {
		return nil, errors.New("artist name is required")
	}
	key := normalizeITunesQuery(artistName)
	if cached, ok := i.cachedReleases(key); ok {
		return cached, nil
	}

	i.cacheMu.Lock()
	if call, ok := i.releaseCalls[key]; ok {
		i.cacheMu.Unlock()
		select {
		case <-call.done:
			return cloneITunesReleases(call.results), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &itunesReleaseCall{done: make(chan struct{})}
	i.releaseCalls[key] = call
	i.cacheMu.Unlock()

	result, err := i.artistReleases(ctx, artistName)
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

func (i *ITunes) artistReleases(ctx context.Context, artistName string) ([]store.Release, error) {
	artists, err := i.SearchArtists(ctx, artistName)
	if err != nil {
		return nil, err
	}
	var artistID string
	for _, artist := range artists {
		if strings.EqualFold(strings.TrimSpace(artist.Name), artistName) {
			artistID = artist.ID
			break
		}
	}
	if artistID == "" {
		return nil, fmt.Errorf("iTunes artist %q was not found", artistName)
	}
	endpoint := i.baseURL + "/lookup?id=" + url.QueryEscape(artistID) +
		"&country=" + url.QueryEscape(i.country) + "&entity=album&limit=200"
	var response itunesResponse
	if err := i.getJSON(ctx, "iTunes artist albums", endpoint, &response); err != nil {
		return nil, err
	}
	result := make([]store.Release, 0, len(response.Results))
	seen := make(map[string]bool)
	for _, item := range response.Results {
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
			Source:           "itunes",
		})
	}
	return result, nil
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
		resp.Body.Close()
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
	lower := strings.ToLower(strings.TrimSpace(title))
	if trackCount == 1 || containsReleaseWord(lower, "single") {
		return "Single"
	}
	if (trackCount >= 2 && trackCount <= 6) || containsReleaseWord(lower, "ep") {
		return "EP"
	}
	return "Album"
}

func containsReleaseWord(title, word string) bool {
	for _, part := range strings.FieldsFunc(title, func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if part == word {
			return true
		}
	}
	return false
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
	i.searchCache[key] = itunesSearchCache{expiresAt: time.Now().Add(ttl), results: cloneITunesArtists(results)}
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

func (i *ITunes) cacheReleasesLocked(key string, results []store.Release) {
	i.releaseCache[key] = itunesReleaseCache{expiresAt: time.Now().Add(i.releaseTTL), results: cloneITunesReleases(results)}
}
