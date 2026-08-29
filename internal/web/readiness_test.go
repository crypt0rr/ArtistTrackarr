package web

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessProbeSerializesAndCachesConcurrentChecks(t *testing.T) {
	var probe readinessProbe
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	check := func(context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	}

	first := make(chan error, 1)
	go func() { first <- probe.check(context.Background(), check) }()
	<-started

	const waiters = 32
	results := make(chan error, waiters)
	var workers sync.WaitGroup
	workers.Add(waiters)
	for range waiters {
		go func() {
			defer workers.Done()
			results <- probe.check(context.Background(), check)
		}()
	}

	// Every waiter must observe the same in-flight check rather than starting
	// another writer transaction while the first one is still active.
	time.Sleep(25 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent readiness checks=%d, want 1", got)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first readiness check=%v", err)
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("waiter readiness check=%v", err)
		}
	}

	// A follow-up request inside the freshness window must use the cached
	// result, including when many callers arrive at once.
	for range waiters {
		if err := probe.check(context.Background(), check); err != nil {
			t.Fatalf("cached readiness check=%v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached readiness checks=%d, want 1", got)
	}
}

func TestReadinessProbeCachesFailures(t *testing.T) {
	var probe readinessProbe
	var calls atomic.Int32
	want := errors.New("database unavailable")
	check := func(context.Context) error {
		calls.Add(1)
		return want
	}

	for range 4 {
		if err := probe.check(context.Background(), check); !errors.Is(err, want) {
			t.Fatalf("readiness error=%v, want %v", err, want)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed readiness checks=%d, want 1 within cache window", got)
	}
}

func TestReadinessProbeWaiterHonorsContext(t *testing.T) {
	var probe readinessProbe
	started := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- probe.check(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { canceled <- probe.check(ctx, func(context.Context) error { return nil }) }()
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error=%v, want context canceled", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first readiness check=%v", err)
	}
}
