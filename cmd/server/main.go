package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// drainStartupResources persists the queued application records before the
// database is closed. If draining times out, the caller must leave the
// database open because the sink writer may still be using it.
func drainStartupResources(sink *logging.AsyncSink, database *store.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		return fmt.Errorf("drain application log sink: %w", err)
	}
	if err := database.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

func main() {
	stdoutLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		stdoutLogger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	stdoutLogger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logHandler := logging.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}), 200)
	logger := slog.New(logHandler)
	if cfg.AllowInsecureHTTP {
		logger.Warn("insecure HTTP public URL explicitly enabled")
	}
	if cfg.AllowPrivateNotificationTargets {
		logger.Warn("private notification targets explicitly enabled")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	database.SetProviderHealthCadences(cfg.PollInterval, cfg.SpotifyPollInterval)
	databaseClosed := false
	defer func() {
		if !databaseClosed {
			_ = database.Close()
		}
	}()
	applicationLogs := logging.NewAsyncSink(256, func(entry logging.Entry) error {
		return database.InsertApplicationLog(context.Background(), entry)
	})
	logHandler.SetSink(applicationLogs.Enqueue)
	startupFailure := func() {
		if err := drainStartupResources(applicationLogs, database); err != nil {
			// The sink may still be using SQLite when its drain timed out. Do not
			// close the database in that case; process exit will reclaim it safely.
			stdoutLogger.Error("startup cleanup incomplete", "error", err)
		}
		databaseClosed = true
		os.Exit(1)
	}

	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		logger.Error("create credential cipher", "error", err)
		startupFailure()
	}
	if err := database.ValidateDestinationCiphertexts(context.Background(), cipher.Decrypt); err != nil {
		logger.Error("validate encrypted notification destinations", "error", err)
		startupFailure()
	}
	artworkCache, err := artwork.NewCache(filepath.Join(filepath.Dir(cfg.DatabasePath), "covers"))
	if err != nil {
		logger.Error("initialize artwork cache", "error", err)
		startupFailure()
	}
	musicBrainz := catalog.NewMusicBrainz(cfg.MusicBrainzContact)
	spotify := catalog.NewSpotify(cfg.SpotifyClientID, cfg.SpotifySecret, cfg.SpotifyMarket)
	itunes := catalog.NewITunes(cfg.ITunesMarket)
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
	sender := notify.NewShoutrrrSender(cfg.AllowPrivateNotificationTargets, notify.DefaultSendTimeout)
	var runnerOptions []jobs.Option
	if spotify != nil {
		runnerOptions = append(runnerOptions, jobs.WithSpotify(spotify))
		runnerOptions = append(runnerOptions, jobs.WithSpotifyInterval(cfg.SpotifyPollInterval))
	}
	runnerOptions = append(runnerOptions, jobs.WithITunes(itunes))
	runnerOptions = append(runnerOptions, jobs.WithListenBrainz(listenBrainz))
	runnerOptions = append(runnerOptions, jobs.WithArtworkCache(artworkCache))
	runner := jobs.New(database, musicBrainz, catalog.AlbumEPNormalizer{}, sender, cipher, cfg.PollInterval, logger,
		runnerOptions...)
	app, err := appweb.New(cfg, database, musicBrainz, spotifyProvider, sender, cipher, artworkCache, runner, logger, itunes)
	if err != nil {
		logger.Error("initialize web application", "error", err)
		startupFailure()
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	// Bind before announcing readiness. A failed bind is a startup failure,
	// not a background serve error, and must never make the container appear
	// healthy while no listener exists.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("bind HTTP listener failed", "address", cfg.ListenAddr, "error", err)
		startupFailure()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runner.Run(ctx)
	serverDone := make(chan error, 1)
	go func() {
		logger.Info("server listening", "address", listener.Addr().String(), "public_url", cfg.PublicURL.String())
		serveErr := server.Serve(listener)
		serverDone <- serveErr
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("http server failed", "error", serveErr)
			stop()
		}
	}()
	<-ctx.Done()
	// HTTP and background work have independent budgets. A slow client must
	// not consume the entire runner shutdown window (and vice versa).
	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 20*time.Second)
	if err := server.Shutdown(httpShutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	serverStopped := false
	serverFailed := false
	select {
	case serveErr := <-serverDone:
		serverStopped = true
		serverFailed = serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed)
	case <-httpShutdownCtx.Done():
		stdoutLogger.Error("http server did not stop before shutdown deadline")
	}
	cancelHTTP()
	runnerStopped := false
	runnerShutdownCtx, cancelRunner := context.WithTimeout(context.Background(), 20*time.Second)
	select {
	case <-runner.Done():
		runnerStopped = true
		cancelRunner()
	case <-runnerShutdownCtx.Done():
		cancelRunner()
	}
	logDrainCtx, cancelLogDrain := context.WithTimeout(context.Background(), 5*time.Second)
	logErr := applicationLogs.Close(logDrainCtx)
	cancelLogDrain()
	if logErr != nil || !serverStopped || !runnerStopped {
		stdoutLogger.Error("background work did not drain before database shutdown",
			"server_stopped", serverStopped, "runner_stopped", runnerStopped, "log_sink_error", logErr)
		// Do not close SQLite while a runner or log writer can still use it. The
		// process is about to exit, so the operating system will reclaim it.
		databaseClosed = true
		return
	}
	sender.CloseIdleConnections()
	if dropped := applicationLogs.Dropped(); dropped > 0 {
		stdoutLogger.Warn("application log records dropped", "count", dropped)
	}
	if sinkErrors := applicationLogs.Errors(); sinkErrors > 0 {
		stdoutLogger.Warn("application log persistence failed", "count", sinkErrors)
	}
	stdoutLogger.Info("server stopped")
	databaseClosed = true
	if err := database.Close(); err != nil {
		stdoutLogger.Error("close database", "error", err)
	}
	if serverFailed {
		os.Exit(1)
	}
}
