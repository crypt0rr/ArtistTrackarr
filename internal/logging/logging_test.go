package logging

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

func TestHandlerKeepsBoundedRedactedSnapshot(t *testing.T) {
	h := NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}), 2)
	logger := slog.New(h)
	logger.Info("first", "count", 1, "secret", "hidden")
	logger.Warn("second")
	logger.Error("third", "destination_url", "https://example.test/private")
	entries := h.Snapshot()
	if len(entries) != 2 || entries[0].Message != "third" || entries[1].Message != "second" {
		t.Fatalf("unexpected snapshot: %#v", entries)
	}
	if len(entries[1].Attributes) != 0 {
		t.Fatalf("sensitive attributes were retained: %#v", entries[1].Attributes)
	}
	if len(entries[0].Attributes) != 0 {
		t.Fatalf("unexpected second attributes: %#v", entries[0].Attributes)
	}
}

func TestHandlerAttributesGroupsAndSink(t *testing.T) {
	h := NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}), 4)
	var calls atomic.Int32
	h.SetSink(func(Entry) { calls.Add(1) })
	child := h.WithAttrs([]slog.Attr{slog.String("component", "tests")}).WithGroup("context")
	logger := slog.New(child)
	logger.Info("grouped event")
	if calls.Load() != 1 {
		t.Fatalf("sink calls=%d, want 1", calls.Load())
	}
	if entries := h.Snapshot(); len(entries) != 1 || entries[0].Message != "grouped event" {
		t.Fatalf("snapshot=%#v", entries)
	}

	sink := NewAsyncSink(1, nil)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.Done():
	default:
		t.Fatal("async sink did not signal Done after close")
	}
}
