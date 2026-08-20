package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestFixedWindowLimiterBoundsRequestsAndExpires(t *testing.T) {
	limiter := newFixedWindowLimiter(2, 20*time.Millisecond)
	if !limiter.Allow("client") {
		t.Fatal("fixed-window limiter rejected the first request")
	}
	if !limiter.Allow("client") {
		t.Fatal("fixed-window limiter rejected the second request")
	}
	if limiter.Allow("client") {
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
	_, proxyNetwork, err := net.ParseCIDR("127.0.0.0/8")
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
	request.Header.Set("X-Forwarded-For", "127.0.0.1, 127.0.0.1")
	if got := app.clientIP(request); got != "127.0.0.1" {
		t.Fatalf("all-trusted forwarded chain address=%q", got)
	}
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := app.clientIP(request); got != "127.0.0.1" {
		t.Fatalf("invalid forwarded chain address=%q", got)
	}
	// Once the whole proxy chain is trusted, changing its leftmost value must
	// not change the throttling identity. The direct peer is the only trusted
	// address available in that case.
	request.Header.Set("X-Forwarded-For", "127.0.0.1, 127.0.0.1")
	first := app.clientIP(request)
	request.Header.Set("X-Forwarded-For", "127.0.0.2, 127.0.0.1")
	second := app.clientIP(request)
	if first != "127.0.0.1" || second != first {
		t.Fatalf("all-trusted proxy chain changed identity: first=%q second=%q", first, second)
	}

	request.RemoteAddr = "203.0.113.10"
	request.Header.Set("X-Forwarded-For", "198.51.100.20")
	if got := app.clientIP(request); got != "203.0.113.10" {
		t.Fatalf("address without port=%q", got)
	}
	app.cfg.TrustProxy = false
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.20")
	if got := app.clientIP(request); got != "127.0.0.1" {
		t.Fatalf("forwarded address accepted when proxy trust disabled: %q", got)
	}
}

func TestLoginThrottleKeysIncludePeerAndAccount(t *testing.T) {
	keys := loginThrottleKeys("198.51.100.20", "Member@Example.com")
	if len(keys) != 2 || keys[0] != "198.51.100.20|member@example.com" || keys[1] != "account:member@example.com" {
		t.Fatalf("login throttle keys=%#v", keys)
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

func TestFormatRetryAfterNormalizesNonPositiveValues(t *testing.T) {
	for _, test := range []struct {
		seconds int
		want    string
	}{
		{seconds: -10, want: "1"},
		{seconds: 0, want: "1"},
		{seconds: 42, want: "42"},
	} {
		if got := formatRetryAfter(test.seconds); got != test.want {
			t.Errorf("formatRetryAfter(%d)=%q, want %q", test.seconds, got, test.want)
		}
	}
}

func TestAllowProviderActionReturnsRetryResponseAfterLimit(t *testing.T) {
	app := &App{
		providerLimiter: newFixedWindowLimiter(1, time.Minute),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test/artists/1/sync", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request = request.WithContext(context.WithValue(request.Context(), sessionKey, store.Session{
		User: store.User{ID: 42},
	}))
	first := httptest.NewRecorder()
	if !app.allowProviderAction(first, request) {
		t.Fatal("first provider action was rejected")
	}
	second := httptest.NewRecorder()
	if app.allowProviderAction(second, request) {
		t.Fatal("second provider action bypassed the limiter")
	}
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "600" {
		t.Fatalf("rate-limited response status=%d retry-after=%q", second.Code, second.Header().Get("Retry-After"))
	}
}
