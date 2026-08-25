package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/version"
)

// ListenBrainzArtistStats contains only public aggregate statistics. No user
// identity, token, or private listening history is retained.
type ListenBrainzArtistStats struct {
	MBID string `json:"artist_mbid"`
	// Pointers because ListenBrainz echoes back every MBID it was asked about and
	// uses JSON null for the counts when it has no data for one - verified live
	// on 2026-08-25. With int64 targets encoding/json leaves the field at zero, so
	// "no data" was indistinguishable from "genuinely zero listeners" and the
	// caller's guard against overwriting known totals could never fire.
	TotalListenCount *int64 `json:"total_listen_count"`
	TotalUserCount   *int64 `json:"total_user_count"`
}

// HasData reports whether ListenBrainz actually returned counts for this artist.
func (s ListenBrainzArtistStats) HasData() bool {
	return s.TotalListenCount != nil || s.TotalUserCount != nil
}

// Counts returns the reported totals, with absent values as zero.
func (s ListenBrainzArtistStats) Counts() (listens, users int64) {
	if s.TotalListenCount != nil {
		listens = *s.TotalListenCount
	}
	if s.TotalUserCount != nil {
		users = *s.TotalUserCount
	}
	return listens, users
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
	maxCache     int
	inflight     map[string]*listenBrainzCall
}

type listenBrainzCacheEntry struct {
	observedAt time.Time
	expiresAt  time.Time
	stats      ListenBrainzArtistStats
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
		maxCache:     2048,
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
			return mergeListenBrainzStats(result, call.stats), coalescedRequestError(ctx, call.err)
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
	body, err := json.Marshal(map[string][]string{"artist_mbids": unique})
	if err != nil {
		return nil, err
	}
	var data []byte
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		data = nil
		retry := false
		delay := time.Duration(1<<attempt) * time.Second
		if err := l.wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/1/popularity/artist", strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", version.UserAgent)
		resp, err := l.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request ListenBrainz popularity: %w", err)
			retry = true
		} else {
			data, err = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			if err != nil {
				lastErr = fmt.Errorf("read ListenBrainz response: %w", err)
				retry = true
			} else if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			} else {
				lastErr = fmt.Errorf("ListenBrainz popularity returned %s", resp.Status)
				if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
					return nil, lastErr
				}
				retry = true
				retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
				if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds > 0 {
					delay = time.Duration(seconds) * time.Second
				} else if retryAt, parseErr := http.ParseTime(retryAfter); parseErr == nil {
					delay = time.Until(retryAt)
					if delay < time.Second {
						delay = time.Second
					}
				}
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
		}
		if retry && attempt < 3 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
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
		l.cache[stats.MBID] = listenBrainzCacheEntry{observedAt: time.Now(), expiresAt: time.Now().Add(l.cacheTTL), stats: stats}
		l.evictCacheLocked()
		l.cacheMu.Unlock()
	}
	return result, nil
}

func (l *ListenBrainz) evictCacheLocked() {
	maxEntries := l.maxCache
	if maxEntries <= 0 {
		maxEntries = 2048
	}
	for len(l.cache) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range l.cache {
			if oldestKey == "" || entry.observedAt.Before(oldest) {
				oldestKey, oldest = key, entry.observedAt
			}
		}
		delete(l.cache, oldestKey)
	}
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
