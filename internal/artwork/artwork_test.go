package artwork

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
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
