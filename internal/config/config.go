package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr                      string
	PublicURL                       *url.URL
	DatabasePath                    string
	SetupToken                      string
	EncryptionKey                   string
	SessionSecret                   string
	MusicBrainzContact              string
	PollInterval                    time.Duration
	SpotifyPollInterval             time.Duration
	TrustProxy                      bool
	TrustedProxyNetworks            []*net.IPNet
	AllowInsecureHTTP               bool
	AllowPrivateNotificationTargets bool
	SpotifyClientID                 string
	SpotifySecret                   string
	SpotifyMarket                   string
	LogLevel                        slog.Level
}

func Load() (Config, error) {
	publicURL, err := url.Parse(env("PUBLIC_URL", "http://localhost:8080"))
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return Config{}, errors.New("PUBLIC_URL must be an absolute http(s) URL")
	}
	if !strings.EqualFold(publicURL.Scheme, "http") && !strings.EqualFold(publicURL.Scheme, "https") {
		return Config{}, errors.New("PUBLIC_URL must use http or https")
	}
	interval, err := time.ParseDuration(env("POLL_INTERVAL", "6h"))
	if err != nil || interval < time.Hour {
		return Config{}, errors.New("POLL_INTERVAL must be a duration of at least 1h")
	}
	spotifyInterval, err := time.ParseDuration(env("SPOTIFY_POLL_INTERVAL", "24h"))
	if err != nil || spotifyInterval < time.Hour {
		return Config{}, errors.New("SPOTIFY_POLL_INTERVAL must be a duration of at least 1h")
	}
	setupToken, err := secret("SETUP_TOKEN")
	if err != nil {
		return Config{}, err
	}
	encryptionKey, err := secret("APP_ENCRYPTION_KEY")
	if err != nil {
		return Config{}, err
	}
	sessionSecret, err := secret("SESSION_SECRET")
	if err != nil {
		return Config{}, err
	}
	spotifySecret, err := secret("SPOTIFY_CLIENT_SECRET")
	if err != nil {
		return Config{}, err
	}
	allowInsecureHTTP := strings.EqualFold(strings.TrimSpace(env("ALLOW_INSECURE_HTTP", "false")), "true")
	trustedProxyNetworks, err := parseTrustedProxyNetworks(env("TRUSTED_PROXY_CIDRS", ""))
	if err != nil {
		return Config{}, err
	}
	trustProxy := strings.EqualFold(env("TRUST_PROXY", "false"), "true")
	if trustProxy && len(trustedProxyNetworks) == 0 {
		return Config{}, errors.New("TRUST_PROXY=true requires TRUSTED_PROXY_CIDRS")
	}
	if strings.EqualFold(publicURL.Scheme, "http") && !isLocalHost(publicURL.Hostname()) && !allowInsecureHTTP {
		return Config{}, errors.New("PUBLIC_URL must use HTTPS unless it points to localhost or ALLOW_INSECURE_HTTP=true")
	}
	cfg := Config{
		ListenAddr:                      env("LISTEN_ADDR", ":8080"),
		PublicURL:                       publicURL,
		DatabasePath:                    env("DATABASE_PATH", "/data/artist-tracker.db"),
		SetupToken:                      setupToken,
		EncryptionKey:                   encryptionKey,
		SessionSecret:                   sessionSecret,
		MusicBrainzContact:              strings.TrimSpace(env("MUSICBRAINZ_CONTACT", "")),
		PollInterval:                    interval,
		SpotifyPollInterval:             spotifyInterval,
		TrustProxy:                      trustProxy,
		TrustedProxyNetworks:            trustedProxyNetworks,
		AllowInsecureHTTP:               allowInsecureHTTP,
		AllowPrivateNotificationTargets: strings.EqualFold(strings.TrimSpace(env("ALLOW_PRIVATE_NOTIFICATION_TARGETS", "false")), "true"),
		SpotifyClientID:                 strings.TrimSpace(env("SPOTIFY_CLIENT_ID", "")),
		SpotifySecret:                   spotifySecret,
		SpotifyMarket:                   strings.ToUpper(strings.TrimSpace(env("SPOTIFY_MARKET", "US"))),
	}
	cfg.LogLevel, err = parseLogLevel(env("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	if len(cfg.SetupToken) < 32 {
		return Config{}, errors.New("SETUP_TOKEN must be at least 32 characters")
	}
	if len(cfg.EncryptionKey) < 32 || len(cfg.SessionSecret) < 32 {
		return Config{}, errors.New("APP_ENCRYPTION_KEY and SESSION_SECRET must each be at least 32 characters")
	}
	if cfg.MusicBrainzContact == "" {
		return Config{}, errors.New("MUSICBRAINZ_CONTACT is required")
	}
	if (cfg.SpotifyClientID == "") != (cfg.SpotifySecret == "") {
		return Config{}, errors.New("SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET must be configured together")
	}
	if len(cfg.SpotifyMarket) != 2 ||
		cfg.SpotifyMarket[0] < 'A' || cfg.SpotifyMarket[0] > 'Z' ||
		cfg.SpotifyMarket[1] < 'A' || cfg.SpotifyMarket[1] > 'Z' {
		return Config{}, errors.New("SPOTIFY_MARKET must be a two-letter ISO country code")
	}
	return cfg, nil
}

func parseTrustedProxyNetworks(value string) ([]*net.IPNet, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	networks := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS contains invalid network %q", part)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, or error")
	}
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func secret(name string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			// Keep startup diagnostics useful without echoing the configured
			// filesystem path (which may itself contain sensitive deployment
			// details). The secret contents are never included.
			reason := "could not read the configured file"
			switch {
			case os.IsNotExist(err):
				reason = "configured file does not exist"
			case os.IsPermission(err):
				reason = "permission denied reading configured file"
			}
			return "", fmt.Errorf("read %s_FILE: %s", name, reason)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}
