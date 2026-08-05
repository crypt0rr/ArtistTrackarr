package web

import (
	"html/template"
	"log/slog"
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
	cfg              config.Config
	store            *store.Store
	mb               catalog.CatalogProvider
	spotify          catalog.SpotifyProvider
	itunes           catalog.ITunesProvider
	sender           notify.NotificationSender
	cipher           *security.Cipher
	artwork          artwork.Provider
	jobs             *jobs.Runner
	logger           *slog.Logger
	templates        *template.Template
	setupLimiter     *fixedWindowLimiter
	loginLimiter     *fixedWindowLimiter
	tokenLimiter     *fixedWindowLimiter
	discoveryLimiter *fixedWindowLimiter
	providerLimiter  *fixedWindowLimiter
	loginSlots       chan struct{}
}

type PageData struct {
	Title                    string
	Version                  string
	User                     *store.User
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
	ReleaseCount             int
	Preferences              store.NotificationPreferences
	ReleaseDetail            *store.ReleaseDetail
	ReleaseEvidenceIssues    []store.EvidenceIssue
	ReleaseUnavailable       bool
	Resolutions              []store.ArtistResolution
	Resolution               *store.ArtistResolution
	Destinations             []store.Destination
	History                  []store.DeliveryHistory
	AdminHistory             []store.AdminDeliveryHistory
	AdminDelivery            *store.AdminDeliveryHistory
	AppLogs                  []logging.Entry
	AdminUsers               []store.AdminUser
	AdminArtists             []store.AdminArtist
	ProviderHealth           []store.ProviderHealth
	ManualSyncs              []store.ManualSyncRequest
	Import                   *store.ImportJob
	FollowCount              int
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
