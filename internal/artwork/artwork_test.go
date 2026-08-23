package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testMBID = "6e335887-60ba-38f0-95af-fae7774336bf"

func jpegImage(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	source.Set(0, 0, color.RGBA{R: 23, G: 107, B: 77, A: 255})
	if err := jpeg.Encode(&buffer, source, nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestCacheFollowsRedirectAndRefreshes(t *testing.T) {
	imageData := jpegImage(t)
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/release-group/"+testMBID+"/front-250", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/image.jpg", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/image.jpg", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(imageData)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	cache, err := NewCache(t.TempDir(), WithBaseURL(server.URL), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}

	first := cache.Get(context.Background(), testMBID)
	second := cache.Get(context.Background(), testMBID)
	if first.Status != "fetched" || second.Status != "cache" || !bytes.Equal(second.Data, imageData) {
		t.Fatalf("unexpected assets: first=%s second=%s", first.Status, second.Status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2 including redirect", got)
	}

	now = now.Add(31 * 24 * time.Hour)
	refreshed := cache.Get(context.Background(), testMBID)
	if refreshed.Status != "fetched" || requests.Load() != 4 {
		t.Fatalf("stale cache did not refresh: status=%s requests=%d", refreshed.Status, requests.Load())
	}
}

func TestCacheRejectsUnapprovedAndDowngradedRedirects(t *testing.T) {
	for name, location := range map[string]string{
		"unapproved host": "https://example.invalid/art.jpg",
		"https downgrade": "http://coverartarchive.org/art.jpg",
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Redirect(w, r, location, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			cache, err := NewCache(t.TempDir(), WithBaseURL(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			asset := cache.Get(context.Background(), testMBID)
			if asset.Status != "upstream-error" || requests.Load() != 1 {
				t.Fatalf("redirect was not rejected: status=%q requests=%d", asset.Status, requests.Load())
			}
		})
	}
}

func TestCacheRejectsPrivateResolvedArtworkAddressBeforeDial(t *testing.T) {
	var dialCalled atomic.Bool
	cache, err := NewCache(t.TempDir(),
		WithHTTPClient(&http.Client{Transport: &http.Transport{}}),
		WithResolver(func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}),
		WithDialer(func(context.Context, string, string) (net.Conn, error) {
			dialCalled.Store(true)
			return nil, errors.New("dial should not be called")
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	asset := cache.Get(context.Background(), testMBID)
	if asset.Status != "upstream-error" || dialCalled.Load() {
		t.Fatalf("private artwork address was dialed or not rejected: status=%q dialed=%v", asset.Status, dialCalled.Load())
	}
}

func TestArtworkBlocksMappedAndReservedResolvedAddresses(t *testing.T) {
	tests := []string{
		"::ffff:127.0.0.1",       // IPv4-mapped loopback
		"100.64.0.1",             // shared address space
		"192.0.2.1",              // documentation IPv4
		"2001:db8::1",            // documentation IPv6
		"2001:0000:4136:e378::1", // Teredo transition
		"2002:c000:0201::1",      // 6to4 transition
		"64:ff9b::c000:0201",     // NAT64 translation prefix
	}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			var dialCalled atomic.Bool
			cache, err := NewCache(t.TempDir(),
				WithHTTPClient(&http.Client{Transport: &http.Transport{}}),
				WithResolver(func(context.Context, string, string) ([]net.IP, error) {
					return []net.IP{net.ParseIP(value)}, nil
				}),
				WithDialer(func(context.Context, string, string) (net.Conn, error) {
					dialCalled.Store(true)
					return nil, errors.New("dial should not be called")
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			asset := cache.Get(context.Background(), testMBID)
			if asset.Status != "upstream-error" || dialCalled.Load() {
				t.Fatalf("reserved artwork address %q was dialed or not rejected: status=%q dialed=%v", value, asset.Status, dialCalled.Load())
			}
		})
	}
}

func TestCacheAcceptsCustomHTTPClient(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	cache, err := NewCache(t.TempDir(), WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	if cache.client != client {
		t.Fatal("custom artwork HTTP client was not retained")
	}
}

func TestMissingArtIsNegativelyCached(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	defer server.Close()
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	cache, _ := NewCache(t.TempDir(), WithBaseURL(server.URL), WithClock(func() time.Time { return now }))

	if got := cache.Get(context.Background(), testMBID); got.Status != "missing" {
		t.Fatalf("first status = %s", got.Status)
	}
	if got := cache.Get(context.Background(), testMBID); got.Status != "missing" {
		t.Fatalf("cached status = %s", got.Status)
	}
	if requests.Load() != 1 {
		t.Fatalf("negative cache made %d requests", requests.Load())
	}
	now = now.Add(25 * time.Hour)
	cache.Get(context.Background(), testMBID)
	if requests.Load() != 2 {
		t.Fatalf("expired negative cache made %d requests", requests.Load())
	}
}

func TestTransientFailuresAreNotNegativelyCached(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cache, _ := NewCache(t.TempDir(), WithBaseURL(server.URL))
	cache.Get(context.Background(), testMBID)
	cache.Get(context.Background(), testMBID)
	if requests.Load() != 2 {
		t.Fatalf("transient response was cached; requests=%d", requests.Load())
	}
}

func TestTransientFailureServesStaleArtwork(t *testing.T) {
	imageData := jpegImage(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(imageData)
			return
		}
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	cache, err := NewCache(t.TempDir(), WithBaseURL(server.URL), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if first := cache.Get(context.Background(), testMBID); first.Status != "fetched" {
		t.Fatalf("initial artwork status=%q", first.Status)
	}
	now = now.Add(31 * 24 * time.Hour)
	stale := cache.Get(context.Background(), testMBID)
	if stale.Status != "stale" || !bytes.Equal(stale.Data, imageData) || stale.MaxAge != time.Hour {
		t.Fatalf("stale artwork=%#v", stale)
	}
}

func TestWriteAtomicRejectsEmptyDataWithoutReplacingExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cover.img")
	original := []byte("complete artwork")
	if err := writeAtomic(path, original); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, nil); err == nil {
		t.Fatal("empty artwork write unexpectedly succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("existing artwork changed after failed write: %q", got)
	}
}

func TestAllowedImageTypes(t *testing.T) {
	for _, value := range []string{"image/jpeg", "image/png", "image/webp; charset=binary", "image/gif"} {
		if !allowedImageType(value) {
			t.Fatalf("allowed image type %q rejected", value)
		}
	}
	for _, value := range []string{"", "text/plain", "image/svg+xml"} {
		if allowedImageType(value) {
			t.Fatalf("unsupported image type %q accepted", value)
		}
	}
}

func TestInvalidAndOversizedResponsesUsePlaceholder(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxImageBytes+1))
	}))
	defer server.Close()
	cache, _ := NewCache(t.TempDir(), WithBaseURL(server.URL))

	if got := cache.Get(context.Background(), "../../etc/passwd"); got.Status != "invalid" {
		t.Fatalf("invalid MBID status = %s", got.Status)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid MBID reached upstream")
	}
	if got := cache.Get(context.Background(), testMBID); got.Status != "invalid" || got.ContentType != "image/svg+xml" {
		t.Fatalf("oversized response = %#v", got)
	}
}

func TestNonImageResponseUsesPlaceholder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer server.Close()
	cache, _ := NewCache(t.TempDir(), WithBaseURL(server.URL))
	if got := cache.Get(context.Background(), testMBID); got.Status != "invalid" || got.ContentType != "image/svg+xml" {
		t.Fatalf("non-image response = %#v", got)
	}
}

func TestConcurrentRequestsAreCoalesced(t *testing.T) {
	imageData := jpegImage(t)
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write(imageData)
	}))
	defer server.Close()
	cache, _ := NewCache(t.TempDir(), WithBaseURL(server.URL))

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			cache.Get(context.Background(), testMBID)
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()
	if requests.Load() != 1 {
		t.Fatalf("coalesced requests = %d, want 1", requests.Load())
	}
}

func TestPruneRemovesStaleAndBoundsCache(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cache, err := NewCache(root, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	oldImage := filepath.Join(root, "old.img")
	oldMissing := filepath.Join(root, "old.missing")
	freshOne := filepath.Join(root, "fresh-one.img")
	freshTwo := filepath.Join(root, "fresh-two.img")
	for path, data := range map[string][]byte{
		oldImage:   []byte("old-image"),
		oldMissing: []byte{},
		freshOne:   []byte("fresh-one"),
		freshTwo:   []byte("fresh-two"),
	} {
		if err := os.WriteFile(path, data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldImage, now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldMissing, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshOne, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshTwo, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	stats, err := cache.Prune(context.Background(), int64(len("fresh-one")), 2)
	if err != nil {
		t.Fatal(err)
	}
	if stats.StaleFiles != 2 || stats.RemovedFiles != 3 {
		t.Fatalf("prune stats=%#v, want two stale and three total removals", stats)
	}
	if _, err := os.Stat(freshTwo); err != nil {
		t.Fatalf("newest cache file was removed: %v", err)
	}
	if _, err := os.Stat(oldImage); !os.IsNotExist(err) {
		t.Fatalf("stale image still exists, err=%v", err)
	}
}

func TestPruneHonorsContext(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Prune(ctx, 1, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("prune error=%v, want context canceled", err)
	}
}

func TestCancelledLeaderDoesNotPoisonOtherWaiters(t *testing.T) {
	// The shared fetch used to run on the first caller's request context, so one
	// browser navigating away handed every other waiter a cancellation
	// placeholder - which their browser then cached - even though the upstream
	// was reachable. The artists grid issues many of these at once.
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 jpeg body"))
	}))
	defer server.Close()

	cache, err := NewCache(t.TempDir(), WithBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	const mbid = testMBID

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan Asset, 1)
	go func() { leaderDone <- cache.Get(leaderCtx, mbid) }()

	// Let the leader take the in-flight slot before the follower arrives.
	for requests.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	followerDone := make(chan Asset, 1)
	go func() { followerDone <- cache.Get(context.Background(), mbid) }()
	time.Sleep(20 * time.Millisecond)

	// The leader gives up, as a closed tab would.
	cancelLeader()
	if asset := <-leaderDone; asset.Status != "cancelled" {
		t.Fatalf("the cancelled leader should get its own cancellation: %#v", asset)
	}

	close(release)
	asset := <-followerDone
	// The leader's cancellation surfaced to the follower as an upstream error
	// carrying the SVG placeholder, not as "cancelled", so assert on what a
	// working fetch actually produces rather than on one failure spelling.
	if asset.Status != "fetched" {
		t.Fatalf("the follower inherited the leader's cancellation: status=%q, want fetched", asset.Status)
	}
	if !bytes.HasPrefix(asset.Data, []byte("\xff\xd8\xff")) {
		t.Fatalf("the follower did not receive the real image: %q", asset.Data)
	}
	if requests.Load() != 1 {
		t.Fatalf("upstream requests=%d, want the fetch to stay coalesced", requests.Load())
	}
}
