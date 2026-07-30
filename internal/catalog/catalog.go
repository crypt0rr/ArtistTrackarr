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
	"strings"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

type CatalogProvider interface {
	SearchArtists(context.Context, string, int) ([]ArtistResult, error)
	ResolveArtist(context.Context, string) (ArtistResult, error)
	ArtistReleases(context.Context, string) ([]store.Release, error)
}

type ReleaseNormalizer interface {
	Normalize([]store.Release) []store.Release
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
	limiter   *time.Ticker
	mu        sync.Mutex
	lastCall  time.Time
}

func NewMusicBrainz(contact string) *MusicBrainz {
	return &MusicBrainz{
		client:    &http.Client{Timeout: 20 * time.Second},
		userAgent: fmt.Sprintf("ArtistReleaseTracker/1.0 (%s)", contact),
	}
}

func (m *MusicBrainz) wait(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if remaining := time.Second - time.Since(m.lastCall); remaining > 0 {
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
	for attempt := 0; attempt < 3; attempt++ {
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
			last = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, target); err != nil {
				return fmt.Errorf("decode MusicBrainz response: %w", err)
			}
			return nil
		}
		last = fmt.Errorf("MusicBrainz returned %s", resp.Status)
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			return last
		}
		delay := time.Duration(attempt+1) * 2 * time.Second
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return last
}

func (m *MusicBrainz) SearchArtists(ctx context.Context, query string, limit int) ([]ArtistResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search query is required")
	}
	if limit < 1 || limit > 25 {
		limit = 10
	}
	endpoint := "https://musicbrainz.org/ws/2/artist?fmt=json&limit=" +
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
	endpoint := "https://musicbrainz.org/ws/2/artist/" + url.PathEscape(mbid) + "?fmt=json&inc=aliases"
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

func (m *MusicBrainz) ArtistReleases(ctx context.Context, mbid string) ([]store.Release, error) {
	var all []store.Release
	offset := 0
	for {
		endpoint := fmt.Sprintf("https://musicbrainz.org/ws/2/release-group?fmt=json&artist=%s&type=album%%7Cep&release-group-status=website-default&limit=100&offset=%d",
			url.QueryEscape(mbid), offset)
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
		if release.PrimaryType != "Album" && release.PrimaryType != "EP" {
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
	mu               sync.Mutex
	token            string
	expires          time.Time
}

type SpotifyArtist struct {
	ID, Name, URL, ImageURL string
}

func NewSpotify(id, secret string) *Spotify {
	if id == "" || secret == "" {
		return nil
	}
	return &Spotify{clientID: id, secret: secret, client: &http.Client{Timeout: 15 * time.Second}}
}

func (s *Spotify) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expires.Add(-time.Minute)) {
		return s.token, nil
	}
	form := strings.NewReader("grant_type=client_credentials")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://accounts.spotify.com/api/token", form)
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
	token, err := s.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := "https://api.spotify.com/v1/search?type=artist&limit=10&q=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Spotify search returned %s", resp.Status)
	}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&response); err != nil {
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

func (s *Spotify) Artist(ctx context.Context, id string) (SpotifyArtist, error) {
	if s == nil {
		return SpotifyArtist{}, errors.New("Spotify is not configured")
	}
	token, err := s.accessToken(ctx)
	if err != nil {
		return SpotifyArtist{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.spotify.com/v1/artists/"+url.PathEscape(id), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return SpotifyArtist{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SpotifyArtist{}, fmt.Errorf("Spotify artist lookup returned %s", resp.Status)
	}
	var item struct {
		ID           string            `json:"id"`
		Name         string            `json:"name"`
		ExternalURLs map[string]string `json:"external_urls"`
		Images       []struct {
			URL string `json:"url"`
		} `json:"images"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&item); err != nil {
		return SpotifyArtist{}, err
	}
	result := SpotifyArtist{ID: item.ID, Name: item.Name, URL: item.ExternalURLs["spotify"]}
	if len(item.Images) > 0 {
		result.ImageURL = item.Images[len(item.Images)-1].URL
	}
	return result, nil
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
