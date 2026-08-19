package notify

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
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
		{"gotify", DestinationInput{Service: "gotify", Host: "push.example", Token: "abc"}, "gotify://push.example/abc"},
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

func TestBuildURLEmailAndOptionalCredentials(t *testing.T) {
	got, err := BuildURL(DestinationInput{Service: "email", Host: "smtp.example", Port: "2525", Username: "mailer", Password: "secret", From: "from@example", To: "to@example"})
	if err != nil {
		t.Fatal(err)
	}
	parsed := got
	for _, want := range []string{"smtp://", "smtp.example:2525", "from=from%40example", "to=to%40example", "mailer"} {
		if !strings.Contains(parsed, want) {
			t.Fatalf("email URL %q does not contain %q", parsed, want)
		}
	}
	if _, err := BuildURL(DestinationInput{Service: "email", Host: "smtp.example", Port: "not-a-port", From: "from@example", To: "to@example"}); err == nil {
		t.Fatal("invalid SMTP port was accepted")
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
	for _, value := range []string{"192.0.2.1", "198.18.0.1", "203.0.113.1", "2001:db8::1", "224.0.0.1"} {
		ip := net.ParseIP(value)
		if ip == nil || !isBlockedIP(ip, false) {
			t.Fatalf("reserved address %s was not blocked", value)
		}
		if isBlockedIP(ip, true) {
			t.Fatalf("private-target opt-in did not allow %s", value)
		}
	}
}

func TestConfigureHTTPClientBoundsSendsAndRevalidatesRedirects(t *testing.T) {
	ConfigureHTTPClient(time.Second, false)
	redirect, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := http.DefaultClient.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("private redirect was accepted by the notification client")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ConfigureHTTPClient(10*time.Millisecond, true)
	sender := ShoutrrrSender{AllowPrivateTargets: true, SendTimeout: 10 * time.Millisecond}
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
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "local or private") {
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
	sender := ShoutrrrSender{AllowPrivateTargets: true, SendTimeout: time.Second}
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
