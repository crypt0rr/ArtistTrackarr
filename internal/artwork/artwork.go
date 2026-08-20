package artwork

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	return func(c *Cache) {
		c.baseURL = strings.TrimRight(value, "/")
		c.allowLoopback = artworkBaseURLIsLoopback(value)
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Cache) { c.client = client }
}

func WithClock(clock func() time.Time) Option {
	return func(c *Cache) { c.now = clock }
}

// WithResolver and WithDialer are narrow seams for deterministic transport
// tests. Production uses the system resolver and a bounded net.Dialer.
func WithResolver(lookup func(context.Context, string, string) ([]net.IP, error)) Option {
	return func(c *Cache) { c.lookupIP = lookup }
}

func WithDialer(dial func(context.Context, string, string) (net.Conn, error)) Option {
	return func(c *Cache) { c.dial = dial }
}

type call struct {
	done  chan struct{}
	asset Asset
}

type Cache struct {
	root                 string
	baseURL              string
	client               *http.Client
	requestClient        *http.Client
	now                  func() time.Time
	lookupIP             func(context.Context, string, string) ([]net.IP, error)
	dial                 func(context.Context, string, string) (net.Conn, error)
	allowLoopback        bool
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
		lookupIP:  net.DefaultResolver.LookupIP,
		dial:      (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
		semaphore: make(chan struct{}, 4),
		inflight:  make(map[string]*call),
	}
	for _, option := range options {
		option(cache)
	}
	if cache.client == nil || cache.now == nil {
		return nil, errors.New("artwork cache requires an HTTP client and clock")
	}
	if cache.lookupIP == nil {
		cache.lookupIP = net.DefaultResolver.LookupIP
	}
	if cache.dial == nil {
		cache.dial = (&net.Dialer{Timeout: 20 * time.Second}).DialContext
	}
	cache.requestClient = cache.secureClient()
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
	parsedEndpoint, parseErr := url.Parse(endpoint)
	if parseErr != nil || validateArtworkTarget(parsedEndpoint, c.allowLoopback) != nil {
		return fallback(stale, "blocked")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fallback(stale, "error")
	}
	request.Header.Set("User-Agent", version.UserAgent)
	response, err := c.requestClient.Do(request)
	if err != nil {
		c.recordTransientFailure()
		return fallback(stale, "upstream-error")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusNotFound {
		c.resetCircuit()
		_ = os.Remove(imagePath)
		// Keep a non-empty marker so the atomic artwork writer can reject empty
		// image payloads without disabling the 24-hour negative cache.
		_ = c.writeCacheFile(missingPath, []byte("missing\n"))
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
	if len(data) == 0 {
		return errors.New("artwork cache data is empty")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".artwork-*")
	if err != nil {
		return err
	}
	tempName := file.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		return err
	}
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
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

func (c *Cache) secureClient() *http.Client {
	client := *c.client
	if client.Timeout <= 0 {
		client.Timeout = 20 * time.Second
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = safeTransport(base, c.allowLoopback, c.lookupIP, c.dial)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("artwork redirect limit exceeded")
		}
		if err := validateArtworkTarget(req.URL, c.allowLoopback); err != nil {
			return err
		}
		return nil
	}
	return &client
}

func safeTransport(base http.RoundTripper, allowLoopback bool,
	lookup func(context.Context, string, string) ([]net.IP, error),
	dial func(context.Context, string, string) (net.Conn, error),
) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	transport = transport.Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("artwork upstream address is invalid")
		}
		if isBlockedArtworkHost(host, allowLoopback) {
			return nil, errors.New("artwork upstream resolved to a local or private network")
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		ips, err := lookup(lookupCtx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("artwork upstream host could not be resolved")
		}
		var lastErr error
		for _, ip := range ips {
			if isBlockedArtworkIP(ip, allowLoopback) {
				lastErr = errors.New("artwork upstream resolved to a local or private network")
				continue
			}
			conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("artwork upstream could not be reached")
		}
		return nil, lastErr
	}
	return transport
}

func validateArtworkTarget(target *url.URL, allowLoopback bool) error {
	if target == nil || target.Hostname() == "" {
		return errors.New("artwork upstream URL is invalid")
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target.Hostname()), "."))
	allowLoopbackHTTP := allowLoopback && target.Scheme == "http" && isLoopbackHost(host)
	if target.Scheme != "https" && !allowLoopbackHTTP {
		return errors.New("artwork upstream must use HTTPS")
	}
	if !allowedArtworkHost(host, allowLoopback) {
		return errors.New("artwork upstream host is not approved")
	}
	if port := target.Port(); port != "" && port != "443" {
		if !allowLoopback || !isLoopbackHost(host) {
			return errors.New("artwork upstream port is not approved")
		}
	}
	return nil
}

func allowedArtworkHost(host string, allowLoopback bool) bool {
	if allowLoopback && isLoopbackHost(host) {
		return true
	}
	for _, domain := range []string{"coverartarchive.org", "archive.org"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isBlockedArtworkHost(host string, allowLoopback bool) bool {
	host = strings.Trim(host, "[]")
	if allowLoopback && isLoopbackHost(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isBlockedArtworkIP(ip, allowLoopback)
	}
	return false
}

func isBlockedArtworkIP(ip net.IP, allowLoopback bool) bool {
	if ip == nil {
		return true
	}
	if allowLoopback && ip.IsLoopback() {
		return false
	}
	// IPv4-mapped IPv6 addresses carry the same routing policy as their
	// four-byte representation. Without normalizing them first, an address
	// such as ::ffff:127.0.0.1 could bypass the checks below on some resolver
	// implementations.
	if v4 := ip.To4(); v4 != nil && len(ip) == net.IPv6len {
		return isBlockedArtworkIP(v4, allowLoopback)
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		artworkReservedNetworksContain(ip)
}

// artworkReservedNetworksContain rejects address space that is not a usable
// public artwork origin even though net.IP may classify it as global unicast.
// Keep this list deliberately aligned with the notification target policy so
// DNS rebinding or transition mechanisms cannot turn an approved CAA hostname
// into a private or non-routable endpoint.
func artworkReservedNetworksContain(ip net.IP) bool {
	for _, cidr := range []string{
		"0.0.0.0/8",       // this-network/reserved addresses
		"100.64.0.0/10",   // RFC 6598 shared address space
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved/future use
		"2001:db8::/32",   // IPv6 documentation
		"2001::/32",       // Teredo transition addresses
		"2002::/16",       // 6to4 transition addresses
		"64:ff9b::/96",    // well-known NAT64 prefix
		"64:ff9b:1::/48",  // network-specific NAT64 prefix
	} {
		if _, network, err := net.ParseCIDR(cidr); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func artworkBaseURLIsLoopback(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed != nil && isLoopbackHost(parsed.Hostname())
}

// ValidMBID reports whether value has the canonical MusicBrainz UUID shape.
// Callers that gate access to the Cover Art Archive can use this check before
// performing any persistent lookup or upstream request.
func ValidMBID(value string) bool {
	return validMBID(value)
}
