package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

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
