package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr          string
	PublicURL           *url.URL
	DatabasePath        string
	SetupToken          string
	EncryptionKey       string
	SessionSecret       string
	MusicBrainzContact  string
	PollInterval        time.Duration
	SpotifyPollInterval time.Duration
	TrustProxy          bool
	SpotifyClientID     string
	SpotifySecret       string
	SpotifyMarket       string
	LogLevel            slog.Level
}

func Load() (Config, error) {
	publicURL, err := url.Parse(env("PUBLIC_URL", "http://localhost:8080"))
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return Config{}, errors.New("PUBLIC_URL must be an absolute http(s) URL")
	}
	interval, err := time.ParseDuration(env("POLL_INTERVAL", "6h"))
	if err != nil || interval < time.Hour {
		return Config{}, errors.New("POLL_INTERVAL must be a duration of at least 1h")
	}
	spotifyInterval, err := time.ParseDuration(env("SPOTIFY_POLL_INTERVAL", "24h"))
	if err != nil || spotifyInterval < time.Hour {
		return Config{}, errors.New("SPOTIFY_POLL_INTERVAL must be a duration of at least 1h")
	}
	cfg := Config{
		ListenAddr:          env("LISTEN_ADDR", ":8080"),
		PublicURL:           publicURL,
		DatabasePath:        env("DATABASE_PATH", "/data/artist-tracker.db"),
		SetupToken:          secret("SETUP_TOKEN"),
		EncryptionKey:       secret("APP_ENCRYPTION_KEY"),
		SessionSecret:       secret("SESSION_SECRET"),
		MusicBrainzContact:  strings.TrimSpace(env("MUSICBRAINZ_CONTACT", "")),
		PollInterval:        interval,
		SpotifyPollInterval: spotifyInterval,
		TrustProxy:          strings.EqualFold(env("TRUST_PROXY", "false"), "true"),
		SpotifyClientID:     strings.TrimSpace(env("SPOTIFY_CLIENT_ID", "")),
		SpotifySecret:       secret("SPOTIFY_CLIENT_SECRET"),
		SpotifyMarket:       strings.ToUpper(strings.TrimSpace(env("SPOTIFY_MARKET", "US"))),
	}
	cfg.LogLevel, err = parseLogLevel(env("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
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

func secret(name string) string {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("read %s_FILE: %v", name, err))
		}
		return strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(os.Getenv(name))
}
