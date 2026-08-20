package web

import (
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
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
	loginSlots              chan struct{}
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

type PageData struct {
	Title                    string
	Version                  string
	User                     *UserView
	CSRF                     string
	Error                    string
	Message                  string
	SetupNeeded              bool
	Artists                  []store.Artist
	Results                  []catalog.ArtistResult
	SpotifyResults           []catalog.SpotifyArtist
	ITunesResults            []catalog.ITunesArtist
	UpcomingReleases         []store.Release
	RecentReleases           []store.Release
	CalendarDays             []CalendarDay
	CalendarMonth            string
	CalendarPrevMonth        string
	CalendarNextMonth        string
	CalendarICSURL           string
	CalendarFeedURL          string
	CalendarFeedCreatedAt    *time.Time
	CalendarFeedExpiresAt    *time.Time
	CalendarFeedActive       bool
	ReleaseCount             int
	Preferences              store.NotificationPreferences
	NotificationHolds        []store.NotificationHold
	ReleaseNotificationHolds []store.NotificationHold
	ReleaseDetail            *store.ReleaseDetail
	ReleaseEvidenceIssues    []store.EvidenceIssue
	ReleaseUnavailable       bool
	Resolutions              []store.ArtistResolution
	Resolution               *store.ArtistResolution
	Destinations             []store.Destination
	DestinationHealth        map[int64]store.DestinationHealth
	History                  []store.DeliveryHistory
	AdminHistory             []store.AdminDeliveryHistory
	AdminDelivery            *store.AdminDeliveryHistory
	AppLogs                  []logging.Entry
	AdminUsers               []store.AdminUser
	AdminArtists             []store.AdminArtist
	ProviderHealth           []store.ProviderHealth
	AdminDestinationHealth   []store.AdminDestinationHealth
	ManualSyncs              []store.ManualSyncRequest
	Import                   *store.ImportJob
	FollowCount              int
	FollowRules              map[int64]store.FollowNotificationRule
	ListenBrainzArtists      []store.Artist
	GenreBreakdown           []store.ArtistBreakdown
	CountryBreakdown         []store.ArtistBreakdown
	TypeBreakdown            []store.ArtistBreakdown
	CoverageSummary          store.CoverageSummary
	CoverageArtists          []store.ArtistCoverage
	CoveragePage             int
	CoveragePages            int
	CoveragePrevPage         int
	CoverageNextPage         int
	CoveragePrevURL          string
	CoverageNextURL          string
	CoveragePageLinks        []PaginationLink
	CoveragePageStart        int
	CoveragePageEnd          int
	AssuranceSummary         store.AssuranceSummary
	Diagnostics              store.DiagnosticsSnapshot
	Retention                store.RetentionReport
	OperationalStatus        string
	OperationalReasons       []string
	OperationalSnapshots     []store.OperationalSnapshot
	RunnerStatus             jobs.RunnerStatus
	DiagnosticReport         string
	EvidenceIssues           []store.EvidenceIssue
	EvidenceIssueCount       int
	EvidenceIssueUnreadCount int
	EvidenceIssueStatus      string
	EvidenceIssueState       string
	EvidenceIssueType        string
	EvidenceIssueSeverity    string
	EvidenceIssuePage        int
	EvidenceIssuePages       int
	EvidenceIssuePrevURL     string
	EvidenceIssueNextURL     string
	EvidenceIssuePageLinks   []PaginationLink
	EvidenceIssuePageStart   int
	EvidenceIssuePageEnd     int
	EvidenceIssueURL         string
	GenreFilter              string
	CountryFilter            string
	TypeFilter               string
	InboxItems               []store.ReleaseInboxItem
	InboxUnreadCount         int
	InboxCount               int
	InboxState               string
	InboxSource              string
	InboxType                string
	InboxPage                int
	InboxPages               int
	InboxPrevURL             string
	InboxNextURL             string
	InboxPageLinks           []PaginationLink
	InboxPageStart           int
	InboxPageEnd             int
	InboxURL                 string
	AdminPage                int
	AdminPages               int
	AdminPrevPage            int
	AdminNextPage            int
	ArtistPage               int
	ArtistPages              int
	ArtistPrevPage           int
	ArtistNextPage           int
	ArtistPrevURL            string
	ArtistNextURL            string
	ArtistPageLinks          []PaginationLink
	ArtistPageStart          int
	ArtistPageEnd            int
	FilteredArtistCount      int
	Query                    string
	GeneratedURL             string
	Token                    string
	TokenKind                string
	TokenEmail               string
	SpotifyOn                bool
	ProviderNotice           string
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
