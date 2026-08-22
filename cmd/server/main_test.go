package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type lifecycleCatalog struct{}

func (lifecycleCatalog) SearchArtists(context.Context, string, int) ([]catalog.ArtistResult, error) {
	return nil, nil
}
func (lifecycleCatalog) ResolveArtist(context.Context, string) (catalog.ArtistResult, error) {
	return catalog.ArtistResult{}, nil
}
func (lifecycleCatalog) ResolveExternalArtist(context.Context, string) ([]catalog.ArtistResult, error) {
	return nil, nil
}
func (lifecycleCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	return nil, nil
}

type lifecycleSenderStub struct{}

func (lifecycleSenderStub) CloseIdleConnections() {}

type failingListener struct {
	err error
	url net.Addr
}

func TestBindHTTPListenerPropagatesFailure(t *testing.T) {
	want := errors.New("address already in use")
	previous := listenTCP
	listenTCP = func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:0" {
			t.Fatalf("listen arguments network=%q address=%q", network, address)
		}
		return nil, want
	}
	t.Cleanup(func() { listenTCP = previous })

	listener, err := bindHTTPListener("127.0.0.1:0")
	if listener != nil {
		t.Fatal("bind returned a listener after failure")
	}
	if !errors.Is(err, want) {
		t.Fatalf("bind error=%v, want %v", err, want)
	}
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr            { return l.url }

func newLifecycleDeps(t *testing.T, database *store.Store, listener net.Listener) serverLifecycleDeps {
	t.Helper()
	sink := logging.NewAsyncSink(4, func(entry logging.Entry) error {
		return database.InsertApplicationLog(context.Background(), entry)
	})
	runner := jobs.New(database, lifecycleCatalog{}, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return serverLifecycleDeps{
		server:    &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
		listener:  listener,
		runner:    runner,
		logSink:   sink,
		database:  database,
		sender:    lifecycleSenderStub{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stdoutLog: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestDrainStartupResourcesPersistsLogsBeforeDatabaseClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artist-trackarr.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sink := logging.NewAsyncSink(4, func(entry logging.Entry) error {
		return database.InsertApplicationLog(context.Background(), entry)
	})
	sink.Enqueue(logging.Entry{
		Time:    time.Now().UTC(),
		Level:   "INFO",
		Message: "startup cleanup test",
	})

	if err := drainStartupResources(sink, database); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var count int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM application_logs WHERE message=?`, "startup cleanup test").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted startup log count=%d, want 1", count)
	}
}

func TestRunServerLifecycleCancelsAndClosesInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artist-trackarr.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deps := newLifecycleDeps(t, database, listener)
	deps.logSink.Enqueue(logging.Entry{Time: time.Now().UTC(), Level: "INFO", Message: "lifecycle ordering"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()

	closed, err := runServerLifecycle(ctx, deps)
	if err != nil || !closed {
		t.Fatalf("lifecycle result closed=%v err=%v", closed, err)
	}
	if err := database.DB.Ping(); err == nil {
		t.Fatal("database remained usable after lifecycle reported it closed")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var persisted int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM application_logs WHERE message=?`, "lifecycle ordering").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 1 {
		t.Fatalf("lifecycle log count=%d, want 1", persisted)
	}
}

func TestRunServerLifecycleReturnsServeFailureAfterDraining(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "artist-trackarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	serveErr := errors.New("listener failed")
	deps := newLifecycleDeps(t, database, failingListener{err: serveErr, url: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}})
	closed, err := runServerLifecycle(context.Background(), deps)
	if !closed || !errors.Is(err, serveErr) {
		t.Fatalf("serve failure result closed=%v err=%v", closed, err)
	}
	if err := database.DB.Ping(); err == nil {
		t.Fatal("database remained usable after serve failure cleanup")
	}
}

func TestShutdownBudgetFitsTheContainerGracePeriod(t *testing.T) {
	// The shutdown stages run in sequence, so their sum is the worst case. If it
	// exceeds the container stop grace period the process is SIGKILLed part-way
	// through the log drain and the SQLite close that follows, which is the
	// unsafe shutdown these budgets exist to prevent.
	if shutdownBudget > shutdownGracePeriod {
		t.Fatalf("shutdown budget %s exceeds the container grace period %s", shutdownBudget, shutdownGracePeriod)
	}
	// The grace period constant must keep matching compose.yaml, or this check
	// silently stops meaning anything.
	compose, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("stop_grace_period: %ds", int(shutdownGracePeriod.Seconds()))
	if !strings.Contains(string(compose), want) {
		t.Fatalf("compose.yaml does not declare %q", want)
	}
}
