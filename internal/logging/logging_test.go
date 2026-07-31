package logging

import (
	"io"
	"log/slog"
	"testing"
)

func TestHandlerKeepsBoundedRedactedSnapshot(t *testing.T) {
	h := NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}), 2)
	logger := slog.New(h)
	logger.Info("first", "count", 1, "secret", "hidden")
	logger.Warn("second")
	logger.Error("third", "destination_url", "https://example.test/private")
	entries := h.Snapshot()
	if len(entries) != 2 || entries[0].Message != "second" || entries[1].Message != "third" {
		t.Fatalf("unexpected snapshot: %#v", entries)
	}
	if len(entries[1].Attributes) != 0 {
		t.Fatalf("sensitive attributes were retained: %#v", entries[1].Attributes)
	}
	if len(entries[0].Attributes) != 0 {
		t.Fatalf("unexpected second attributes: %#v", entries[0].Attributes)
	}
}
