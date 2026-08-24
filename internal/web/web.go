package web

import (
	"html/template"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type ctxKey int

const (
	sessionKey ctxKey = iota
	csrfKey
)

type App struct {
	cfg                     config.Config
	store                   *store.Store
	mb                      catalog.CatalogProvider
	spotify                 catalog.SpotifyProvider
	itunes                  catalog.ITunesProvider
	sender                  notify.NotificationSender
	cipher                  *security.Cipher
	artwork                 artwork.Provider
	jobs                    *jobs.Runner
	logger                  *slog.Logger
	templates               *template.Template
	setupLimiter            *fixedWindowLimiter
	loginLimiter            *fixedWindowLimiter
	tokenLimiter            *fixedWindowLimiter
	discoveryLimiter        *fixedWindowLimiter
	importLimiter           *fixedWindowLimiter
	importSlots             chan struct{}
	providerLimiter         *fixedWindowLimiter
	artworkLimiter          *fixedWindowLimiter
	destinationTestLimiter  *fixedWindowLimiter
	destinationRetryLimiter *fixedWindowLimiter
	calendarFeedLimiter     *fixedWindowLimiter
	// untrustedForwardingWarned keeps the proxy misconfiguration warning to one
	// line per process.
	untrustedForwardingWarned atomic.Bool
	passwordChangeLimiter     *fixedWindowLimiter
	loginSlots                chan struct{}
	// logHealth reports the application-log sink's drop and write-failure
	// counters. It is set after construction rather than passed to New because
	// the sink is created around the same store this App is built from.
	logHealth func() LogSinkHealth
}

// LogSinkHealth is the application-log sink's runtime loss counters.
type LogSinkHealth struct {
	// Dropped counts records discarded because the sink queue was full, and
	// Errors counts records the sink accepted but failed to persist.
	Dropped uint64
	Errors  uint64
}

// SetLogHealth wires the application-log sink's counters into diagnostics.
// Both were previously read only on the clean-shutdown path, strictly after an
// early return that fires whenever the drain is unclean - so a SIGKILL, a
// panic, or a stalled drain reported nothing, and an operator watching a
// provider outage fill the 256-entry queue saw a truncated log history with no
// indication that anything had been lost.
func (a *App) SetLogHealth(report func() LogSinkHealth) {
	a.logHealth = report
}

// logSinkHealth reports the sink counters, or zeroes when nothing is wired.
func (a *App) logSinkHealth() LogSinkHealth {
	if a.logHealth == nil {
		return LogSinkHealth{}
	}
	return a.logHealth()
}

// UserView is the deliberately narrow projection exposed to templates. The
// persistence model contains PasswordHash for authentication, but rendering
// code should never receive credential material by accident.
type UserView struct {
	ID           int64
	Email        string
	Username     string
	Role         string
	Timezone     string
	ReminderTime string
}

func followDeliveryLabel(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case store.FollowDeliveryImmediate:
		return "Immediate only"
	case store.FollowDeliveryDigest:
		return "Digest only"
	case store.FollowDeliveryOff:
		return "No notifications"
	default:
		return "Account defaults"
	}
}

// followRulePaused reports whether a pause is still in force.
//
// Nothing ever clears paused_until once it lapses - PauseFollowNotificationRule
// is its only writer - so a nil check reports a follow as paused forever after
// a single 7-day pause, while the delivery engine, which compares against now
// (internal/store/follow_notification_rules.go:38-42), resumed delivering the
// moment it expired. The page then asserted "paused" over an artist that was
// actively alerting, and removed the Pause control so the member could not
// re-pause without first clicking "Resume now".
func followRulePaused(rule store.FollowNotificationRule) bool {
	return rule.PausedUntil != nil && rule.PausedUntil.After(time.Now().UTC())
}

// followRuleSummary describes a follow's delivery state for the Artists page.
// The expiry goes through formatTime so it carries the reader's timezone and a
// zone label; it was the last raw-UTC clock render in the product, and it lives
// in Go where the regression guard that globs the template tree cannot see it.
func followRuleSummary(rule store.FollowNotificationRule, timezones ...string) string {
	if followRulePaused(rule) {
		return "Paused until " + formatTime(*rule.PausedUntil, timezones...)
	}
	return followDeliveryLabel(rule.DeliveryMode)
}

type CalendarDay struct {
	Date     string
	Label    string
	Today    bool
	Releases []store.CalendarRelease
}

type PaginationLink struct {
	Number  int
	URL     string
	Current bool
}

type providerHealthPayload struct {
	Provider           string     `json:"provider"`
	Status             string     `json:"status"`
	StatusClass        string     `json:"status_class"`
	LastSuccessAt      *time.Time `json:"last_success_at,omitempty"`
	LastSuccessDisplay string     `json:"last_success_display,omitempty"`
	LastFailureAt      *time.Time `json:"last_failure_at,omitempty"`
	LastFailureDisplay string     `json:"last_failure_display,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	NextCheckAt        *time.Time `json:"next_check_at,omitempty"`
	NextCheckDisplay   string     `json:"next_check_display,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
	UpdatedDisplay     string     `json:"updated_display"`
	RateLimited        bool       `json:"rate_limited"`
	QuotaExceeded      bool       `json:"quota_exceeded"`
}
