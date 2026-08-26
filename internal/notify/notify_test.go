package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/netpolicy"
	"time"

	"github.com/containrrr/shoutrrr/pkg/util/jsonclient"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name  string
		input DestinationInput
		want  string
	}{
		{"ntfy", DestinationInput{Service: "ntfy", Topic: "records"}, "ntfy://ntfy.sh/records"},
		{"generic", DestinationInput{Service: "generic", Target: "https://hooks.example/new"}, "generic+https://hooks.example/new"},
		{"discord", DestinationInput{Service: "discord", Token: "bot-token", Target: "123456"}, "discord://bot-token@123456"},
		{"telegram", DestinationInput{Service: "telegram", Token: "bot-token", Target: "-100123"}, "telegram://bot-token@telegram?chats=-100123"},
		{"advanced", DestinationInput{Service: "advanced", RawURL: " matrix://user:pass@example "}, "matrix://user:pass@example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := BuildURL(test.input)
			if err != nil || got != test.want {
				t.Fatalf("BuildURL() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

// The former TestBuildURLEmailAndOptionalCredentials covered smtp: URLs, which
// ValidateTransportPolicy rejects unconditionally, so the builder could only
// produce a destination that failed the moment it was used. Both the gotify and
// email branches were removed; the invariant below is what keeps the builder and
// the policy from drifting apart again.
func TestBuildURLOnlyProducesTransportsThePolicyAccepts(t *testing.T) {
	for _, test := range []struct {
		name  string
		input DestinationInput
	}{
		{"ntfy", DestinationInput{Service: "ntfy", Topic: "records"}},
		{"generic", DestinationInput{Service: "generic", Target: "https://hooks.example/new"}},
		{"discord", DestinationInput{Service: "discord", Token: "bot-token", Target: "123456"}},
		{"telegram", DestinationInput{Service: "telegram", Token: "bot-token", Target: "-100123"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			built, err := BuildURL(test.input)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}
			if err := ValidateTransportPolicy(built); err != nil {
				t.Fatalf("BuildURL produced %q, which the transport policy rejects: %v", built, err)
			}
		})
	}
	// A service the policy cannot accept must not be buildable at all.
	for _, service := range []string{"gotify", "email"} {
		if _, err := BuildURL(DestinationInput{Service: service, Host: "h", Token: "t"}); err == nil {
			t.Fatalf("BuildURL still constructs %q, which the transport policy always rejects", service)
		}
	}
}

func TestValidateOutboundTargetTreatsProviderIdentifiersAsNonHosts(t *testing.T) {
	for _, value := range []string{
		"discord://token@123456",
		"telegram://token@telegram?chats=-100123",
	} {
		if err := validateOutboundTarget(context.Background(), value, false, false); err != nil {
			t.Fatalf("provider URL %q was treated as a network target: %v", value, err)
		}
	}
}

func TestBlockedHostRecognizesObfuscatedIPv4Literals(t *testing.T) {
	for _, host := range []string{
		"2130706433", // decimal 127.0.0.1
		"0x7f000001", // hexadecimal 127.0.0.1
		"0177.0.0.1", // octal 127.0.0.1
		"127.1",      // 127.0.0.1 in two-component form
		"127.0.0.1",  // ordinary spelling remains covered here
		"3232235777", // decimal 192.168.1.1
	} {
		if !isBlockedHost(host, false) {
			t.Fatalf("obfuscated private host %q was not blocked", host)
		}
	}
	if isBlockedHost("2130706433", true) {
		t.Fatal("private-target opt-in rejected an obfuscated address")
	}
	for _, host := range []string{"example.test", "0177.example.test", "127.0.0.1."} {
		if netpolicy.ParseLegacyIPv4(host) != nil {
			t.Fatalf("ordinary hostname %q was parsed as a legacy IPv4 literal", host)
		}
	}
}

func TestNewShoutrrrSenderReusesTransportAcrossSends(t *testing.T) {
	sender := NewShoutrrrSender(true, time.Second)
	first := sender.httpClient()
	second := sender.httpClient()
	if first == second {
		t.Fatal("sender returned the same client object for separate sends")
	}
	if first.Transport == nil || second.Transport == nil || first.Transport != second.Transport {
		t.Fatalf("sender did not reuse its transport: first=%T second=%T", first.Transport, second.Transport)
	}
	sender.CloseIdleConnections()
}

func TestShoutrrrSenderValidationAndSendGuards(t *testing.T) {
	sender := ShoutrrrSender{}
	if err := sender.Validate("generic+https://example.com/hook"); err != nil {
		t.Fatalf("valid generic destination rejected: %v", err)
	}
	for _, value := range []string{"", "generic+https://127.0.0.1/hook"} {
		if err := sender.Validate(value); err == nil {
			t.Fatalf("invalid/private destination %q was accepted", value)
		}
	}
	if err := sender.Send(context.Background(), "generic+https://127.0.0.1/hook", "title", "body"); err == nil {
		t.Fatal("Send accepted a private destination")
	}
	if err := sender.Send(context.Background(), "unsupported://", "title", "body"); err == nil {
		t.Fatal("Send accepted an unsupported destination scheme")
	}
}

func TestNotificationMessageLimitsRejectLossyPayloadsBeforeNetwork(t *testing.T) {
	sender := ShoutrrrSender{AllowPrivateTargets: true}
	tests := []struct {
		name      string
		service   string
		title     string
		body      string
		wantSvc   string
		wantLimit int
	}{
		{
			name:      "telegram counts rendered unicode title",
			service:   "telegram://12345:mock-token@telegram?chats=-100123",
			title:     "Titel",
			body:      strings.Repeat("é", telegramMessageLimit),
			wantSvc:   "Telegram",
			wantLimit: telegramMessageLimit,
		},
		{
			name:      "discord rejects beyond total chunk budget",
			service:   "discord://token@123456",
			body:      strings.Repeat("x", discordMessageLimit+1),
			wantSvc:   "Discord",
			wantLimit: discordMessageLimit,
		},
		{
			name:      "generic webhook is bounded",
			service:   "generic+https://hooks.example/releases",
			body:      strings.Repeat("x", genericMessageLimitBytes+1),
			wantSvc:   "GENERIC+HTTPS",
			wantLimit: genericMessageLimitBytes,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := sender.Send(context.Background(), test.service, test.title, test.body)
			var limitErr *MessageLimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("Send error=%v, want MessageLimitError", err)
			}
			if limitErr.Service != test.wantSvc || limitErr.Limit != test.wantLimit {
				t.Fatalf("limit error=%#v, want service=%q limit=%d", limitErr, test.wantSvc, test.wantLimit)
			}
		})
	}
}

func TestNotificationMessageLimitsAllowNormalPayloads(t *testing.T) {
	for _, test := range []struct {
		service string
		body    string
	}{
		{service: "discord://token@123456", body: strings.Repeat("é", discordMessageLimit)},
		{service: "generic+https://hooks.example/releases", body: strings.Repeat("x", genericMessageLimitBytes)},
	} {
		if err := validateNotificationMessage(test.service, "", test.body); err != nil {
			t.Fatalf("validateNotificationMessage(%q)=%v for payload at limit", test.service, err)
		}
	}
}

func TestShoutrrrSenderSendsGenericWebhookAndReportsUpstreamErrors(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		requests <- string(body)
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := ShoutrrrSender{AllowPrivateTargets: true}
	if err := sender.Send(context.Background(), "generic+"+server.URL+"/ok", "Release title", "Release body"); err != nil {
		t.Fatalf("generic webhook send failed: %v", err)
	}
	if got := <-requests; !strings.Contains(got, "Release body") {
		t.Fatalf("webhook body=%q", got)
	}
	if err := sender.Send(context.Background(), "generic+"+server.URL+"/fail", "Release title", "Release body"); err == nil {
		t.Fatal("upstream webhook failure was ignored")
	}
}

func TestTelegramUsesScopedClientAndRestoresGlobals(t *testing.T) {
	previousHTTPClient := http.DefaultClient
	previousJSONClient := jsonclient.DefaultClient
	t.Cleanup(func() {
		http.DefaultClient = previousHTTPClient
		jsonclient.DefaultClient = previousJSONClient
	})

	var sentinelCalls atomic.Int32
	sentinelClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		sentinelCalls.Add(1)
		return nil, errors.New("sentinel client should not receive Telegram requests")
	})}
	sentinelJSONClient := jsonclient.NewWithHTTPClient(sentinelClient)
	http.DefaultClient = sentinelClient
	jsonclient.DefaultClient = sentinelJSONClient

	var scopedCalls atomic.Int32
	scopedClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		scopedCalls.Add(1)
		if req.URL.Host != "api.telegram.org" || req.URL.Path != "/bot12345:mock-token/sendMessage" {
			t.Errorf("unexpected Telegram request URL %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
			Request:    req,
		}, nil
	})}
	sender := ShoutrrrSender{client: scopedClient, SendTimeout: time.Second}
	if err := sender.Send(context.Background(), "telegram://12345:mock-token@telegram?chats=-100123", "title", "body"); err != nil {
		t.Fatalf("Telegram send failed: %v", err)
	}
	if got := scopedCalls.Load(); got != 1 {
		t.Fatalf("scoped Telegram client calls=%d, want 1", got)
	}
	if got := sentinelCalls.Load(); got != 0 {
		t.Fatalf("sentinel client calls=%d, want 0", got)
	}
	if http.DefaultClient != sentinelClient {
		t.Fatal("Telegram send leaked its scoped HTTP client")
	}
	if jsonclient.DefaultClient != sentinelJSONClient {
		t.Fatal("Telegram send leaked its scoped JSON client")
	}

	var timeoutCalls atomic.Int32
	timeoutClient := &http.Client{
		Timeout: 5 * time.Millisecond,
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			timeoutCalls.Add(1)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(5 * time.Millisecond):
				return nil, context.DeadlineExceeded
			}
		}),
	}
	// Use a fresh sender so a Telegram call that exceeds the sender-owned
	// client timeout still restores both compatibility globals.
	timeoutSender := ShoutrrrSender{client: timeoutClient, SendTimeout: time.Second}
	started := time.Now()
	err := timeoutSender.Send(context.Background(), "telegram://12345:mock-token@telegram?chats=-100123", "title", "body")
	if err == nil {
		t.Fatal("Telegram timeout was not reported")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Telegram timeout took %s", elapsed)
	}
	if got := timeoutCalls.Load(); got != 1 {
		t.Fatalf("timeout Telegram client calls=%d, want 1", got)
	}
	if http.DefaultClient != sentinelClient {
		t.Fatal("timed-out Telegram send leaked its scoped HTTP client")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	var cancelCalls atomic.Int32
	cancelClient := &http.Client{
		Timeout: time.Second,
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancelCalls.Add(1)
			cancel()
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	}
	cancelSender := ShoutrrrSender{client: cancelClient, SendTimeout: time.Second}
	if err := cancelSender.Send(cancelCtx, "telegram://12345:mock-token@telegram?chats=-100123", "title", "body"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Telegram send error=%v, want context.Canceled", err)
	}
	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("canceled Telegram client calls=%d, want 1", got)
	}
	if http.DefaultClient != sentinelClient {
		t.Fatal("canceled Telegram send leaked its scoped HTTP client")
	}
}

func TestTelegramDirectClientBuildsBoundedPayload(t *testing.T) {
	var got telegramSendPayload
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
			Request:    req,
		}, nil
	})}
	sender := ShoutrrrSender{client: client, SendTimeout: time.Second}
	serviceURL := "telegram://12345:mock-token@telegram?chats=-100123&preview=No&notification=No&parsemode=HTML"
	if err := sender.Send(context.Background(), serviceURL, "A <release>", "Body & details"); err != nil {
		t.Fatalf("Telegram send failed: %v", err)
	}
	if got.ChatID != "-100123" || got.ParseMode != "HTML" || !got.DisablePreview || !got.DisableNotification {
		t.Fatalf("payload=%#v", got)
	}
	if got.Text != "<b>A &lt;release&gt;</b>\nBody & details" {
		t.Fatalf("payload text=%q", got.Text)
	}
}

func TestTelegramRateLimitErrorParsesRetryAfter(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":24}}`)),
			Request:    req,
		}, nil
	})}
	limiter := &requestLimiter{interval: time.Millisecond}
	sender := ShoutrrrSender{client: client, SendTimeout: time.Second, limiter: limiter}
	err := sender.Send(context.Background(), "telegram://12345:mock-token@telegram?chats=-100123", "title", "body")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("Telegram error=%v, want RateLimitError", err)
	}
	if rateLimitErr.Service != "Telegram" || rateLimitErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit error=%#v", rateLimitErr)
	}
	if rateLimitErr.RetryAfter != 24*time.Second {
		t.Fatalf("retry-after=%s, want 24s", rateLimitErr.RetryAfter)
	}
	if !strings.Contains(err.Error(), "retry after 24s") {
		t.Fatalf("rate limit error=%q, want retry-after detail", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("Telegram requests=%d, want 1", requests.Load())
	}
	limiter.mu.Lock()
	next := limiter.next
	limiter.mu.Unlock()
	if time.Until(next) < 23*time.Second {
		t.Fatalf("limiter cooldown=%s, want at least 23s", time.Until(next))
	}
}

func TestTelegramRateLimitErrorParsesRetryAfterDescription(t *testing.T) {
	var gotRateLimit *RateLimitError
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 24"}`)),
			Request:    req,
		}, nil
	})}
	sender := ShoutrrrSender{client: client, SendTimeout: time.Second, limiter: &requestLimiter{interval: time.Millisecond}}
	err := sender.Send(context.Background(), "telegram://12345:mock-token@telegram?chats=-100123", "title", "body")
	if !errors.As(err, &gotRateLimit) || gotRateLimit.RetryAfter != 24*time.Second {
		t.Fatalf("Telegram error=%#v, want retry-after parsed from description", err)
	}
}

func TestNotificationRequestLimiterSpacesTelegramRequests(t *testing.T) {
	var mu sync.Mutex
	var requestTimes []time.Time
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
			Request:    req,
		}, nil
	})}
	const interval = 20 * time.Millisecond
	sender := ShoutrrrSender{client: client, SendTimeout: time.Second, limiter: &requestLimiter{interval: interval}}
	if err := sender.Send(context.Background(), "telegram://12345:mock-token@telegram?chats=-100123,-100456", "title", "body"); err != nil {
		t.Fatalf("Telegram send failed: %v", err)
	}
	mu.Lock()
	times := append([]time.Time(nil), requestTimes...)
	mu.Unlock()
	if len(times) != 2 {
		t.Fatalf("Telegram requests=%d, want 2", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < interval-5*time.Millisecond {
		t.Fatalf("Telegram request gap=%s, want at least %s", gap, interval-5*time.Millisecond)
	}
}

func TestRequestLimiterWaitHonorsCancellationAndBoundsCooldown(t *testing.T) {
	limiter := &requestLimiter{interval: time.Hour}
	if err := limiter.wait(context.Background()); err != nil {
		t.Fatalf("first limiter wait failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := limiter.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled limiter wait=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled limiter wait took %s", elapsed)
	}
	cooldownLimiter := &requestLimiter{interval: time.Second}
	cooldownLimiter.cooldown(2 * time.Hour)
	cooldownLimiter.mu.Lock()
	next := cooldownLimiter.next
	cooldownLimiter.mu.Unlock()
	if remaining := time.Until(next); remaining > maxNotificationCooldown+time.Second || remaining < maxNotificationCooldown-time.Second {
		t.Fatalf("cooldown=%s, want bounded to %s", remaining, maxNotificationCooldown)
	}
}

func TestObservedHTTPClientCompletesAndClearsInflightRequest(t *testing.T) {
	base := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	client, observer := observedHTTPClient(base, context.Background())

	done := make(chan error, 1)
	go func() {
		request, err := http.NewRequest(http.MethodGet, "https://example.test/hook", nil)
		if err != nil {
			done <- err
			return
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("observed client request failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("observed client request did not complete")
	}
	if observer.hasInFlightRequest() {
		t.Fatal("observed client retained an in-flight request after completion")
	}
}

func TestBuildURLRejectsIncompleteInput(t *testing.T) {
	for _, input := range []DestinationInput{
		{Service: "ntfy"},
		{Service: "generic", Target: "file:///tmp/hook"},
		{Service: "email", Host: "smtp.example"},
		{Service: "unknown"},
	} {
		if _, err := BuildURL(input); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Fatalf("expected error for %#v", input)
		}
	}
}

func TestValidateTransportPolicyMatrix(t *testing.T) {
	for _, serviceURL := range []string{
		"discord://token@123456",
		"telegram://token@telegram?chats=-100123",
		"ntfy://ntfy.sh/releases",
		"generic+http://hooks.example/releases",
		"generic+https://hooks.example/releases",
	} {
		if err := ValidateTransportPolicy(serviceURL); err != nil {
			t.Errorf("supported transport %q rejected: %v", serviceURL, err)
		}
	}
	for _, serviceURL := range []string{
		"gotify://push.example/token",
		"smtp://mail.example:587/",
		"matrix://room.example/room",
		"http://hooks.example/releases",
		"generic+ftp://hooks.example/releases",
	} {
		if err := ValidateTransportPolicy(serviceURL); !errors.Is(err, ErrUnsupportedTransport) {
			t.Errorf("unsupported transport %q error=%v, want ErrUnsupportedTransport", serviceURL, err)
		}
	}
}

func TestCanonicalTransportService(t *testing.T) {
	tests := map[string]string{
		"generic+http://hooks.example/releases":  "generic",
		"generic+https://hooks.example/releases": "generic",
		"DISCORD://token@123456":                 "discord",
		"telegram://token@telegram?chats=-100":   "telegram",
		"ntfy://ntfy.sh/releases":                "ntfy",
		"smtp://mail.example:587/":               "smtp",
		"not a URL":                              "unknown",
	}
	for serviceURL, want := range tests {
		if got := CanonicalTransportService(serviceURL); got != want {
			t.Errorf("CanonicalTransportService(%q)=%q, want %q", serviceURL, got, want)
		}
	}
}

func TestRedactErrorRemovesDestinationCredentials(t *testing.T) {
	message := RedactError(errors.New(`send failed to ntfy://user:pass@example.test/topic?token=abc: password=secret`))
	if strings.Contains(message, "example.test") || strings.Contains(message, "pass@") || strings.Contains(message, "?token=abc") || strings.Contains(message, "=secret") {
		t.Fatalf("redacted error still contains sensitive data: %q", message)
	}
	if !strings.Contains(message, "redacted destination") || !strings.Contains(message, "password=[redacted]") {
		t.Fatalf("redacted error lost useful context: %q", message)
	}
}

func TestRedactErrorRemovesBearerTokens(t *testing.T) {
	message := RedactError(errors.New(`request failed: Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9; bearer abc.def-123`))
	if strings.Contains(message, "eyJhbGci") || strings.Contains(message, "abc.def-123") {
		t.Fatalf("bearer token leaked in redacted error: %q", message)
	}
	if !strings.Contains(message, "Bearer [redacted]") {
		t.Fatalf("redacted bearer error lost context: %q", message)
	}
}

func TestValidateOutboundTargetRejectsPrivateAndLoopbackTargets(t *testing.T) {
	for _, value := range []string{
		"generic+http://127.0.0.1/hook",
		"ntfy://10.0.0.5/topic",
		"smtp://[::1]:25/",
		"generic+https://100.64.0.1/hook",
		"generic+https://[::ffff:127.0.0.1]/hook",
	} {
		if err := validateOutboundTarget(context.Background(), value, false, false); err == nil {
			t.Fatalf("private target %q was accepted", value)
		}
	}
	if err := validateOutboundTarget(context.Background(), "generic+https://127.0.0.1/hook", true, false); err != nil {
		t.Fatalf("explicit private-target opt-in rejected: %v", err)
	}
}

func TestOutboundTargetResolutionAndHostClassification(t *testing.T) {
	if err := validateOutboundTarget(context.Background(), "generic+https://invalid.invalid/hook", false, true); err == nil {
		t.Fatal("unresolvable destination was accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateOutboundTarget(canceled, "generic+https://invalid.invalid/hook", false, true); err == nil {
		t.Fatal("canceled destination lookup was accepted")
	}
	for _, test := range []struct {
		name string
		url  string
		host string
	}{
		{name: "generic", url: "generic+https://hooks.example/path", host: "hooks.example"},
		{name: "discord target is not host", url: "discord://token@123456", host: ""},
		{name: "telegram target is not host", url: "telegram://token@telegram?chats=-100", host: ""},
		{name: "ipv6", url: "https://[2001:db8::1]/hook", host: "2001:db8::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := outboundHost(parsed); got != test.host {
				t.Fatalf("outboundHost(%q)=%q, want %q", test.url, got, test.host)
			}
		})
	}
	if got := outboundHost(nil); got != "" {
		t.Fatalf("outboundHost(nil)=%q", got)
	}
	for _, value := range []string{"192.0.2.1", "198.18.0.1", "203.0.113.1", "2001:db8::1", "2001:0000:4136:e378:8000:63bf:3fff:fdd2", "2002:c000:0201::1", "64:ff9b::c000:0201", "64:ff9b:1::1", "224.0.0.1"} {
		ip := net.ParseIP(value)
		if ip == nil || !isBlockedIP(ip, false) {
			t.Fatalf("reserved address %s was not blocked", value)
		}
		if isBlockedIP(ip, true) {
			t.Fatalf("private-target opt-in did not allow %s", value)
		}
	}
}

func TestSenderHTTPClientBoundsSendsAndRevalidatesRedirects(t *testing.T) {
	client := newHTTPClient(time.Second, false, nil, nil)
	redirect, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("private redirect was accepted by the notification client")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := NewShoutrrrSender(true, 10*time.Millisecond)
	defer sender.CloseIdleConnections()
	before := http.DefaultClient
	if err := sender.Send(context.Background(), "generic+"+server.URL+"/slow", "title", "body"); err == nil {
		t.Fatal("slow notification was not bounded by the client timeout")
	}
	if http.DefaultClient != before {
		t.Fatal("notification send leaked its scoped HTTP client into the application")
	}
}

func TestDialApprovedRevalidatesResolvedAddressesAtConnectionTime(t *testing.T) {
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	dialCalled := false
	_, err := dialApproved(context.Background(), "tcp", "hooks.example:443", false, lookup, func(context.Context, string, string) (net.Conn, error) {
		dialCalled = true
		return nil, nil
	})
	if err == nil || dialCalled {
		t.Fatalf("resolved private address was dialed: err=%v called=%v", err, dialCalled)
	}

	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	dialCalled = false
	_, err = dialApproved(context.Background(), "tcp", "hooks.example:443", true, lookup, func(context.Context, string, string) (net.Conn, error) {
		dialCalled = true
		return left, nil
	})
	if err != nil || !dialCalled {
		t.Fatalf("private-target opt-in did not reach injected dialer: err=%v called=%v", err, dialCalled)
	}
}

func TestHTTPClientUsesResolverSeamForRedirectAndDial(t *testing.T) {
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	dialCalled := false
	client := newHTTPClient(time.Second, false, lookup, func(context.Context, string, string) (net.Conn, error) {
		dialCalled = true
		return nil, errors.New("dial should not be attempted for a blocked address")
	})
	request, err := http.NewRequest(http.MethodGet, "http://hooks.example/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err == nil || !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("blocked resolved address error=%v", err)
	}
	if dialCalled {
		t.Fatal("transport dialer was called after resolver rejected the address")
	}
}

func BenchmarkShoutrrrSendSerialization(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	// Use the production constructor so the benchmark measures both the
	// compatibility mutex and the sender-owned keep-alive transport. A zero
	// value sender intentionally creates a fallback client for unit tests, but
	// that would hide transport reuse in this benchmark.
	sender := NewShoutrrrSender(true, time.Second)
	sender.limiter = &requestLimiter{}
	defer sender.CloseIdleConnections()
	serviceURL := "generic+" + server.URL + "/hook"
	b.ReportAllocs()
	var queueWaitNanos, clientHoldNanos int64
	var sends int64
	observer := func(queueWait, clientHold time.Duration) {
		atomic.AddInt64(&queueWaitNanos, queueWait.Nanoseconds())
		atomic.AddInt64(&clientHoldNanos, clientHold.Nanoseconds())
		atomic.AddInt64(&sends, 1)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := sender.send(context.Background(), serviceURL, "benchmark", "body", observer); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.StopTimer()
	count := atomic.LoadInt64(&sends)
	if count > 0 {
		b.ReportMetric(float64(atomic.LoadInt64(&queueWaitNanos))/float64(count), "queue-wait-ns/op")
		b.ReportMetric(float64(atomic.LoadInt64(&clientHoldNanos))/float64(count), "client-mutex-ns/op")
	}
}

// TestQueuedSendGetsItsOwnTransportBudget pins where the per-send watchdog
// starts. Every HTTP-based send serialises through one process-global gate,
// because Shoutrrr reaches http.DefaultClient for several services. The send
// context used to be created before that gate was acquired, so queue time was
// charged against the transport budget: with the gate held for a full send
// timeout by someone else, the next send in line arrived with nothing left,
// returned a deadline error having issued no request at all, and the delivery
// layer recorded that as a genuine failure and backed the destination off - a
// slow destination degrading a healthy unrelated one.
func TestQueuedSendGetsItsOwnTransportBudget(t *testing.T) {
	const budget = 200 * time.Millisecond
	slowStarted := make(chan struct{})
	var fastRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(slowStarted)
			// Outlast the sender's whole budget so the gate is held for it.
			select {
			case <-r.Context().Done():
			case <-time.After(3 * budget):
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fastRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := ShoutrrrSender{AllowPrivateTargets: true, SendTimeout: budget}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		// Expected to fail; it is here only to hold the gate.
		_ = sender.Send(context.Background(), "generic+"+server.URL+"/slow", "title", "body")
	}()
	<-slowStarted

	// This send queues behind the slow one. Its budget must start when it
	// reaches the transport, not now.
	err := sender.Send(context.Background(), "generic+"+server.URL+"/fast", "title", "body")
	if err != nil {
		t.Fatalf("a send queued behind a slow one failed without reaching its destination: %v", err)
	}
	if got := fastRequests.Load(); got != 1 {
		t.Fatalf("requests to the healthy destination=%d, want 1", got)
	}
	<-slowDone
}

// TestQueuedSendStopsWaitingWhenItsCallerGivesUp keeps the queue bounded now
// that waiting no longer consumes the send budget. Shutdown and per-delivery
// deadlines both arrive as a cancelled context, and a send still parked in the
// queue has to honour them rather than block on the gate.
func TestQueuedSendStopsWaitingWhenItsCallerGivesUp(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(held)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := ShoutrrrSender{AllowPrivateTargets: true, SendTimeout: 5 * time.Second}
	holder := make(chan struct{})
	go func() {
		defer close(holder)
		_ = sender.Send(context.Background(), "generic+"+server.URL+"/hold", "title", "body")
	}()
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { waiting <- sender.Send(ctx, "generic+"+server.URL+"/queued", "title", "body") }()
	// Give the second send time to reach the gate, then withdraw it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued send returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled send stayed blocked on the gate")
	}
	close(release)
	<-holder
}

// TestDestinationInputCarriesOnlyConsumedFields is #289. #243 removed Gotify and
// SMTP as selectable transports but left their inputs behind: the Port control
// was hidden by app.js on first paint and could never be shown again, because no
// service's field set includes it — yet with JavaScript unavailable every field
// rendered, offering a control BuildURL has no service left to consume. In the
// mirror direction the handler read `from` and `to` form values that no template
// emits.
//
// This pins the struct to what BuildURL actually reads, so the next transport
// removal cannot leave the same residue.
func TestDestinationInputCarriesOnlyConsumedFields(t *testing.T) {
	fields := reflect.TypeOf(DestinationInput{})
	source, err := os.ReadFile("notify.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	start := strings.Index(body, "func BuildURL(")
	if start < 0 {
		t.Fatal("BuildURL not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("BuildURL body not delimited")
	}
	buildURL := body[start : start+end]

	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		if !strings.Contains(buildURL, "input."+name) {
			t.Errorf("DestinationInput.%s is never read by BuildURL, so the form collects a value nothing consumes", name)
		}
	}
}
