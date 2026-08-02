package catalog

import (
	"context"
	"encoding/base64"
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
	"github.com/crypt0rr/artist-tracker/internal/version"
)

type CatalogProvider interface {
	SearchArtists(context.Context, string, int) ([]ArtistResult, error)
	ResolveArtist(context.Context, string) (ArtistResult, error)
	ResolveExternalArtist(context.Context, string) ([]ArtistResult, error)
	ArtistReleases(context.Context, string) ([]store.Release, error)
}

type ReleaseNormalizer interface {
	Normalize([]store.Release) []store.Release
}

type SpotifyProvider interface {
	SearchArtists(context.Context, string) ([]SpotifyArtist, error)
	Artist(context.Context, string) (SpotifyArtist, error)
}

// SpotifyBatchArtistProvider is implemented by providers that can resolve
// several artist IDs with one Web API request. It is optional so callers can
// continue to work with small test providers and other implementations.
type SpotifyBatchArtistProvider interface {
	Artists(context.Context, []string) ([]SpotifyArtist, error)
}

type SpotifyReleaseProvider interface {
	ArtistReleases(context.Context, string) ([]store.Release, error)
}

type SpotifyIncrementalReleaseProvider interface {
	SpotifyReleaseProvider
	ArtistReleasesSince(context.Context, string, string) ([]store.Release, error)
}

type ArtistResult struct {
	MBID            string
	Name            string
	SortName        string
	Type            string
	Country         string
	Disambiguation  string
	Aliases         []string
	SpotifyID       string
	SpotifyURL      string
	SpotifyImageURL string
	Score           int
}

func (a ArtistResult) StoreArtist() store.Artist {
	return store.Artist{
		MBID: a.MBID, Name: a.Name, SortName: a.SortName, Type: a.Type,
		Country: a.Country, Disambiguation: a.Disambiguation,
		SpotifyID: a.SpotifyID, SpotifyURL: a.SpotifyURL, SpotifyImageURL: a.SpotifyImageURL,
	}
}

type MusicBrainz struct {
	client    *http.Client
	userAgent string
	baseURL   string
	interval  time.Duration
	retryBase time.Duration
	mu        sync.Mutex
	lastCall  time.Time
}

type HTTPStatusError struct {
	Provider string
	Status   int
	Text     string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s returned %s", e.Provider, e.Text)
}

func NewMusicBrainz(contact string) *MusicBrainz {
	return &MusicBrainz{
		client:    &http.Client{Timeout: 20 * time.Second},
		userAgent: fmt.Sprintf("%s (%s)", version.UserAgent, contact),
		baseURL:   "https://musicbrainz.org",
		interval:  time.Second,
		retryBase: 250 * time.Millisecond,
	}
}

func (m *MusicBrainz) wait(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if remaining := m.interval - time.Since(m.lastCall); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	m.lastCall = time.Now()
	return nil
}

func (m *MusicBrainz) getJSON(ctx context.Context, endpoint string, target any) error {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if err := m.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", m.userAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := m.client.Do(req)
		if err != nil {
			last = fmt.Errorf("request MusicBrainz: %w", err)
			if err := m.waitForRetry(ctx, attempt, ""); err != nil {
				return err
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			last = fmt.Errorf("read MusicBrainz response: %w", readErr)
			if err := m.waitForRetry(ctx, attempt, ""); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, target); err != nil {
				return fmt.Errorf("decode MusicBrainz response: %w", err)
			}
			return nil
		}
		last = &HTTPStatusError{Provider: "MusicBrainz", Status: resp.StatusCode, Text: resp.Status}
		if !transientStatus(resp.StatusCode) {
			return last
		}
		if err := m.waitForRetry(ctx, attempt, resp.Header.Get("Retry-After")); err != nil {
			return err
		}
	}
	return last
}

func transientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func (m *MusicBrainz) waitForRetry(ctx context.Context, attempt int, retryAfter string) error {
	if attempt >= 3 {
		return nil
	}
	delay := time.Duration(1<<attempt) * m.retryBase
	if seconds, err := time.ParseDuration(strings.TrimSpace(retryAfter) + "s"); err == nil && seconds > delay {
		delay = min(seconds, 30*time.Second)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *MusicBrainz) SearchArtists(ctx context.Context, query string, limit int) ([]ArtistResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search query is required")
	}
	if limit < 1 || limit > 25 {
		limit = 10
	}
	endpoint := m.baseURL + "/ws/2/artist?fmt=json&limit=" +
		fmt.Sprint(limit) + "&query=" + url.QueryEscape(query)
	var response struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			SortName       string `json:"sort-name"`
			Type           string `json:"type"`
			Country        string `json:"country"`
			Disambiguation string `json:"disambiguation"`
			Score          int    `json:"score"`
			Aliases        []struct {
				Name string `json:"name"`
			} `json:"aliases"`
		} `json:"artists"`
	}
	if err := m.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	result := make([]ArtistResult, 0, len(response.Artists))
	for _, item := range response.Artists {
		a := ArtistResult{
			MBID: item.ID, Name: item.Name, SortName: item.SortName, Type: item.Type,
			Country: item.Country, Disambiguation: item.Disambiguation, Score: item.Score,
		}
		for _, alias := range item.Aliases {
			if len(a.Aliases) == 3 {
				break
			}
			a.Aliases = append(a.Aliases, alias.Name)
		}
		result = append(result, a)
	}
	return result, nil
}

func (m *MusicBrainz) ResolveArtist(ctx context.Context, mbid string) (ArtistResult, error) {
	mbid = extractMBID(mbid)
	if !validMBID(mbid) {
		return ArtistResult{}, errors.New("invalid MusicBrainz artist ID")
	}
	endpoint := m.baseURL + "/ws/2/artist/" + url.PathEscape(mbid) + "?fmt=json&inc=aliases"
	var item struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SortName       string `json:"sort-name"`
		Type           string `json:"type"`
		Country        string `json:"country"`
		Disambiguation string `json:"disambiguation"`
		Aliases        []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := m.getJSON(ctx, endpoint, &item); err != nil {
		return ArtistResult{}, err
	}
	return ArtistResult{
		MBID: item.ID, Name: item.Name, SortName: item.SortName, Type: item.Type,
		Country: item.Country, Disambiguation: item.Disambiguation,
	}, nil
}

func (m *MusicBrainz) ResolveExternalArtist(ctx context.Context, externalURL string) ([]ArtistResult, error) {
	externalURL = strings.TrimSpace(externalURL)
	parsed, err := url.Parse(externalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("invalid external artist URL")
	}
	endpoint := m.baseURL + "/ws/2/url?fmt=json&inc=artist-rels&resource=" + url.QueryEscape(externalURL)
	var response struct {
		Relations []struct {
			Artist *struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				SortName       string `json:"sort-name"`
				Type           string `json:"type"`
				Country        string `json:"country"`
				Disambiguation string `json:"disambiguation"`
			} `json:"artist"`
		} `json:"relations"`
	}
	if err := m.getJSON(ctx, endpoint, &response); err != nil {
		var status *HTTPStatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]bool)
	var result []ArtistResult
	for _, relation := range response.Relations {
		if relation.Artist == nil || !validMBID(relation.Artist.ID) || seen[relation.Artist.ID] {
			continue
		}
		seen[relation.Artist.ID] = true
		result = append(result, ArtistResult{
			MBID: relation.Artist.ID, Name: relation.Artist.Name, SortName: relation.Artist.SortName,
			Type: relation.Artist.Type, Country: relation.Artist.Country,
			Disambiguation: relation.Artist.Disambiguation,
		})
	}
	return result, nil
}

func (m *MusicBrainz) ArtistReleases(ctx context.Context, mbid string) ([]store.Release, error) {
	var all []store.Release
	offset := 0
	for {
		endpoint := fmt.Sprintf("%s/ws/2/release-group?fmt=json&artist=%s&type=album%%7Cep%%7Csingle&release-group-status=website-default&limit=100&offset=%d",
			m.baseURL, url.QueryEscape(mbid), offset)
		var response struct {
			Count         int `json:"release-group-count"`
			ReleaseGroups []struct {
				ID               string   `json:"id"`
				Title            string   `json:"title"`
				PrimaryType      string   `json:"primary-type"`
				SecondaryTypes   []string `json:"secondary-types"`
				FirstReleaseDate string   `json:"first-release-date"`
			} `json:"release-groups"`
		}
		if err := m.getJSON(ctx, endpoint, &response); err != nil {
			return nil, err
		}
		for _, item := range response.ReleaseGroups {
			precision := 0
			switch len(item.FirstReleaseDate) {
			case 4:
				precision = 1
			case 7:
				precision = 2
			case 10:
				precision = 3
			}
			all = append(all, store.Release{
				MBID: item.ID, Title: item.Title, PrimaryType: item.PrimaryType,
				SecondaryTypes: item.SecondaryTypes, FirstReleaseDate: item.FirstReleaseDate,
				DatePrecision: precision, MusicBrainzURL: "https://musicbrainz.org/release-group/" + item.ID,
			})
		}
		offset += len(response.ReleaseGroups)
		if offset >= response.Count || len(response.ReleaseGroups) == 0 {
			break
		}
	}
	return all, nil
}

type AlbumEPNormalizer struct{}

func (AlbumEPNormalizer) Normalize(input []store.Release) []store.Release {
	seen := make(map[string]bool)
	output := make([]store.Release, 0, len(input))
	for _, release := range input {
		if release.PrimaryType != "Album" && release.PrimaryType != "EP" && release.PrimaryType != "Single" {
			continue
		}
		if release.MBID == "" || seen[release.MBID] {
			continue
		}
		seen[release.MBID] = true
		output = append(output, release)
	}
	return output
}

type Spotify struct {
	clientID, secret string
	client           *http.Client
	accountsURL      string
	apiURL           string
	market           string
	tokenMu          sync.Mutex
	token            string
	expires          time.Time
	requestMu        sync.Mutex
	lastRequest      time.Time
	blockedUntil     time.Time
	blockedReason    string
	blockedQuota     bool
	requestInterval  time.Duration
	retryBase        time.Duration
	maxInlineRetry   time.Duration
	wait             func(context.Context, time.Duration) error
	cacheMu          sync.Mutex
	releaseCache     map[string]spotifyReleaseCache
	cacheTTL         time.Duration
	searchCache      map[string]spotifySearchCache
	artistCache      map[string]spotifyArtistCache
	searchCalls      map[string]*spotifySearchCall
	artistCalls      map[string]*spotifyArtistCall
	searchCacheTTL   time.Duration
	emptySearchTTL   time.Duration
	artistCacheTTL   time.Duration
	maxSearchCache   int
	maxArtistCache   int
}

type spotifyReleaseCache struct {
	observedAt time.Time
	oldestDate string
	releases   []store.Release
}

type spotifySearchCache struct {
	observedAt time.Time
	expiresAt  time.Time
	results    []SpotifyArtist
}

type spotifyArtistCache struct {
	observedAt time.Time
	expiresAt  time.Time
	artist     SpotifyArtist
}

type spotifySearchCall struct {
	done    chan struct{}
	results []SpotifyArtist
	err     error
}

type spotifyArtistCall struct {
	done   chan struct{}
	artist SpotifyArtist
	err    error
}

type SpotifyArtist struct {
	ID, Name, URL, ImageURL string
}

type SpotifyRateLimitError struct {
	Operation      string
	Status         int
	Reason         string
	RetryAfter     time.Duration
	QuotaExceeded  bool
	AlreadyBlocked bool
}

func (e *SpotifyRateLimitError) Error() string {
	message := e.Operation + " returned 429 Too Many Requests"
	if e.Reason != "" {
		message += " (" + e.Reason + ")"
	}
	if e.RetryAfter > 0 {
		message += "; retry after " + e.RetryAfter.Round(time.Second).String()
	}
	return message
}

func NewSpotify(id, secret string, market ...string) *Spotify {
	if id == "" || secret == "" {
		return nil
	}
	selectedMarket := "US"
	if len(market) > 0 && strings.TrimSpace(market[0]) != "" {
		selectedMarket = strings.ToUpper(strings.TrimSpace(market[0]))
	}
	return &Spotify{
		clientID: id, secret: secret, client: &http.Client{Timeout: 15 * time.Second},
		accountsURL: "https://accounts.spotify.com", apiURL: "https://api.spotify.com", market: selectedMarket,
		requestInterval: time.Second, retryBase: time.Second, maxInlineRetry: 30 * time.Second,
		wait: waitContext, releaseCache: make(map[string]spotifyReleaseCache), cacheTTL: 24 * time.Hour,
		searchCache: make(map[string]spotifySearchCache), artistCache: make(map[string]spotifyArtistCache),
		searchCalls: make(map[string]*spotifySearchCall), artistCalls: make(map[string]*spotifyArtistCall),
		searchCacheTTL: 10 * time.Minute, emptySearchTTL: 2 * time.Minute, artistCacheTTL: 24 * time.Hour,
		maxSearchCache: 256, maxArtistCache: 512,
	}
}

func (s *Spotify) accessToken(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.token != "" && time.Now().Before(s.expires.Add(-time.Minute)) {
		return s.token, nil
	}
	form := strings.NewReader("grant_type=client_credentials")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.accountsURL+"/api/token", form)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.clientID+":"+s.secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Spotify token endpoint returned %s", resp.Status)
	}
	var result struct {
		Token     string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	s.token, s.expires = result.Token, time.Now().Add(time.Duration(result.ExpiresIn)*time.Second)
	return s.token, nil
}

func (s *Spotify) SearchArtists(ctx context.Context, query string) ([]SpotifyArtist, error) {
	if s == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("search query is required")
	}
	key := normalizeSpotifySearchQuery(query)
	if cached, ok := s.cachedSearch(key); ok {
		return cached, nil
	}

	// Search is user-driven and multiple browser requests can arrive at once
	// (for example after a refresh). Coalesce an identical in-flight query so
	// only the first request consumes a Spotify quota slot.
	s.cacheMu.Lock()
	if call, ok := s.searchCalls[key]; ok {
		s.cacheMu.Unlock()
		select {
		case <-call.done:
			return cloneSpotifyArtists(call.results), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &spotifySearchCall{done: make(chan struct{})}
	s.searchCalls[key] = call
	s.cacheMu.Unlock()

	result, err := s.searchArtists(ctx, query)
	s.cacheMu.Lock()
	delete(s.searchCalls, key)
	call.results = cloneSpotifyArtists(result)
	call.err = err
	if err == nil {
		s.cacheSearchLocked(key, result)
		for _, artist := range result {
			s.cacheArtistLocked(artist)
		}
	}
	close(call.done)
	s.cacheMu.Unlock()
	return result, err
}

func (s *Spotify) searchArtists(ctx context.Context, query string) ([]SpotifyArtist, error) {
	endpoint := s.apiURL + "/v1/search?type=artist&limit=10&q=" + url.QueryEscape(query)
	var response struct {
		Artists struct {
			Items []struct {
				ID           string            `json:"id"`
				Name         string            `json:"name"`
				ExternalURLs map[string]string `json:"external_urls"`
				Images       []struct {
					URL string `json:"url"`
				} `json:"images"`
			} `json:"items"`
		} `json:"artists"`
	}
	if err := s.getAPIJSON(ctx, "Spotify search", endpoint, &response); err != nil {
		return nil, err
	}
	var result []SpotifyArtist
	for _, item := range response.Artists.Items {
		a := SpotifyArtist{ID: item.ID, Name: item.Name, URL: item.ExternalURLs["spotify"]}
		if len(item.Images) > 0 {
			a.ImageURL = item.Images[len(item.Images)-1].URL
		}
		result = append(result, a)
	}
	return result, nil
}

func (s *Spotify) Artist(ctx context.Context, id string) (SpotifyArtist, error) {
	if s == nil {
		return SpotifyArtist{}, errors.New("Spotify is not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return SpotifyArtist{}, errors.New("Spotify artist ID is required")
	}
	if cached, ok := s.cachedArtist(id); ok {
		return cached, nil
	}

	// A follow form normally arrives immediately after a search. Search
	// results populate this cache, so verifying the selected artist does not
	// issue a second request in the common path.
	s.cacheMu.Lock()
	if call, ok := s.artistCalls[id]; ok {
		s.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.artist, call.err
		case <-ctx.Done():
			return SpotifyArtist{}, ctx.Err()
		}
	}
	call := &spotifyArtistCall{done: make(chan struct{})}
	s.artistCalls[id] = call
	s.cacheMu.Unlock()

	artist, err := s.artist(ctx, id)
	s.cacheMu.Lock()
	delete(s.artistCalls, id)
	call.artist = artist
	call.err = err
	if err == nil {
		s.cacheArtistLocked(artist)
	}
	close(call.done)
	s.cacheMu.Unlock()
	return artist, err
}

func (s *Spotify) artist(ctx context.Context, id string) (SpotifyArtist, error) {
	var item spotifyArtistPayload
	endpoint := s.apiURL + "/v1/artists/" + url.PathEscape(id)
	if err := s.getAPIJSON(ctx, "Spotify artist lookup", endpoint, &item); err != nil {
		return SpotifyArtist{}, err
	}
	return item.artist(), nil
}

// Artists resolves up to Spotify's batch endpoint limit. Cached IDs are
// returned without a network request; only missing IDs are sent to Spotify.
func (s *Spotify) Artists(ctx context.Context, ids []string) ([]SpotifyArtist, error) {
	if s == nil {
		return nil, errors.New("Spotify is not configured")
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, errors.New("Spotify artist IDs are required")
	}
	if len(unique) > 50 {
		return nil, errors.New("Spotify artist batch is limited to 50 IDs")
	}

	resultByID := make(map[string]SpotifyArtist, len(unique))
	var missing []string
	for _, id := range unique {
		if artist, ok := s.cachedArtist(id); ok {
			resultByID[id] = artist
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		endpoint := s.apiURL + "/v1/artists?ids=" + url.QueryEscape(strings.Join(missing, ","))
		var response struct {
			Artists []*spotifyArtistPayload `json:"artists"`
		}
		if err := s.getAPIJSON(ctx, "Spotify artist batch lookup", endpoint, &response); err != nil {
			return nil, err
		}
		for _, payload := range response.Artists {
			if payload == nil || payload.ID == "" {
				continue
			}
			artist := payload.artist()
			resultByID[artist.ID] = artist
			s.cacheArtist(artist)
		}
	}
	result := make([]SpotifyArtist, 0, len(resultByID))
	for _, id := range unique {
		if artist, ok := resultByID[id]; ok {
			result = append(result, artist)
		}
	}
	return result, nil
}

type spotifyArtistPayload struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ExternalURLs map[string]string `json:"external_urls"`
	Images       []struct {
		URL string `json:"url"`
	} `json:"images"`
}

func (p spotifyArtistPayload) artist() SpotifyArtist {
	artist := SpotifyArtist{ID: p.ID, Name: p.Name, URL: p.ExternalURLs["spotify"]}
	if len(p.Images) > 0 {
		artist.ImageURL = p.Images[len(p.Images)-1].URL
	}
	return artist
}

func normalizeSpotifySearchQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func cloneSpotifyArtists(input []SpotifyArtist) []SpotifyArtist {
	return append([]SpotifyArtist(nil), input...)
}

func (s *Spotify) cachedSearch(key string) ([]SpotifyArtist, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.searchCache[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt) {
		delete(s.searchCache, key)
		return nil, false
	}
	return cloneSpotifyArtists(entry.results), true
}

func (s *Spotify) cacheSearchLocked(key string, results []SpotifyArtist) {
	if s.searchCache == nil {
		s.searchCache = make(map[string]spotifySearchCache)
	}
	ttl := s.searchCacheTTL
	if len(results) == 0 && s.emptySearchTTL > 0 {
		ttl = s.emptySearchTTL
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	now := time.Now()
	s.searchCache[key] = spotifySearchCache{
		observedAt: now, expiresAt: now.Add(ttl), results: cloneSpotifyArtists(results),
	}
	s.evictSearchCacheLocked()
}

func (s *Spotify) evictSearchCacheLocked() {
	maxEntries := s.maxSearchCache
	if maxEntries <= 0 {
		maxEntries = 256
	}
	for len(s.searchCache) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.searchCache {
			if oldestKey == "" || entry.observedAt.Before(oldest) {
				oldestKey, oldest = key, entry.observedAt
			}
		}
		delete(s.searchCache, oldestKey)
	}
}

func (s *Spotify) cachedArtist(id string) (SpotifyArtist, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.artistCache[id]
	if !ok {
		return SpotifyArtist{}, false
	}
	if !entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt) {
		delete(s.artistCache, id)
		return SpotifyArtist{}, false
	}
	return entry.artist, true
}

func (s *Spotify) cacheArtist(artist SpotifyArtist) {
	s.cacheMu.Lock()
	s.cacheArtistLocked(artist)
	s.cacheMu.Unlock()
}

func (s *Spotify) cacheArtistLocked(artist SpotifyArtist) {
	id := strings.TrimSpace(artist.ID)
	if id == "" {
		return
	}
	if s.artistCache == nil {
		s.artistCache = make(map[string]spotifyArtistCache)
	}
	ttl := s.artistCacheTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now()
	s.artistCache[id] = spotifyArtistCache{observedAt: now, expiresAt: now.Add(ttl), artist: artist}
	s.evictArtistCacheLocked()
}

func (s *Spotify) evictArtistCacheLocked() {
	maxEntries := s.maxArtistCache
	if maxEntries <= 0 {
		maxEntries = 512
	}
	for len(s.artistCache) > maxEntries {
		oldestID := ""
		var oldest time.Time
		for id, entry := range s.artistCache {
			if oldestID == "" || entry.observedAt.Before(oldest) {
				oldestID, oldest = id, entry.observedAt
			}
		}
		delete(s.artistCache, oldestID)
	}
}

func Enrich(results []ArtistResult, spotify []SpotifyArtist) {
	for i := range results {
		for _, candidate := range spotify {
			if strings.EqualFold(strings.TrimSpace(results[i].Name), strings.TrimSpace(candidate.Name)) {
				results[i].SpotifyID = candidate.ID
				results[i].SpotifyURL = candidate.URL
				results[i].SpotifyImageURL = candidate.ImageURL
				break
			}
		}
	}
}

func SpotifyID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "spotify:artist:") {
		value = strings.TrimPrefix(value, "spotify:artist:")
	} else if parsed, err := url.Parse(value); err == nil && strings.Contains(parsed.Host, "spotify.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 2 && parts[0] == "artist" {
			value = parts[1]
		}
	}
	if len(value) != 22 {
		return "", false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return "", false
		}
	}
	return value, true
}

func (s *Spotify) ArtistReleases(ctx context.Context, artistID string) ([]store.Release, error) {
	return s.ArtistReleasesSince(ctx, artistID, "")
}

func (s *Spotify) ArtistReleasesSince(ctx context.Context, artistID, since string) ([]store.Release, error) {
	if s == nil {
		return nil, errors.New("Spotify is not configured")
	}
	if _, ok := SpotifyID(artistID); !ok {
		return nil, errors.New("invalid Spotify artist ID")
	}
	if cached, ok := s.cachedReleases(artistID, since); ok {
		return cached, nil
	}
	// Spotify permits up to 50 items per artist-albums page. Using the largest
	// page keeps local-history walks cheap and reduces pressure on the rolling
	// API quota without changing the result set.
	const pageSize = 50
	var result []store.Release
	seen := make(map[string]bool)
	for offset := 0; offset < 1000; offset += pageSize {
		endpoint := fmt.Sprintf(
			"%s/v1/artists/%s/albums?include_groups=album%%2Csingle%%2Ccompilation&market=%s&limit=%d&offset=%d",
			s.apiURL, url.PathEscape(artistID), url.QueryEscape(s.market), pageSize, offset,
		)
		var page struct {
			Total int `json:"total"`
			Items []struct {
				ID            string            `json:"id"`
				Name          string            `json:"name"`
				AlbumType     string            `json:"album_type"`
				AlbumGroup    string            `json:"album_group"`
				TotalTracks   int               `json:"total_tracks"`
				ReleaseDate   string            `json:"release_date"`
				DatePrecision string            `json:"release_date_precision"`
				ExternalURLs  map[string]string `json:"external_urls"`
				Images        []struct {
					URL   string `json:"url"`
					Width int    `json:"width"`
				} `json:"images"`
			} `json:"items"`
		}
		if err := s.getAPIJSON(ctx, "Spotify artist albums", endpoint, &page); err != nil {
			return nil, err
		}
		oldest := ""
		for _, item := range page.Items {
			if item.ReleaseDate != "" && (oldest == "" || item.ReleaseDate < oldest) {
				oldest = item.ReleaseDate
			}
			if item.ID == "" || seen[item.ID] {
				continue
			}
			primaryType, secondaryTypes, eligible := spotifyReleaseType(
				item.AlbumType, item.AlbumGroup, item.Name, item.TotalTracks,
			)
			if !eligible {
				continue
			}
			seen[item.ID] = true
			result = append(result, store.Release{
				MBID: "spotify:" + item.ID, Title: item.Name, PrimaryType: primaryType,
				SecondaryTypes: secondaryTypes, FirstReleaseDate: item.ReleaseDate,
				DatePrecision: spotifyDatePrecision(item.DatePrecision, item.ReleaseDate),
				SpotifyID:     item.ID, SpotifyURL: item.ExternalURLs["spotify"],
				SpotifyImageURL: spotifyReleaseImage(item.Images), Source: "spotify",
			})
		}
		if since == "" || oldest == "" || oldest <= since || len(page.Items) == 0 || offset+len(page.Items) >= page.Total {
			break
		}
	}
	s.cacheReleases(artistID, result)
	return result, nil
}

func (s *Spotify) cachedReleases(artistID, since string) ([]store.Release, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached, ok := s.releaseCache[artistID]
	if !ok || time.Since(cached.observedAt) >= s.cacheTTL || (since != "" && (cached.oldestDate == "" || cached.oldestDate > since)) {
		return nil, false
	}
	return append([]store.Release(nil), cached.releases...), true
}

func (s *Spotify) cacheReleases(artistID string, releases []store.Release) {
	oldest := ""
	for _, release := range releases {
		if release.FirstReleaseDate != "" && (oldest == "" || release.FirstReleaseDate < oldest) {
			oldest = release.FirstReleaseDate
		}
	}
	s.cacheMu.Lock()
	s.releaseCache[artistID] = spotifyReleaseCache{observedAt: time.Now(), oldestDate: oldest, releases: append([]store.Release(nil), releases...)}
	s.cacheMu.Unlock()
}

func (s *Spotify) getAPIJSON(ctx context.Context, operation, endpoint string, target any) error {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	var token string
	for attempt := 0; attempt < 3; attempt++ {
		if err := s.waitUntilReady(ctx, operation); err != nil {
			return err
		}
		if token == "" {
			var err error
			token, err = s.accessToken(ctx)
			if err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.client.Do(req)
		s.lastRequest = time.Now()
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
		if resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%s returned %s", operation, resp.Status)
		}
		rateErr := spotifyRateLimitError(operation, resp, body, attempt, s.retryBase)
		s.blockedUntil = time.Now().Add(rateErr.RetryAfter)
		s.blockedReason = rateErr.Reason
		s.blockedQuota = rateErr.QuotaExceeded
		if rateErr.QuotaExceeded || rateErr.RetryAfter > s.maxInlineRetry || attempt == 2 {
			return rateErr
		}
	}
	return errors.New(operation + " failed after retries")
}

func (s *Spotify) waitUntilReady(ctx context.Context, operation string) error {
	now := time.Now()
	if remaining := time.Until(s.blockedUntil); remaining > 0 {
		if remaining > s.maxInlineRetry || s.blockedQuota {
			return &SpotifyRateLimitError{
				Operation: operation, Status: http.StatusTooManyRequests, Reason: s.blockedReason,
				RetryAfter: remaining, QuotaExceeded: s.blockedQuota, AlreadyBlocked: true,
			}
		}
		if err := s.wait(ctx, remaining); err != nil {
			return err
		}
		s.blockedUntil, s.blockedReason, s.blockedQuota = time.Time{}, "", false
		now = time.Now()
	}
	if remaining := s.requestInterval - now.Sub(s.lastRequest); remaining > 0 {
		return s.wait(ctx, remaining)
	}
	return nil
}

func spotifyRateLimitError(
	operation string, response *http.Response, body []byte, attempt int, retryBase time.Duration,
) *SpotifyRateLimitError {
	var payload struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	reason := strings.TrimSpace(payload.Error.Reason)
	retryAfter := time.Duration(0)
	hasRetryAfter := false
	if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
		hasRetryAfter = true
	}
	quotaExceeded := strings.EqualFold(reason, "QUOTA_EXCEEDED")
	if quotaExceeded && !hasRetryAfter {
		retryAfter = 30 * time.Minute
	} else if retryAfter <= 0 {
		retryAfter = time.Duration(1<<attempt) * retryBase
	}
	return &SpotifyRateLimitError{
		Operation: operation, Status: response.StatusCode, Reason: reason, RetryAfter: retryAfter,
		QuotaExceeded: quotaExceeded,
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func spotifyReleaseType(albumType, albumGroup, title string, totalTracks int) (string, []string, bool) {
	kind := strings.ToLower(strings.TrimSpace(albumType))
	group := strings.ToLower(strings.TrimSpace(albumGroup))
	switch {
	case kind == "album" && group == "compilation", kind == "compilation":
		return "Album", []string{"Compilation"}, true
	case kind == "album":
		return "Album", nil, true
	case kind == "single" && (totalTracks >= 4 || strings.Contains(strings.ToLower(title), " ep")):
		return "EP", nil, true
	case kind == "single":
		return "Single", nil, true
	default:
		return "", nil, false
	}
}

func spotifyDatePrecision(precision, value string) int {
	switch strings.ToLower(precision) {
	case "year":
		return 1
	case "month":
		return 2
	case "day":
		return 3
	}
	switch len(value) {
	case 4:
		return 1
	case 7:
		return 2
	case 10:
		return 3
	default:
		return 0
	}
}

func spotifyReleaseImage(images []struct {
	URL   string `json:"url"`
	Width int    `json:"width"`
}) string {
	if len(images) == 0 {
		return ""
	}
	selected := images[0].URL
	for _, image := range images {
		if image.Width >= 250 {
			selected = image.URL
		}
	}
	return selected
}

func extractMBID(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && strings.Contains(parsed.Host, "musicbrainz.org") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 2 && parts[0] == "artist" {
			return parts[1]
		}
	}
	return value
}

func validMBID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
