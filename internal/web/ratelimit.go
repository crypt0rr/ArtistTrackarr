package web

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// fixedWindowLimiter is deliberately process-local. ArtistTrackarr supports a
// single application replica, so keeping abuse controls in memory avoids a new
// persistence table while still putting a hard bound on expensive endpoints.
type fixedWindowLimiter struct {
	mu         sync.Mutex
	entries    map[string]windowEntry
	limit      int
	window     time.Duration
	maxEntries int
}

type windowEntry struct {
	started time.Time
	count   int
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		entries:    make(map[string]windowEntry),
		limit:      limit,
		window:     window,
		maxEntries: 4096,
	}
}

func (l *fixedWindowLimiter) Allow(key string) bool {
	if l == nil || l.limit < 1 || l.window <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for existingKey, entry := range l.entries {
		if now.Sub(entry.started) >= l.window {
			delete(l.entries, existingKey)
		}
	}
	entry, ok := l.entries[key]
	if !ok || now.Sub(entry.started) >= l.window {
		if !ok && len(l.entries) >= l.maxEntries {
			// Keep abuse controls bounded even when an attacker presents many
			// distinct source addresses inside one window. Evict the oldest
			// bucket; the request remains subject to the fixed global cap.
			var oldestKey string
			var oldest time.Time
			for candidateKey, candidate := range l.entries {
				if oldestKey == "" || candidate.started.Before(oldest) {
					oldestKey, oldest = candidateKey, candidate.started
				}
			}
			if oldestKey != "" {
				delete(l.entries, oldestKey)
			}
		}
		l.entries[key] = windowEntry{started: now, count: 1}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

func rateLimited(w http.ResponseWriter, retryAfter int, message string) {
	w.Header().Set("Retry-After", formatRetryAfter(retryAfter))
	http.Error(w, message, http.StatusTooManyRequests)
}

func formatRetryAfter(seconds int) string {
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

func (a *App) allowProviderAction(w http.ResponseWriter, r *http.Request) bool {
	session, ok := currentSession(r)
	if !ok || a.providerLimiter == nil {
		return true
	}
	key := strconv.FormatInt(session.User.ID, 10) + "|" + a.clientIP(r)
	if a.providerLimiter.Allow(key) {
		return true
	}
	rateLimited(w, 600, "provider requests are temporarily rate limited; try again later")
	return false
}
