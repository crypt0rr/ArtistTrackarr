package notify

import (
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
