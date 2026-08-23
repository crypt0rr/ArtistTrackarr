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

func followRuleSummary(rule store.FollowNotificationRule) string {
	if rule.PausedUntil != nil {
		return "Paused until " + rule.PausedUntil.Format("2006-01-02 15:04")
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
