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

// TestDeliberateDiagnosticFieldsSurviveRedaction pins the three fields the
// application emits specifically as operator signal. The redaction net matches
// key SUBSTRINGS, so "token_fingerprint", "auth_tokens" and "public_url" were
// all destroyed - in stdout, in the in-memory ring, and in the application_logs
// row the admin page renders. The fingerprint is a truncated SHA-256 prefix
// added precisely so an operator can correlate a failing invite link without
// seeing the token; redacting it removed the only thing it was for, and a row
// count rendered as [redacted] reads as though a leak was prevented.
func TestDeliberateDiagnosticFieldsSurviveRedaction(t *testing.T) {
	for _, key := range []string{"token_fingerprint", "auth_tokens", "public_url"} {
		if sensitiveKey(key) {
			t.Errorf("%q is redacted, so the operator signal it exists to carry never arrives", key)
		}
	}
}

// TestRedactionAllowlistIsExactNotSubstring keeps the carve-out from reopening
// the hole the net exists to close. Every key below is a near miss of an
// allowlisted one and must still be destroyed.
func TestRedactionAllowlistIsExactNotSubstring(t *testing.T) {
	for _, key := range []string{
		"token", "auth_token", "session_token", "api_token",
		"public_url_secret", "public_urls", "url", "callback_url",
		"password", "client_secret", "encrypted_url", "body",
		"TOKEN_FINGERPRINT_RAW", "x-auth-tokens-header",
	} {
		if !sensitiveKey(key) {
			t.Errorf("%q is not redacted; the allowlist is matching too loosely", key)
		}
	}
	// Case and surrounding space must not smuggle a value past the net either.
	for _, key := range []string{"  Token  ", "SECRET", " Password "} {
		if !sensitiveKey(key) {
			t.Errorf("%q is not redacted", key)
		}
	}
}

// TestAllowlistedKeysReachTheHandlerIntact drives the real handler rather than
// the predicate, because redactAttr runs in both Handle and WithAttrs and the
// value has to survive both paths.
func TestAllowlistedKeysReachTheHandlerIntact(t *testing.T) {
	var buf bytes.Buffer
	handler := NewHandler(slog.NewJSONHandler(&buf, nil), 16)
	logger := slog.New(handler)
	logger.Info("server listening", "public_url", "https://tracker.example", "token_fingerprint", "a1b2c3d4e5f6")
	logger.With("auth_tokens", 42).Info("retention cleanup completed")

	out := buf.String()
	for _, want := range []string{"https://tracker.example", "a1b2c3d4e5f6", "42"} {
		if !strings.Contains(out, want) {
			t.Fatalf("value %q did not survive redaction: %s", want, out)
		}
	}
	if strings.Contains(out, "[redacted]") {
		t.Fatalf("a deliberately safe field was redacted: %s", out)
	}
	// And the ring the admin page reads must agree with stdout.
	for _, entry := range handler.Snapshot() {
		for _, f := range entry.Attributes {
			if f.Value == "[redacted]" {
				t.Fatalf("field %q redacted in the ring but not in stdout", f.Key)
			}
		}
	}
}

// TestRecordsLoggedBeforeTheSinkExistsArePersisted covers the startup ordering.
// The sink writes to SQLite, so it cannot be attached until the database is
// open — but the process legitimately logs before that, including the two
// "security explicitly weakened" warnings for AllowInsecureHTTP and
// AllowPrivateNotificationTargets. Those reached stdout and the in-memory ring
// but were never enqueued, so application_logs held no record of them; and the
// admin page only falls back to the ring when the table returns zero rows,
// which never happens on a running install. Neither flag is surfaced anywhere
// else in the UI, so there was no in-app evidence at all.
func TestRecordsLoggedBeforeTheSinkExistsArePersisted(t *testing.T) {
	handler := NewHandler(slog.NewJSONHandler(io.Discard, nil), 16)
	logger := slog.New(handler)

	// Emitted before the database is open, exactly as cmd/server/main.go does.
	logger.Warn("insecure HTTP public URL explicitly enabled")
	logger.Warn("private notification targets explicitly enabled")

	var persisted []Entry
	handler.SetSink(func(e Entry) { persisted = append(persisted, e) })

	if len(persisted) != 2 {
		t.Fatalf("persisted %d pre-sink records, want 2: %+v", len(persisted), persisted)
	}
	if persisted[0].Message != "insecure HTTP public URL explicitly enabled" ||
		persisted[1].Message != "private notification targets explicitly enabled" {
		t.Fatalf("pre-sink records replayed out of order: %+v", persisted)
	}

	// Records after attachment still go straight through, exactly once.
	logger.Info("server listening")
	if len(persisted) != 3 || persisted[2].Message != "server listening" {
		t.Fatalf("post-sink record not persisted once: %+v", persisted)
	}
}

// TestSinkReplayHappensOnlyOnce keeps the backlog from being re-delivered if a
// sink is swapped or removed later; a duplicate would show as a phantom repeat
// of a startup warning in the admin log.
func TestSinkReplayHappensOnlyOnce(t *testing.T) {
	handler := NewHandler(slog.NewJSONHandler(io.Discard, nil), 16)
	logger := slog.New(handler)
	logger.Warn("early record")

	var first, second []Entry
	handler.SetSink(func(e Entry) { first = append(first, e) })
	handler.SetSink(nil)
	logger.Warn("while detached")
	handler.SetSink(func(e Entry) { second = append(second, e) })

	if len(first) != 1 {
		t.Fatalf("first sink got %d records, want 1", len(first))
	}
	if len(second) != 0 {
		t.Fatalf("second sink replayed %d records that were already delivered: %+v", len(second), second)
	}
}

// TestPreSinkBacklogIsBounded keeps a sink that never arrives from growing an
// unbounded buffer.
func TestPreSinkBacklogIsBounded(t *testing.T) {
	handler := NewHandler(slog.NewJSONHandler(io.Discard, nil), 16)
	logger := slog.New(handler)
	for i := 0; i < pendingSinkLimit+50; i++ {
		logger.Info("early")
	}
	var persisted int
	handler.SetSink(func(Entry) { persisted++ })
	if persisted != pendingSinkLimit {
		t.Fatalf("replayed %d records, want the %d-record cap", persisted, pendingSinkLimit)
	}
}
