package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/go-chi/chi/v5"
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
	if !limiter.Allow("first") {
		t.Fatal("limiter rejected the first distinct key")
	}
	if !limiter.Allow("second") {
		t.Fatal("limiter rejected the second distinct key")
	}
	if !limiter.Allow("third") {
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

func TestClientIPJoinsRepeatedForwardedHeaderLines(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: config.Config{TrustProxy: true, TrustedProxyNetworks: []*net.IPNet{proxyNetwork}}}
	request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Add("X-Forwarded-For", "198.51.100.99")
	request.Header.Add("X-Forwarded-For", "203.0.113.7")
	if got := app.clientIP(request); got != "203.0.113.7" {
		t.Fatalf("clientIP=%q, want the nearest untrusted address across all header lines", got)
	}
}

func TestClientIPAggregatesIPv6Slash64(t *testing.T) {
	want := "2001:db8:abcd:1::"
	app := &App{}
	direct := []string{
		"[2001:db8:abcd:1:1111::1]:1234",
		"[2001:db8:abcd:1:2222::2]:5678",
	}
	for _, peer := range direct {
		request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		request.RemoteAddr = peer
		if got := app.clientIP(request); got != want {
			t.Errorf("direct peer %q resolved to %q, want %q", peer, got, want)
		}
	}

	_, proxyNetwork, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	app.cfg = config.Config{TrustProxy: true, TrustedProxyNetworks: []*net.IPNet{proxyNetwork}}
	for _, forwarded := range []string{
		"2001:db8:abcd:1:1111::1, 127.0.0.1",
		"2001:db8:abcd:1:2222::2, 127.0.0.1",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("X-Forwarded-For", forwarded)
		if got := app.clientIP(request); got != want {
			t.Errorf("forwarded chain %q resolved to %q, want %q", forwarded, got, want)
		}
	}

	app.cfg = config.Config{}
	request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	request.RemoteAddr = "[::ffff:198.51.100.9]:1234"
	if got := app.clientIP(request); got != "198.51.100.9" {
		t.Fatalf("IPv4-mapped peer resolved to %q, want normalized IPv4", got)
	}
}

func TestLoginWarningUsesJoinedForwardedHeaderLines(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "login-warning.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	public, _ := url.Parse("http://example.test")
	cfg := config.Config{
		PublicURL: public, SessionSecret: "the session secret has more than 32 bytes",
		EncryptionKey: "the encryption key has more than 32 bytes",
	}
	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := New(cfg, database, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil,
		slog.New(slog.NewJSONHandler(&logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test/login", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Add("X-Forwarded-For", "")
	request.Header.Add("X-Forwarded-For", "203.0.113.9")
	request.Form = url.Values{"email": {"member@example.com"}, "password": {"incorrect password"}}
	app.login(httptest.NewRecorder(), request)
	if !strings.Contains(logs.String(), "login throttling is degraded") {
		t.Fatalf("login did not warn about the joined forwarded-header value: %s", logs.String())
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

func TestAuthenticatedLimiterKeysUseUserIDOnly(t *testing.T) {
	app := &App{providerLimiter: newFixedWindowLimiter(1, time.Minute)}
	requestFor := func(address string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "http://example.test/artists/1/sync", nil)
		request.RemoteAddr = address + ":1234"
		return request.WithContext(context.WithValue(request.Context(), sessionKey, store.Session{
			User: store.User{ID: 42},
		}))
	}
	first := httptest.NewRecorder()
	if !app.allowProviderAction(first, requestFor("192.0.2.10")) {
		t.Fatal("first provider action was rejected")
	}
	second := httptest.NewRecorder()
	if app.allowProviderAction(second, requestFor("198.51.100.20")) {
		t.Fatal("changing client address reset the same user's provider budget")
	}
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "600" {
		t.Fatalf("rate-limited response status=%d retry-after=%q", second.Code, second.Header().Get("Retry-After"))
	}
}

func TestFixedWindowLimiterAllowNChargesAtomically(t *testing.T) {
	limiter := newFixedWindowLimiter(5, time.Minute)
	if !limiter.AllowN("member", 2) {
		t.Fatal("limiter rejected a two-token charge")
	}
	if got := limiter.entries["member"].count; got != 2 {
		t.Fatalf("new bucket count=%d, want 2", got)
	}
	if !limiter.AllowN("member", 2) {
		t.Fatal("limiter rejected a valid second two-token charge")
	}
	if got := limiter.entries["member"].count; got != 4 {
		t.Fatalf("second charge count=%d, want 4", got)
	}
	if limiter.AllowN("member", 2) {
		t.Fatal("limiter accepted a charge larger than the remaining budget")
	}
	if got := limiter.entries["member"].count; got != 4 {
		t.Fatalf("rejected charge changed count to %d, want 4", got)
	}
	if !limiter.AllowN("member", 1) {
		t.Fatal("rejected charge consumed budget needed by a later request")
	}
	if got := limiter.entries["member"].count; got != 5 {
		t.Fatalf("last available token count=%d, want 5", got)
	}
	if limiter.AllowN("member", 1) {
		t.Fatal("limiter accepted a request after the full budget was consumed")
	}
}

func TestFollowBatchChargesOneTokenPerArtistWithoutSpotify(t *testing.T) {
	mb, ids := rateLimitTestCatalog()
	app, userID := newRateLimitTestApp(t, mb, nil, nil, 10)
	request := requestWithForm(userID, "192.0.2.10", url.Values{"mbids": ids})
	response := httptest.NewRecorder()
	app.followBatch(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("10-artist batch status=%d, want redirect", response.Code)
	}
	if mb.resolveCalls != 10 {
		t.Fatalf("MusicBrainz lookups=%d, want 10", mb.resolveCalls)
	}
	key := userRateLimitKey(userID)
	if got := app.providerLimiter.entries[key].count; got != 10 {
		t.Fatalf("batch charged %d tokens, want 10", got)
	}
	otherUserID, err := app.store.CreateUser(context.Background(), "other@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	otherBatch := httptest.NewRecorder()
	app.followBatch(otherBatch, requestWithForm(otherUserID, "203.0.113.20", url.Values{"mbids": ids}))
	if otherBatch.Code != http.StatusSeeOther {
		t.Fatalf("second user's batch status=%d, want redirect", otherBatch.Code)
	}
	if got := app.providerLimiter.entries[userRateLimitKey(otherUserID)].count; got != 10 {
		t.Fatalf("second user's independent batch charged %d tokens, want 10", got)
	}
	limited := httptest.NewRecorder()
	if app.allowProviderAction(limited, requestWithForm(userID, "198.51.100.20", nil)) {
		t.Fatal("the next provider action bypassed the exhausted batch budget")
	}
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("next action status=%d Retry-After=%q, want 429 with retry delay", limited.Code, limited.Header().Get("Retry-After"))
	}
}

func TestSingleFollowSyncAndSpotifyBatchEachCostOneToken(t *testing.T) {
	mb, ids := rateLimitTestCatalog()
	const spotifyID = "0OdUWJ0sBjDrqHygGUXeCF"
	spotify := &fakeSpotify{artists: map[string]catalog.SpotifyArtist{
		spotifyID: {ID: spotifyID, Name: "Spotify Artist"},
	}}
	app, userID := newRateLimitTestApp(t, mb, spotify, nil, 10)
	key := userRateLimitKey(userID)

	followResponse := httptest.NewRecorder()
	app.follow(followResponse, requestWithForm(userID, "192.0.2.10", url.Values{"mbid": {ids[0]}}))
	if followResponse.Code != http.StatusSeeOther || app.providerLimiter.entries[key].count != 1 {
		t.Fatalf("single follow status=%d charged=%d, want redirect and one token", followResponse.Code, app.providerLimiter.entries[key].count)
	}

	artist, err := app.store.UpsertArtist(context.Background(), store.Artist{
		MBID: "33333333-3333-4333-8333-333333333333", Name: "Sync Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Follow(context.Background(), userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	syncRequest := requestWithForm(userID, "192.0.2.10", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", fmt.Sprint(artist.ID))
	syncRequest = syncRequest.WithContext(context.WithValue(syncRequest.Context(), chi.RouteCtxKey, routeContext))
	syncResponse := httptest.NewRecorder()
	app.syncArtist(syncResponse, syncRequest)
	if syncResponse.Code != http.StatusSeeOther || app.providerLimiter.entries[key].count != 2 {
		t.Fatalf("sync status=%d charged=%d, want redirect and two total tokens", syncResponse.Code, app.providerLimiter.entries[key].count)
	}

	spotifyResponse := httptest.NewRecorder()
	app.followSpotifyBatch(spotifyResponse, requestWithForm(userID, "192.0.2.10", url.Values{"spotify_ids": {spotifyID}}))
	if spotifyResponse.Code != http.StatusSeeOther || app.providerLimiter.entries[key].count != 3 {
		t.Fatalf("Spotify batch status=%d charged=%d, want redirect and three total tokens", spotifyResponse.Code, app.providerLimiter.entries[key].count)
	}
}

func TestFollowBatchChargesSpotifyEnrichmentPerArtist(t *testing.T) {
	mb, ids := rateLimitTestCatalog()
	spotify := &fakeSpotify{}
	app, userID := newRateLimitTestApp(t, mb, spotify, nil, 20)
	response := httptest.NewRecorder()
	app.followBatch(response, requestWithForm(userID, "192.0.2.10", url.Values{"mbids": ids}))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("Spotify-enriched batch status=%d, want redirect", response.Code)
	}
	if mb.resolveCalls != 10 || spotify.searchCalls != 10 {
		t.Fatalf("provider lookups: MusicBrainz=%d Spotify=%d, want 10 each", mb.resolveCalls, spotify.searchCalls)
	}
	if got := app.providerLimiter.entries[userRateLimitKey(userID)].count; got != 20 {
		t.Fatalf("Spotify-enriched batch charged %d tokens, want 20", got)
	}
}

func TestBatchProviderRateLimitRejectsBeforeLookup(t *testing.T) {
	t.Run("MusicBrainz plus Spotify", func(t *testing.T) {
		mb, ids := rateLimitTestCatalog()
		spotify := &fakeSpotify{}
		app, userID := newRateLimitTestApp(t, mb, spotify, nil, 19)
		response := httptest.NewRecorder()
		app.followBatch(response, requestWithForm(userID, "192.0.2.10", url.Values{"mbids": ids}))
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
			t.Fatalf("status=%d Retry-After=%q, want 429 with retry delay", response.Code, response.Header().Get("Retry-After"))
		}
		if mb.resolveCalls != 0 || spotify.searchCalls != 0 {
			t.Fatalf("provider calls happened before rate-limit rejection: MusicBrainz=%d Spotify=%d", mb.resolveCalls, spotify.searchCalls)
		}
	})
	t.Run("iTunes", func(t *testing.T) {
		itunes := &countingITunes{}
		app, userID := newRateLimitTestApp(t, nil, nil, itunes, 9)
		values := make([]string, 10)
		for i := range values {
			values[i] = fmt.Sprintf("%d", i+1)
		}
		response := httptest.NewRecorder()
		app.followITunesBatch(response, requestWithForm(userID, "192.0.2.10", url.Values{"itunes_ids": values}))
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
			t.Fatalf("status=%d Retry-After=%q, want 429 with retry delay", response.Code, response.Header().Get("Retry-After"))
		}
		if itunes.calls != 0 {
			t.Fatalf("iTunes lookups=%d, want none before rate-limit rejection", itunes.calls)
		}
	})
}

type countingITunes struct{ calls int }

func (f *countingITunes) SearchArtists(context.Context, string) ([]catalog.ITunesArtist, error) {
	return nil, nil
}

func (f *countingITunes) Artist(_ context.Context, id string) (catalog.ITunesArtist, error) {
	f.calls++
	return catalog.ITunesArtist{ID: id, Name: "iTunes Artist"}, nil
}

func newRateLimitTestApp(
	t *testing.T,
	mb catalog.CatalogProvider,
	spotify catalog.SpotifyProvider,
	itunes catalog.ITunesProvider,
	limit int,
) (*App, int64) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "provider-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	userID, err := database.CreateUser(context.Background(), "member@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		cfg:   config.Config{SessionSecret: "the session secret has more than 32 bytes"},
		store: database, mb: mb, spotify: spotify, itunes: itunes,
		providerLimiter: newFixedWindowLimiter(limit, 10*time.Minute),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, userID
}

func requestWithForm(userID int64, remoteIP string, form url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/provider-action", nil)
	request.RemoteAddr = remoteIP + ":1234"
	request.Form = form
	return request.WithContext(context.WithValue(request.Context(), sessionKey, store.Session{
		User: store.User{ID: userID},
	}))
}

func rateLimitTestCatalog() (*searchCatalog, []string) {
	resolved := make(map[string]catalog.ArtistResult, 10)
	ids := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("%08d-1111-4111-8111-%012d", i, i)
		ids = append(ids, id)
		resolved[id] = catalog.ArtistResult{MBID: id, Name: fmt.Sprintf("Artist %d", i)}
	}
	return &searchCatalog{resolved: resolved}, ids
}
