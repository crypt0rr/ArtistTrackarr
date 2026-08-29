package web

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

const (
	// The Compose healthcheck has a five-second timeout. Keep the shared probe
	// below that budget so an external caller cannot leave an in-flight writer
	// check around longer than the orchestrator is willing to wait.
	readinessProbeTimeout = 4 * time.Second
	// A health result is useful to a burst of callers, but should not hide a
	// database transition for long. Failures are cached too, so a full/read-only
	// database does not receive a write attempt for every unauthenticated probe.
	readinessCacheTTL = 5 * time.Second
)

// readinessProbe serializes the expensive part of an unauthenticated readiness
// check and shares its result with concurrent callers. The store itself remains
// behind this small web-layer gate because /readyz is a public orchestration
// endpoint while Store.Ready is also used by authenticated diagnostics.
type readinessProbe struct {
	mu        sync.Mutex
	checkedAt time.Time
	lastError error
	inFlight  bool
	completed chan struct{}
}

func (p *readinessProbe) check(ctx context.Context, probe func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if probe == nil {
		return errors.New("readiness probe is unavailable")
	}

	for {
		now := time.Now()
		p.mu.Lock()
		if !p.checkedAt.IsZero() && now.Sub(p.checkedAt) < readinessCacheTTL {
			err := p.lastError
			p.mu.Unlock()
			return err
		}
		if p.inFlight {
			completed := p.completed
			p.mu.Unlock()
			select {
			case <-completed:
				// Re-check the cache under the lock. The probe may have failed
				// or the cache may have expired while this caller was waiting.
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		p.inFlight = true
		p.completed = make(chan struct{})
		completed := p.completed
		p.mu.Unlock()

		probeCtx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		err := probe(probeCtx)
		cancel()

		p.mu.Lock()
		p.checkedAt = time.Now()
		p.lastError = err
		p.inFlight = false
		close(completed)
		p.mu.Unlock()
		return err
	}
}

func (a *App) readyCheck(ctx context.Context) error {
	if a == nil || a.store == nil {
		return &store.DatabaseHealthError{State: store.DatabaseUnavailable, Err: errors.New("database handle is unavailable")}
	}
	return a.readyProbe.check(ctx, a.store.Ready)
}
