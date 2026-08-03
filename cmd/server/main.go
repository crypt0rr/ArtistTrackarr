package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	appweb "github.com/crypt0rr/artist-tracker/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logHandler := logging.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}), 200)
	logger := slog.New(logHandler)
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	logHandler.SetSink(func(entry logging.Entry) { _ = database.InsertApplicationLog(context.Background(), entry) })

	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		logger.Error("create credential cipher", "error", err)
		os.Exit(1)
	}
	artworkCache, err := artwork.NewCache(filepath.Join(filepath.Dir(cfg.DatabasePath), "covers"))
	if err != nil {
		logger.Error("initialize artwork cache", "error", err)
		os.Exit(1)
	}
	musicBrainz := catalog.NewMusicBrainz(cfg.MusicBrainzContact)
	spotify := catalog.NewSpotify(cfg.SpotifyClientID, cfg.SpotifySecret, cfg.SpotifyMarket)
	itunes := catalog.NewITunes(cfg.SpotifyMarket)
	listenBrainz := catalog.NewListenBrainz()
	var spotifyProvider catalog.SpotifyProvider
	if spotify != nil {
		spotifyProvider = spotify
		if health, healthErr := database.ProviderHealthByName(context.Background(), "spotify"); healthErr == nil {
			if health.NextCheckAt != nil && (health.RateLimited || health.QuotaExceeded) {
				reason := "RATE_LIMITED"
				if health.QuotaExceeded {
					reason = "QUOTA_EXCEEDED"
				}
				spotify.RestoreCooldown(*health.NextCheckAt, reason, health.QuotaExceeded)
			}
		} else if !errors.Is(healthErr, sql.ErrNoRows) {
			logger.Warn("restore Spotify provider cooldown failed", "error", healthErr)
		}
	}
	if health, healthErr := database.ProviderHealthByName(context.Background(), "itunes"); healthErr == nil {
		if health.NextCheckAt != nil && health.RateLimited {
			itunes.RestoreCooldown(*health.NextCheckAt, health.LastError)
		}
	} else if !errors.Is(healthErr, sql.ErrNoRows) {
		logger.Warn("restore iTunes provider cooldown failed", "error", healthErr)
	}
	sender := notify.ShoutrrrSender{}
	var runnerOptions []jobs.Option
	if spotify != nil {
		runnerOptions = append(runnerOptions, jobs.WithSpotify(spotify))
		runnerOptions = append(runnerOptions, jobs.WithSpotifyInterval(cfg.SpotifyPollInterval))
	}
	runnerOptions = append(runnerOptions, jobs.WithITunes(itunes))
	runnerOptions = append(runnerOptions, jobs.WithListenBrainz(listenBrainz))
	runner := jobs.New(database, musicBrainz, catalog.AlbumEPNormalizer{}, sender, cipher, cfg.PollInterval, logger,
		runnerOptions...)
	app, err := appweb.New(cfg, database, musicBrainz, spotifyProvider, sender, cipher, artworkCache, runner, logger, itunes)
	if err != nil {
		logger.Error("initialize web application", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runner.Run(ctx)
	go func() {
		logger.Info("server listening", "address", cfg.ListenAddr, "public_url", cfg.PublicURL.String())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	logger.Info("server stopped")
}
