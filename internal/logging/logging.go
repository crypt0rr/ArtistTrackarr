package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Field struct {
	Key   string
	Value string
}

type Entry struct {
	Time       time.Time
	Level      string
	Message    string
	Attributes []Field
}

type Handler struct {
	next   slog.Handler
	buffer *ring
	attrs  []slog.Attr
	sink   *sinkState
}

type sinkState struct {
	mu sync.RWMutex
	fn func(Entry)
}

type ring struct {
	mu      sync.RWMutex
	entries []Entry
	limit   int
}

func NewHandler(next slog.Handler, limit int) *Handler {
	if limit < 1 {
		limit = 200
	}
	return &Handler{next: next, buffer: &ring{limit: limit}, sink: &sinkState{}}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	// Redact before the record reaches the downstream handler. The downstream
	// handler is what writes to stdout, so redacting afterwards would hide a
	// credential from the operator's own diagnostics while still shipping it to
	// whatever collects container logs.
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, redactAttr(attr))
		return true
	})
	safe := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	safe.AddAttrs(attrs...)
	if err := h.next.Handle(ctx, safe); err != nil {
		return err
	}
	if record.Level >= slog.LevelInfo {
		fields := make([]Field, 0, len(h.attrs)+len(attrs))
		// h.attrs were redacted when the child handler was built.
		for _, attr := range h.attrs {
			appendField(&fields, attr)
		}
		for _, attr := range attrs {
			appendField(&fields, attr)
		}
		entry := Entry{
			Time:       record.Time,
			Level:      record.Level.String(),
			Message:    record.Message,
			Attributes: fields,
		}
		h.buffer.add(entry)
		var sink func(Entry)
		if h.sink != nil {
			h.sink.mu.RLock()
			sink = h.sink.fn
			h.sink.mu.RUnlock()
		}
		if sink != nil {
			sink(entry)
		}
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Persistent attributes reach every later record, so redact them once here
	// rather than on each Handle call.
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		redacted = append(redacted, redactAttr(attr))
	}
	child := &Handler{next: h.next.WithAttrs(redacted), buffer: h.buffer, attrs: append(append([]slog.Attr{}, h.attrs...), redacted...), sink: h.sink}
	return child
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), buffer: h.buffer, attrs: h.attrs, sink: h.sink}
}

func (h *Handler) SetSink(sink func(Entry)) {
	if h.sink == nil {
		h.sink = &sinkState{}
	}
	h.sink.mu.Lock()
	h.sink.fn = sink
	h.sink.mu.Unlock()
}

func (h *Handler) Snapshot() []Entry {
	return h.buffer.snapshot()
}

// Redacted is the placeholder substituted for a sensitive attribute value. The
// key is preserved so an operator can still see that a field was present.
const Redacted = "[redacted]"

// redactAttr replaces sensitive values, recursing into groups. This is the one
// place redaction happens, so the downstream handler, the in-memory buffer, and
// the persisted sink cannot disagree about what is safe to record.
func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		nested := make([]slog.Attr, 0, len(group))
		for _, item := range group {
			nested = append(nested, redactAttr(item))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(nested...)}
	}
	if sensitiveKey(strings.TrimSpace(attr.Key)) {
		return slog.Attr{Key: attr.Key, Value: slog.StringValue(Redacted)}
	}
	return attr
}

// appendField formats an already-redacted attribute for the in-memory entry.
func appendField(fields *[]Field, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := strings.TrimSpace(attr.Key)
	if key == "" {
		return
	}
	*fields = append(*fields, Field{Key: key, Value: fmt.Sprint(attr.Value)})
}

// safeDiagnosticKeys are attribute keys that trip the substring net below but
// carry nothing secret by construction. They are matched EXACTLY: a substring
// allowlist would reopen the hole the net exists to close, so "auth_tokens" is
// exempt while "auth_token" is not.
//
// Every entry needs a reason to be here, because each one is a hole:
//
//	token_fingerprint - a truncated SHA-256 prefix, not reversible to the token.
//	    It exists so an operator can correlate a failing invite or reset link
//	    without ever seeing the token; redacting it removes the only thing it
//	    was added for.
//	auth_tokens       - an integer row count reported by the retention sweep and
//	    the maintenance job. Rendering a count as [redacted] reads as though a
//	    leak was prevented, which teaches an operator to distrust the marker.
//	public_url        - the operator-configured public base URL. It is public by
//	    definition and is the one startup line confirming PUBLIC_URL parsed as
//	    intended, which is the most common source of broken notification and
//	    calendar links.
var safeDiagnosticKeys = map[string]struct{}{
	"token_fingerprint": {},
	"auth_tokens":       {},
	"public_url":        {},
}

// sensitiveKey reports whether an attribute value must be destroyed before it
// reaches stdout, the in-memory ring, or the application_logs table. The
// substring net is deliberately broad because it must also cover keys nobody
// anticipated; safeDiagnosticKeys carves out the few that the application emits
// on purpose as operator signal.
func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if _, safe := safeDiagnosticKeys[key]; safe {
		return false
	}
	for _, part := range []string{"password", "secret", "token", "credential", "encrypted", "url", "body"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func (r *ring) add(entry Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	if len(r.entries) > r.limit {
		r.entries = append([]Entry(nil), r.entries[len(r.entries)-r.limit:]...)
	}
}

func (r *ring) snapshot() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]Entry, len(r.entries))
	for i := range r.entries {
		entries[len(r.entries)-1-i] = r.entries[i]
	}
	return entries
}
