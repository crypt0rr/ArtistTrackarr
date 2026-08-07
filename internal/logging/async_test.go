package logging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAsyncSinkDropsWithoutBlockingAndDrains(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []Entry
	sink := NewAsyncSink(1, func(entry Entry) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		mu.Lock()
		seen = append(seen, entry)
		mu.Unlock()
		return nil
	})
	sink.Enqueue(Entry{Message: "first"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async sink did not start")
	}
	sink.Enqueue(Entry{Message: "queued"})
	sink.Enqueue(Entry{Message: "dropped"})
	if got := sink.Dropped(); got != 1 {
		t.Fatalf("dropped=%d, want 1", got)
	}
	close(release)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("drained %d entries, want 2", len(seen))
	}
}

func TestAsyncSinkCountsWriterErrors(t *testing.T) {
	sink := NewAsyncSink(2, func(Entry) error { return errors.New("write failed") })
	sink.Enqueue(Entry{Message: "one"})
	sink.Enqueue(Entry{Message: "two"})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.Errors(); got != 2 {
		t.Fatalf("errors=%d, want 2", got)
	}
}

func TestAsyncSinkCloseCanBeCanceledAndRejectsLaterEntries(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	sink := NewAsyncSink(1, func(Entry) error {
		close(started)
		<-release
		return nil
	})
	sink.Enqueue(Entry{Message: "in flight"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async sink did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled close error=%v", err)
	}
	sink.Enqueue(Entry{Message: "after close"})
	if got := sink.Dropped(); got != 0 {
		t.Fatalf("closed enqueue changed dropped count to %d", got)
	}
	close(release)
	select {
	case <-sink.Done():
	case <-time.After(time.Second):
		t.Fatal("async sink did not finish after cancellation")
	}
}
