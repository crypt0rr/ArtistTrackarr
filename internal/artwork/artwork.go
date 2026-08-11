package artwork

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/version"
)

const (
	maxImageBytes = 5 << 20
	successTTL    = 30 * 24 * time.Hour
	missingTTL    = 24 * time.Hour
)

var placeholder = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 250 250" role="img" aria-label="No cover art"><rect width="250" height="250" fill="#dff1e8"/><circle cx="125" cy="125" r="62" fill="#176b4d"/><circle cx="125" cy="125" r="18" fill="#dff1e8"/><path d="M125 63v44l39-22a62 62 0 0 0-39-22Z" fill="#fffdf8" opacity=".8"/></svg>`)

type Asset struct {
	Data        []byte
	ContentType string
	Status      string
	MaxAge      time.Duration
}

const (
	// DefaultMaxCacheBytes and DefaultMaxCacheFiles keep the persistent
	// artwork cache bounded for household deployments without adding another
	// required runtime setting.
	DefaultMaxCacheBytes int64 = 1 << 30
	DefaultMaxCacheFiles       = 25_000
)

type PruneStats struct {
	RemovedFiles int
	RemovedBytes int64
	StaleFiles   int
}

type Provider interface {
	Get(context.Context, string) Asset
}

type Option func(*Cache)

func WithBaseURL(value string) Option {
	return func(c *Cache) { c.baseURL = strings.TrimRight(value, "/") }
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Cache) { c.client = client }
}

func WithClock(clock func() time.Time) Option {
	return func(c *Cache) { c.now = clock }
}

type call struct {
	done  chan struct{}
	asset Asset
}

type Cache struct {
	root                 string
	baseURL              string
	client               *http.Client
	now                  func() time.Time
	semaphore            chan struct{}
	mu                   sync.Mutex
	inflight             map[string]*call
	pruneMu              sync.Mutex
	circuitFailureStreak int
	circuitUntil         time.Time
}

func NewCache(root string, options ...Option) (*Cache, error) {
	cache := &Cache{
		root:      root,
		baseURL:   "https://coverartarchive.org",
		client:    &http.Client{Timeout: 20 * time.Second},
		now:       time.Now,
		semaphore: make(chan struct{}, 4),
		inflight:  make(map[string]*call),
	}
	for _, option := range options {
		option(cache)
	}
	if cache.client == nil || cache.now == nil {
		return nil, errors.New("artwork cache requires an HTTP client and clock")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create artwork cache: %w", err)
	}
	return cache, nil
}

func (c *Cache) Get(ctx context.Context, mbid string) Asset {
	if !validMBID(mbid) {
		return placeholderAsset("invalid")
	}
	if asset, fresh := c.cached(mbid); fresh {
		return asset
	}

	c.mu.Lock()
	if existing, ok := c.inflight[mbid]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return placeholderAsset("cancelled")
		case <-existing.done:
			return existing.asset
		}
	}
	pending := &call{done: make(chan struct{})}
	c.inflight[mbid] = pending
	c.mu.Unlock()

	pending.asset = c.refresh(ctx, mbid)
	c.mu.Lock()
	delete(c.inflight, mbid)
	close(pending.done)
	c.mu.Unlock()
	return pending.asset
}

func (c *Cache) cached(mbid string) (Asset, bool) {
	imagePath, missingPath := c.paths(mbid)
	if info, err := os.Stat(imagePath); err == nil {
		data, readErr := os.ReadFile(imagePath)
		if age := c.now().Sub(info.ModTime()); readErr == nil && age < successTTL {
			asset := imageAsset(data, "cache")
			asset.MaxAge = successTTL - max(age, 0)
			return asset, true
		}
	}
	if info, err := os.Stat(missingPath); err == nil && c.now().Sub(info.ModTime()) < missingTTL {
		return placeholderAsset("missing"), true
	}
	return Asset{}, false
}

func (c *Cache) refresh(ctx context.Context, mbid string) Asset {
	imagePath, missingPath := c.paths(mbid)
	stale, _ := os.ReadFile(imagePath)
	c.mu.Lock()
	if c.circuitUntil.After(c.now()) {
		c.mu.Unlock()
		return fallback(stale, "circuit-open")
	}
	c.mu.Unlock()
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return fallback(stale, "cancelled")
	}

	endpoint := c.baseURL + "/release-group/" + url.PathEscape(mbid) + "/front-250"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fallback(stale, "error")
	}
	request.Header.Set("User-Agent", version.UserAgent)
	response, err := c.client.Do(request)
	if err != nil {
		c.recordTransientFailure()
		return fallback(stale, "upstream-error")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusNotFound {
		c.resetCircuit()
		_ = os.Remove(imagePath)
		_ = c.writeCacheFile(missingPath, nil)
		return placeholderAsset("missing")
	}
	if response.StatusCode != http.StatusOK {
		c.recordTransientFailureAfter(response)
		return fallback(stale, "upstream-error")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxImageBytes {
		c.recordTransientFailure()
		return fallback(stale, "invalid")
	}
	contentType := http.DetectContentType(data)
	if !allowedImageType(contentType) {
		c.recordTransientFailure()
		return fallback(stale, "invalid")
	}
	if err := c.writeCacheFile(imagePath, data); err != nil {
		c.recordTransientFailure()
		return fallback(data, "uncached")
	}
	c.resetCircuit()
	_ = os.Remove(missingPath)
	return Asset{Data: data, ContentType: contentType, Status: "fetched", MaxAge: successTTL}
}

func (c *Cache) resetCircuit() {
	c.mu.Lock()
	c.circuitFailureStreak = 0
	c.circuitUntil = time.Time{}
	c.mu.Unlock()
}

func (c *Cache) recordTransientFailure() {
	c.recordTransientFailureAfter(nil)
}

func (c *Cache) recordTransientFailureAfter(response *http.Response) {
	delay := time.Minute
	if response != nil {
		if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
	}
	c.mu.Lock()
	c.circuitFailureStreak++
	backoff := time.Minute * time.Duration(1<<min(c.circuitFailureStreak-1, 5))
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	if delay > backoff {
		backoff = delay
	}
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	// A single upstream blip should still be retried on the next request;
	// open the circuit only after a short consecutive-failure streak.
	if c.circuitFailureStreak >= 3 {
		c.circuitUntil = c.now().Add(backoff)
	}
	c.mu.Unlock()
}

// Prune removes stale entries and then oldest entries until both cache limits
// are satisfied. Files are written atomically by refresh, so a directory
// snapshot is safe to inspect while a fetch is in progress. Temporary files
// are deliberately ignored and are cleaned up by their writer.
func (c *Cache) Prune(ctx context.Context, maxBytes int64, maxFiles int) (PruneStats, error) {
	select {
	case <-ctx.Done():
		return PruneStats{}, ctx.Err()
	default:
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCacheBytes
	}
	if maxFiles <= 0 {
		maxFiles = DefaultMaxCacheFiles
	}
	c.pruneMu.Lock()
	defer c.pruneMu.Unlock()
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PruneStats{}, nil
		}
		return PruneStats{}, err
	}
	type cacheFile struct {
		path    string
		modTime time.Time
		size    int64
		stale   bool
	}
	files := make([]cacheFile, 0, len(entries))
	now := c.now()
	var totalBytes int64
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".img") && !strings.HasSuffix(entry.Name(), ".missing")) {
			continue
		}
		select {
		case <-ctx.Done():
			return PruneStats{}, ctx.Err()
		default:
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			return PruneStats{}, infoErr
		}
		ttl := successTTL
		if strings.HasSuffix(entry.Name(), ".missing") {
			ttl = missingTTL
		}
		age := now.Sub(info.ModTime())
		stale := age >= ttl
		files = append(files, cacheFile{path: filepath.Join(c.root, entry.Name()), modTime: info.ModTime(), size: info.Size(), stale: stale})
		totalBytes += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	stats := PruneStats{}
	remove := func(index int) error {
		file := files[index]
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		stats.RemovedFiles++
		stats.RemovedBytes += file.size
		if file.stale {
			stats.StaleFiles++
		}
		totalBytes -= file.size
		files = append(files[:index], files[index+1:]...)
		return nil
	}
	for index := 0; index < len(files); {
		if !files[index].stale {
			index++
			continue
		}
		if err := remove(index); err != nil {
			return stats, err
		}
	}
	for len(files) > maxFiles || totalBytes > maxBytes {
		if len(files) == 0 {
			break
		}
		if err := remove(0); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (c *Cache) paths(mbid string) (string, string) {
	return filepath.Join(c.root, mbid+".img"), filepath.Join(c.root, mbid+".missing")
}

func imageAsset(data []byte, status string) Asset {
	return Asset{Data: data, ContentType: http.DetectContentType(data), Status: status, MaxAge: successTTL}
}

func placeholderAsset(status string) Asset {
	return Asset{Data: placeholder, ContentType: "image/svg+xml", Status: status, MaxAge: 5 * time.Minute}
}

func fallback(stale []byte, status string) Asset {
	if len(stale) > 0 {
		asset := imageAsset(stale, "stale")
		asset.MaxAge = time.Hour
		return asset
	}
	return placeholderAsset(status)
}

func allowedImageType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0])) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func (c *Cache) writeCacheFile(path string, data []byte) error {
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	now := c.now()
	return os.Chtimes(path, now, now)
}

func writeAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".artwork-*")
	if err != nil {
		return err
	}
	tempName := file.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := file.Chmod(0o640); err == nil {
		_, err = file.Write(data)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func validMBID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
