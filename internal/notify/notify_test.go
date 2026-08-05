package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
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
