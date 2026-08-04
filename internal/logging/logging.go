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
	if err := h.next.Handle(ctx, record); err != nil {
		return err
	}
	if record.Level >= slog.LevelInfo {
		fields := make([]Field, 0, len(h.attrs)+record.NumAttrs())
		for _, attr := range h.attrs {
			appendField(&fields, attr)
		}
		record.Attrs(func(attr slog.Attr) bool {
			appendField(&fields, attr)
			return true
		})
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
	child := &Handler{next: h.next.WithAttrs(attrs), buffer: h.buffer, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...), sink: h.sink}
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

func appendField(fields *[]Field, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := strings.TrimSpace(attr.Key)
	if key == "" || sensitiveKey(key) {
		return
	}
	*fields = append(*fields, Field{Key: key, Value: fmt.Sprint(attr.Value)})
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
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
