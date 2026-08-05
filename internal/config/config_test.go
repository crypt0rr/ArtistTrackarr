package config

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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
	for _, name := range []string{"PUBLIC_URL", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "LOG_LEVEL"} {
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
	if err := os.Setenv("APP_ENCRYPTION_KEY", strings.Repeat("e", 32)); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SETUP_TOKEN", strings.Repeat("t", 32)); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SESSION_SECRET", strings.Repeat("s", 32)); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("MUSICBRAINZ_CONTACT", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("Load() log level = %v, err=%v; want info", cfg.LogLevel, err)
	}
}

func TestParseTrustedProxyNetworks(t *testing.T) {
	networks, err := parseTrustedProxyNetworks("127.0.0.1/32, 10.0.0.0/8")
	if err != nil || len(networks) != 2 || !networks[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Fatalf("trusted proxy networks=%v err=%v", networks, err)
	}
	if _, err := parseTrustedProxyNetworks("not-a-network"); err == nil {
		t.Fatal("invalid trusted proxy network was accepted")
	}
}

func TestLoadRejectsUntrustedHTTPAndProxyWithoutNetworks(t *testing.T) {
	for _, name := range []string{"PUBLIC_URL", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "LOG_LEVEL"} {
		value, present := os.LookupEnv(name)
		_ = os.Unsetenv(name)
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	_ = os.Setenv("PUBLIC_URL", "http://tracker.example")
	_ = os.Setenv("APP_ENCRYPTION_KEY", strings.Repeat("e", 32))
	_ = os.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	_ = os.Setenv("MUSICBRAINZ_CONTACT", "test@example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PUBLIC_URL must use HTTPS") {
		t.Fatalf("external HTTP was accepted: %v", err)
	}
	_ = os.Setenv("PUBLIC_URL", "https://tracker.example")
	_ = os.Setenv("TRUST_PROXY", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TRUST_PROXY=true requires") {
		t.Fatalf("proxy without networks was accepted: %v", err)
	}
}

func TestSecretFileTakesPrecedenceAndTrims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("  file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("TEST_SECRET", "environment-value"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("TEST_SECRET_FILE", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Unsetenv("TEST_SECRET")
		_ = os.Unsetenv("TEST_SECRET_FILE")
	})
	value, err := secret("TEST_SECRET")
	if err != nil || value != "file-value" {
		t.Fatalf("secret() = %q, %v; want trimmed file value", value, err)
	}
}

func TestSecretUsesTrimmedEnvironmentValueWithoutFile(t *testing.T) {
	if err := os.Setenv("TEST_SECRET", "  environment-value\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("TEST_SECRET_FILE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("TEST_SECRET") })
	value, err := secret("TEST_SECRET")
	if err != nil || value != "environment-value" {
		t.Fatalf("secret() = %q, %v; want trimmed environment value", value, err)
	}
}

func TestSecretFileErrorIsReturnedWithoutPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if err := os.Setenv("TEST_SECRET_FILE", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("TEST_SECRET_FILE") })
	value, err := secret("TEST_SECRET")
	if err == nil || value != "" {
		t.Fatalf("secret() = %q, %v; want an error and empty value", value, err)
	}
	if !strings.Contains(err.Error(), "TEST_SECRET_FILE") {
		t.Fatalf("secret error %q does not identify the configured secret file", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("secret error exposed the configured path: %q", err)
	}
}

func TestSecretFileDirectoryIsReturnedAsError(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "secret-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("TEST_SECRET_FILE", directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("TEST_SECRET_FILE") })
	if value, err := secret("TEST_SECRET"); err == nil || value != "" {
		t.Fatalf("secret() = %q, %v; want an error for directory-valued file", value, err)
	}
}

func TestLoadReturnsSecretFileError(t *testing.T) {
	for _, name := range []string{"PUBLIC_URL", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "LOG_LEVEL"} {
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
	if err := os.Setenv("APP_ENCRYPTION_KEY_FILE", filepath.Join(t.TempDir(), "missing-key")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("APP_ENCRYPTION_KEY_FILE") })
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "APP_ENCRYPTION_KEY_FILE") {
		t.Fatalf("Load() error=%v; want APP_ENCRYPTION_KEY_FILE read error", err)
	}
}
