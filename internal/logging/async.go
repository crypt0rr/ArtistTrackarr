package logging

import (
	"context"
	"sync/atomic"
	"time"
)

// AsyncSink decouples application-log persistence from the caller that emits
// the log record. Enqueue is deliberately bounded and non-blocking: logging
// must never hold up an HTTP request or a provider transaction when SQLite is
// busy. Records that arrive after the buffer is full are retained in the
// in-memory ring by Handler and counted as dropped here.
type AsyncSink struct {
	queue chan Entry
	sink  func(Entry) error
	stop  chan struct{}
	done  chan struct{}

	closed  atomic.Bool
	dropped atomic.Uint64
	errors  atomic.Uint64
	// lastLossAt is the unix-nano instant of the most recent drop or write
	// failure. The counters are cumulative for the process lifetime, so the
	// operational status needs to know whether loss is still happening rather
	// than whether it ever happened.
	lastLossAt atomic.Int64
}

func NewAsyncSink(buffer int, sink func(Entry) error) *AsyncSink {
	if buffer < 1 {
		buffer = 1
	}
	result := &AsyncSink{
		queue: make(chan Entry, buffer),
		sink:  sink,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go result.run()
	return result
}

func (s *AsyncSink) run() {
	defer close(s.done)
	for {
		select {
		case entry := <-s.queue:
			s.write(entry)
		case <-s.stop:
			// The queue stays open so an Enqueue already in flight can finish
			// without racing a close. Drain what is currently buffered, then
			// report completion to the shutdown coordinator.
			for {
				select {
				case entry := <-s.queue:
					s.write(entry)
				default:
					return
				}
			}
		}
	}
}

func (s *AsyncSink) write(entry Entry) {
	if s.sink == nil {
		return
	}
	if err := s.sink(entry); err != nil {
		s.errors.Add(1)
		s.lastLossAt.Store(time.Now().UTC().UnixNano())
	}
}

// Enqueue implements the sink callback expected by logging.Handler.
func (s *AsyncSink) Enqueue(entry Entry) {
	if s.closed.Load() {
		return
	}
	select {
	case s.queue <- entry:
		if s.closed.Load() {
			// Close may have won the race after the non-blocking send. The
			// writer drains entries already queued, but this one may arrive
			// after it has finished; count it as dropped for observability.
			s.dropped.Add(1)
			s.lastLossAt.Store(time.Now().UTC().UnixNano())
		}
	default:
		s.dropped.Add(1)
		s.lastLossAt.Store(time.Now().UTC().UnixNano())
	}
}

// Close stops accepting records, drains the queued records, and waits for the
// writer. A timeout leaves the writer running; callers must not close its
// underlying resource until Done reports completion.
func (s *AsyncSink) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.closed.CompareAndSwap(false, true) {
		close(s.stop)
	}

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *AsyncSink) Done() <-chan struct{} { return s.done }

func (s *AsyncSink) Dropped() uint64 { return s.dropped.Load() }

func (s *AsyncSink) Errors() uint64 { return s.errors.Load() }

// LastLossAt reports when a record was most recently dropped or failed to
// persist, or the zero time if neither has happened.
func (s *AsyncSink) LastLossAt() time.Time {
	nanos := s.lastLossAt.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}
