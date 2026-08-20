package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

// lifecycleSender is the small part of the notification sender needed during
// shutdown. Keeping it as an interface makes the server lifecycle testable
// without constructing a transport or making network requests.
type lifecycleSender interface {
	CloseIdleConnections()
}

type serverLifecycleDeps struct {
	server    *http.Server
	listener  net.Listener
	runner    *jobs.Runner
	logSink   *logging.AsyncSink
	database  *store.Store
	sender    lifecycleSender
	logger    *slog.Logger
	stdoutLog *slog.Logger
	publicURL string
}

// runServerLifecycle owns the HTTP server, scheduler, application-log sink,
// and database shutdown order. It returns whether the database was closed
// safely; callers must leave it open when a worker or log sink misses its
// deadline so no connection can be closed underneath active work.
func runServerLifecycle(parent context.Context, deps serverLifecycleDeps) (bool, error) {
	if parent == nil {
		parent = context.Background()
	}
	if deps.server == nil || deps.listener == nil || deps.runner == nil || deps.logSink == nil || deps.database == nil {
		return false, errors.New("incomplete server lifecycle dependencies")
	}
	if deps.logger == nil {
		deps.logger = slog.Default()
	}
	if deps.stdoutLog == nil {
		deps.stdoutLog = deps.logger
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go deps.runner.Run(ctx)

	serverDone := make(chan error, 1)
	go func() {
		deps.logger.Info("server listening", "address", deps.listener.Addr().String(), "public_url", deps.publicURL)
		serverDone <- deps.server.Serve(deps.listener)
	}()

	var serveErr error
	serverDoneConsumed := false
	serverStopped := false
	select {
	case serveErr = <-serverDone:
		serverDoneConsumed = true
		serverStopped = true
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			deps.logger.Error("http server failed", "error", serveErr)
			cancel()
		}
	case <-ctx.Done():
	}

	// HTTP shutdown and background work have independent budgets. A slow
	// client must not consume the entire runner shutdown window (and vice
	// versa).
	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 20*time.Second)
	shutdownErr := deps.server.Shutdown(httpShutdownCtx)
	cancelHTTP()
	if shutdownErr != nil {
		deps.logger.Error("graceful shutdown failed", "error", shutdownErr)
	}

	if !serverDoneConsumed {
		select {
		case err := <-serverDone:
			if serveErr == nil {
				serveErr = err
			}
			serverStopped = true
		case <-time.After(20 * time.Second):
			deps.stdoutLog.Error("http server did not stop before shutdown deadline")
		}
	}

	runnerStopped := false
	runnerShutdownCtx, cancelRunner := context.WithTimeout(context.Background(), 20*time.Second)
	select {
	case <-deps.runner.Done():
		runnerStopped = true
	case <-runnerShutdownCtx.Done():
		deps.stdoutLog.Error("background runner did not stop before shutdown deadline")
	}
	cancelRunner()

	logDrainCtx, cancelLogDrain := context.WithTimeout(context.Background(), 5*time.Second)
	logErr := deps.logSink.Close(logDrainCtx)
	cancelLogDrain()
	if logErr != nil {
		deps.stdoutLog.Error("application log sink did not drain before database shutdown", "error", logErr)
	}
	if !serverStopped || !runnerStopped || logErr != nil || shutdownErr != nil {
		// Do not close SQLite while the runner or log writer can still use it.
		reason := fmt.Sprintf("server lifecycle did not drain (server_stopped=%t runner_stopped=%t)", serverStopped, runnerStopped)
		if logErr != nil {
			return false, fmt.Errorf("%s: application log sink: %w", reason, logErr)
		}
		if shutdownErr != nil {
			return false, fmt.Errorf("%s: HTTP shutdown: %w", reason, shutdownErr)
		}
		return false, errors.New(reason)
	}

	if deps.sender != nil {
		deps.sender.CloseIdleConnections()
	}
	if dropped := deps.logSink.Dropped(); dropped > 0 {
		deps.stdoutLog.Warn("application log records dropped", "count", dropped)
	}
	if sinkErrors := deps.logSink.Errors(); sinkErrors > 0 {
		deps.stdoutLog.Warn("application log persistence failed", "count", sinkErrors)
	}
	if err := deps.database.Close(); err != nil {
		return false, fmt.Errorf("close database: %w", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return true, serveErr
	}
	deps.stdoutLog.Info("server stopped")
	return true, nil
}
