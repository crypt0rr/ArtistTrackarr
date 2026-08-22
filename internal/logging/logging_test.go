package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
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
		t.Fatalf("unexpected attributes on an attribute-free record: %#v", entries[1].Attributes)
	}
	// The key survives so an operator can see a field was present; the value
	// does not.
	if len(entries[0].Attributes) != 1 || entries[0].Attributes[0].Key != "destination_url" ||
		entries[0].Attributes[0].Value != Redacted {
		t.Fatalf("sensitive attribute was not redacted: %#v", entries[0].Attributes)
	}
}

func TestHandlerRedactsBeforeTheDownstreamHandler(t *testing.T) {
	// The downstream handler is what writes to stdout. Assert on the bytes it
	// actually receives, not on Snapshot(): redaction that only shapes the
	// in-memory view hides a credential from the operator while still shipping
	// it to whatever collects container logs.
	var out bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}), 8)
	logger := slog.New(h.WithAttrs([]slog.Attr{slog.String("session_token", "persistent-attr-secret")}))
	logger.Info("delivery attempted",
		"destination_url", "https://hooks.example.test/T000/B000/xoxb-real-credential",
		"password", "hunter2",
		"api_secret", "sk-live-abcdef",
		"nested", slog.GroupValue(slog.String("bearer_token", "grouped-secret"), slog.String("artist_id", "42")),
		"artist_id", 42,
	)
	written := out.String()
	for _, leaked := range []string{
		"xoxb-real-credential", "hunter2", "sk-live-abcdef",
		"grouped-secret", "persistent-attr-secret",
	} {
		if strings.Contains(written, leaked) {
			t.Fatalf("credential %q reached the downstream handler: %s", leaked, written)
		}
	}
	// Non-sensitive attributes and the message must survive intact.
	if !strings.Contains(written, `"artist_id":42`) || !strings.Contains(written, "delivery attempted") {
		t.Fatalf("redaction damaged safe output: %s", written)
	}
	if !strings.Contains(written, Redacted) {
		t.Fatalf("no redaction placeholder in output: %s", written)
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
