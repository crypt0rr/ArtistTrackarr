package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/config"
)

func TestFixedWindowLimiterBoundsRequestsAndExpires(t *testing.T) {
	limiter := newFixedWindowLimiter(2, 20*time.Millisecond)
	if !limiter.Allow("client") || !limiter.Allow("client") || limiter.Allow("client") {
		t.Fatal("fixed-window limiter did not reject the third request")
	}
	time.Sleep(25 * time.Millisecond)
	if !limiter.Allow("client") {
		t.Fatal("fixed-window limiter did not expire")
	}
}

func TestFixedWindowLimiterCapsDistinctKeys(t *testing.T) {
	limiter := newFixedWindowLimiter(1, time.Minute)
	limiter.maxEntries = 2
	if !limiter.Allow("first") || !limiter.Allow("second") || !limiter.Allow("third") {
		t.Fatal("limiter rejected a request while making room for a bounded key set")
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.entries) != 2 {
		t.Fatalf("limiter retained %d entries; want 2", len(limiter.entries))
	}
}

func TestClientIPOnlyTrustsForwardedHeadersFromConfiguredProxy(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: config.Config{TrustProxy: true, TrustedProxyNetworks: []*net.IPNet{proxyNetwork}}}
	request := httptest.NewRequest("GET", "http://example.test", nil)
	request.RemoteAddr = "203.0.113.10:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.20")
	if got := app.clientIP(request); got != "203.0.113.10" {
		t.Fatalf("untrusted peer accepted forwarded address: %q", got)
	}
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.20, 127.0.0.1")
	if got := app.clientIP(request); got != "198.51.100.20" {
		t.Fatalf("trusted proxy client address=%q", got)
	}
}

func TestPasswordSlotsBoundConcurrentArgon2Work(t *testing.T) {
	app := &App{loginSlots: make(chan struct{}, 1)}
	request := httptest.NewRequest("POST", "http://example.test/login", nil)
	firstWriter := httptest.NewRecorder()
	_, release, ok := app.acquirePasswordSlot(firstWriter, request, nil, 5, "busy")
	if !ok {
		t.Fatal("first password operation was rejected")
	}

	secondWriter := httptest.NewRecorder()
	if _, _, ok := app.acquirePasswordSlot(secondWriter, request, nil, 5, "busy"); ok {
		t.Fatal("concurrent password operation bypassed the shared slot limit")
	}
	if secondWriter.Code != http.StatusTooManyRequests {
		t.Fatalf("busy password operation status=%d; want %d", secondWriter.Code, http.StatusTooManyRequests)
	}

	release()
	thirdWriter := httptest.NewRecorder()
	_, thirdRelease, ok := app.acquirePasswordSlot(thirdWriter, request, nil, 5, "busy")
	if !ok {
		t.Fatal("password slot was not released")
	}
	thirdRelease()
}
