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
	for _, name := range []string{"PUBLIC_URL", "LISTEN_ADDR", "DATABASE_PATH", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "ITUNES_MARKET", "LOG_LEVEL"} {
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
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "artist-tracker.db"))
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

func TestParseBoolAcceptsOnlyExplicitValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "true", value: "true", want: true},
		{name: "mixed case true", value: " TrUe ", want: true},
		{name: "false", value: "false", want: false},
		{name: "mixed case false", value: " FaLsE ", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseBool("TEST_FLAG", test.value)
			if err != nil || got != test.want {
				t.Fatalf("parseBool(%q) = %v, %v; want %v", test.value, got, err, test.want)
			}
		})
	}
	for _, value := range []string{"", "yes", "no", "1", "0", "enabled"} {
		if got, err := parseBool("TEST_FLAG", value); err == nil || got {
			t.Fatalf("parseBool(%q) = %v, %v; want a validation error", value, got, err)
		}
	}
}

func TestLoadRejectsUntrustedHTTPAndProxyWithoutNetworks(t *testing.T) {
	for _, name := range []string{"PUBLIC_URL", "LISTEN_ADDR", "DATABASE_PATH", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "ITUNES_MARKET", "LOG_LEVEL"} {
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
	for _, name := range []string{"PUBLIC_URL", "LISTEN_ADDR", "DATABASE_PATH", "SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "MUSICBRAINZ_CONTACT", "POLL_INTERVAL", "SPOTIFY_POLL_INTERVAL", "TRUST_PROXY", "TRUSTED_PROXY_CIDRS", "ALLOW_INSECURE_HTTP", "ALLOW_PRIVATE_NOTIFICATION_TARGETS", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_MARKET", "ITUNES_MARKET", "LOG_LEVEL"} {
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

func setLoadBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("PUBLIC_URL", "https://tracker.example")
	t.Setenv("LISTEN_ADDR", ":8080")
	t.Setenv("DATABASE_PATH", "/tmp/artist-tracker-test.db")
	t.Setenv("SETUP_TOKEN", strings.Repeat("t", 32))
	t.Setenv("APP_ENCRYPTION_KEY", strings.Repeat("e", 32))
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	t.Setenv("MUSICBRAINZ_CONTACT", "test@example.com")
	t.Setenv("POLL_INTERVAL", "6h")
	t.Setenv("SPOTIFY_POLL_INTERVAL", "24h")
	t.Setenv("TRUST_PROXY", "false")
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	t.Setenv("ALLOW_INSECURE_HTTP", "false")
	t.Setenv("ALLOW_PRIVATE_NOTIFICATION_TARGETS", "false")
	t.Setenv("SPOTIFY_CLIENT_ID", "")
	t.Setenv("SPOTIFY_CLIENT_SECRET", "")
	t.Setenv("SPOTIFY_MARKET", "US")
	t.Setenv("ITUNES_MARKET", "US")
	t.Setenv("LOG_LEVEL", "info")
	for _, name := range []string{"SETUP_TOKEN", "APP_ENCRYPTION_KEY", "SESSION_SECRET", "SPOTIFY_CLIENT_SECRET"} {
		t.Setenv(name+"_FILE", "")
	}
}

func TestLoadRejectsInvalidRuntimeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		set  func()
		want string
	}{
		{name: "log level", set: func() { t.Setenv("LOG_LEVEL", "trace") }, want: "LOG_LEVEL"},
		{name: "poll interval", set: func() { t.Setenv("POLL_INTERVAL", "30m") }, want: "POLL_INTERVAL"},
		{name: "Spotify credentials", set: func() { t.Setenv("SPOTIFY_CLIENT_ID", "client-id") }, want: "SPOTIFY_CLIENT_ID"},
		{name: "market", set: func() { t.Setenv("SPOTIFY_MARKET", "USA") }, want: "SPOTIFY_MARKET"},
		{name: "trust proxy boolean", set: func() { t.Setenv("TRUST_PROXY", "yes") }, want: "TRUST_PROXY"},
		{name: "insecure HTTP boolean", set: func() { t.Setenv("ALLOW_INSECURE_HTTP", "enabled") }, want: "ALLOW_INSECURE_HTTP"},
		{name: "private target boolean", set: func() { t.Setenv("ALLOW_PRIVATE_NOTIFICATION_TARGETS", "1") }, want: "ALLOW_PRIVATE_NOTIFICATION_TARGETS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setLoadBaseline(t)
			test.set()
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error=%v; want %q validation", err, test.want)
			}
		})
	}
}

func TestLoadAllowsLocalHTTPAndValidOptionalConfiguration(t *testing.T) {
	setLoadBaseline(t)
	t.Setenv("PUBLIC_URL", "http://localhost:8080")
	t.Setenv("SPOTIFY_CLIENT_ID", "client-id")
	t.Setenv("SPOTIFY_CLIENT_SECRET", "client-secret")
	t.Setenv("SPOTIFY_MARKET", "nl")
	t.Setenv("ITUNES_MARKET", "ca")
	t.Setenv("LOG_LEVEL", "DEBUG")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpotifyMarket != "NL" || cfg.ITunesMarket != "CA" || cfg.SpotifyClientID != "client-id" || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("optional configuration=%#v", cfg)
	}
}

func TestLoadRejectsUnsafeOrEmptyRuntimeValues(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T)
		want string
	}{
		{name: "empty listen address", set: func(t *testing.T) { t.Setenv("LISTEN_ADDR", " \t") }, want: "LISTEN_ADDR"},
		{name: "invalid listen address", set: func(t *testing.T) { t.Setenv("LISTEN_ADDR", "not-an-address") }, want: "LISTEN_ADDR"},
		{name: "empty database path", set: func(t *testing.T) { t.Setenv("DATABASE_PATH", " \t") }, want: "DATABASE_PATH"},
		{name: "public URL path", set: func(t *testing.T) { t.Setenv("PUBLIC_URL", "https://tracker.example/app") }, want: "PUBLIC_URL"},
		{name: "same secrets", set: func(t *testing.T) { t.Setenv("SESSION_SECRET", strings.Repeat("e", 32)) }, want: "different values"},
		{name: "iTunes market", set: func(t *testing.T) { t.Setenv("ITUNES_MARKET", "USA") }, want: "ITUNES_MARKET"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setLoadBaseline(t)
			test.set(t)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error=%v; want %q validation", err, test.want)
			}
		})
	}
}

func TestValidateDatabasePathRejectsDirectoryAndMissingParent(t *testing.T) {
	directory := t.TempDir()
	if err := validateDatabasePath(directory); err == nil || !strings.Contains(err.Error(), "file") {
		t.Fatalf("directory database path accepted: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing", "artist-tracker.db")
	if err := validateDatabasePath(missing); err == nil || !strings.Contains(err.Error(), "parent directory") {
		t.Fatalf("missing database parent accepted: %v", err)
	}
}

func TestLoadRejectsUnsafeMusicBrainzContact(t *testing.T) {
	setLoadBaseline(t)
	t.Setenv("MUSICBRAINZ_CONTACT", "operator@example.com\nInjected: value")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MUSICBRAINZ_CONTACT") {
		t.Fatalf("unsafe MusicBrainz contact accepted: %v", err)
	}
}

func TestLoadTrimsTrustProxy(t *testing.T) {
	setLoadBaseline(t)
	t.Setenv("TRUST_PROXY", " true ")
	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1/32")
	cfg, err := Load()
	if err != nil || !cfg.TrustProxy || len(cfg.TrustedProxyNetworks) != 1 {
		t.Fatalf("Load() trust proxy=%v networks=%v err=%v", cfg.TrustProxy, cfg.TrustedProxyNetworks, err)
	}
}
