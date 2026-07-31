package config

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{value: "debug", want: slog.LevelDebug},
		{value: "INFO", want: slog.LevelInfo},
		{value: " Warn ", want: slog.LevelWarn},
		{value: "error", want: slog.LevelError},
	}
	for _, test := range tests {
		got, err := parseLogLevel(test.value)
		if err != nil || got != test.want {
			t.Fatalf("parseLogLevel(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
}

func TestLogLevelFiltersRecords(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logger.Info("hidden")
	logger.Warn("visible")
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "visible") {
		t.Fatalf("unexpected filtered log output: %s", output.String())
	}
}

func TestParseLogLevelRejectsInvalidAndEmptyValues(t *testing.T) {
	for _, value := range []string{"", "trace", "verbose"} {
		if _, err := parseLogLevel(value); err == nil {
			t.Fatalf("parseLogLevel(%q) accepted invalid value", value)
		}
	}
}

func TestLoadDefaultsToInfoLogLevel(t *testing.T) {
	for _, name := range []string{"PUBLIC_URL", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "LOG_LEVEL"} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	os.Setenv("APP_ENCRYPTION_KEY", strings.Repeat("e", 32))
	os.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	os.Setenv("MUSICBRAINZ_CONTACT", "test@example.com")
	cfg, err := Load()
	if err != nil || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("Load() log level = %v, err=%v; want info", cfg.LogLevel, err)
	}
}
