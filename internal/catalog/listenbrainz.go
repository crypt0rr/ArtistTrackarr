package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ListenBrainzArtistStats contains only public aggregate statistics. No user
// identity, token, or private listening history is retained.
type ListenBrainzArtistStats struct {
	MBID             string `json:"artist_mbid"`
	TotalListenCount int64  `json:"total_listen_count"`
	TotalUserCount   int64  `json:"total_user_count"`
}

type ListenBrainz struct {
	client       *http.Client
	baseURL      string
	requestMu    sync.Mutex
	lastRequest  time.Time
	requestEvery time.Duration
	cacheMu      sync.Mutex
	cache        map[string]listenBrainzCacheEntry
	cacheTTL     time.Duration
	inflight     map[string]*listenBrainzCall
}

type listenBrainzCacheEntry struct {
	expiresAt time.Time
	stats     ListenBrainzArtistStats
}

type listenBrainzCall struct {
	done  chan struct{}
	stats map[string]ListenBrainzArtistStats
	err   error
}

func NewListenBrainz() *ListenBrainz {
	return &ListenBrainz{
		client:       &http.Client{Timeout: 20 * time.Second},
		baseURL:      "https://api.listenbrainz.org",
		requestEvery: time.Second,
		cache:        make(map[string]listenBrainzCacheEntry),
		cacheTTL:     24 * time.Hour,
		inflight:     make(map[string]*listenBrainzCall),
	}
}

func (l *ListenBrainz) wait(ctx context.Context) error {
	l.requestMu.Lock()
	defer l.requestMu.Unlock()
	if remaining := l.requestEvery - time.Since(l.lastRequest); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	l.lastRequest = time.Now()
	return nil
}

func (l *ListenBrainz) Popularity(ctx context.Context, mbids []string) (map[string]ListenBrainzArtistStats, error) {
	if l == nil {
		return nil, errors.New("ListenBrainz is unavailable")
	}
	unique := make([]string, 0, len(mbids))
	seen := make(map[string]bool, len(mbids))
	result := make(map[string]ListenBrainzArtistStats, len(mbids))
	for _, raw := range mbids {
		mbid := strings.ToLower(strings.TrimSpace(raw))
		if mbid == "" || seen[mbid] {
			continue
		}
		seen[mbid] = true
		if cached, ok := l.cached(mbid); ok {
			result[mbid] = cached
			continue
		}
		unique = append(unique, mbid)
	}
	if len(unique) == 0 {
		return result, nil
	}
	key := strings.Join(unique, ",")
	l.cacheMu.Lock()
	if call, ok := l.inflight[key]; ok {
		l.cacheMu.Unlock()
		select {
		case <-call.done:
			return mergeListenBrainzStats(result, call.stats), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &listenBrainzCall{done: make(chan struct{})}
	l.inflight[key] = call
	l.cacheMu.Unlock()
	fetched, err := l.fetchPopularity(ctx, unique, seen, result)
	l.cacheMu.Lock()
	delete(l.inflight, key)
	call.stats, call.err = fetched, err
	close(call.done)
	l.cacheMu.Unlock()
	return fetched, err
}

func (l *ListenBrainz) fetchPopularity(ctx context.Context, unique []string, seen map[string]bool, result map[string]ListenBrainzArtistStats) (map[string]ListenBrainzArtistStats, error) {
	if err := l.wait(ctx); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string][]string{"artist_mbids": unique})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/1/popularity/artist", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request ListenBrainz popularity: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read ListenBrainz response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListenBrainz popularity returned %s", resp.Status)
	}
	var payload []ListenBrainzArtistStats
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode ListenBrainz response: %w", err)
	}
	for _, stats := range payload {
		stats.MBID = strings.ToLower(strings.TrimSpace(stats.MBID))
		if stats.MBID == "" || !seen[stats.MBID] {
			continue
		}
		result[stats.MBID] = stats
		l.cacheMu.Lock()
		l.cache[stats.MBID] = listenBrainzCacheEntry{expiresAt: time.Now().Add(l.cacheTTL), stats: stats}
		l.cacheMu.Unlock()
	}
	return result, nil
}

func mergeListenBrainzStats(base, extra map[string]ListenBrainzArtistStats) map[string]ListenBrainzArtistStats {
	merged := make(map[string]ListenBrainzArtistStats, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

func (l *ListenBrainz) cached(mbid string) (ListenBrainzArtistStats, bool) {
	l.cacheMu.Lock()
	defer l.cacheMu.Unlock()
	entry, ok := l.cache[mbid]
	if !ok || !time.Now().Before(entry.expiresAt) {
		if ok {
			delete(l.cache, mbid)
		}
		return ListenBrainzArtistStats{}, false
	}
	return entry.stats, true
}
