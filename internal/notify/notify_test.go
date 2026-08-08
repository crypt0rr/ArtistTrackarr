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
	"testing"
	"time"
)

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
	if err := sender.Send(context.Background(), "generic+"+server.URL+"/slow", "title", "body"); err == nil {
		t.Fatal("slow notification was not bounded by the client timeout")
	}
}
