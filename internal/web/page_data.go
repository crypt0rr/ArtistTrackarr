package web

import (
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

// PageData is the template-facing composition of the domain projections. The
// projections below keep account, discovery, release, admin, coverage, and
// inbox fields owned by their respective page concerns while preserving the
// existing template field names and rendering contract.
type PageData struct {
	PageMeta
	PageDiscovery
	PageRelease
	PageAccount
	PageAdmin
	PageCoverage
	PageInbox
}

// PageMeta contains the fields shared by every rendered page.
type PageMeta struct {
	Title          string
	Version        string
	User           *UserView
	CSRF           string
	Error          string
	Message        string
	SetupNeeded    bool
	SpotifyOn      bool
	ProviderNotice string
}

// PageDiscovery contains artist search and watchlist projections.
type PageDiscovery struct {
	Artists          []store.Artist
	Results          []catalog.ArtistResult
	SpotifyResults   []catalog.SpotifyArtist
	ITunesResults    []catalog.ITunesArtist
	UpcomingReleases []store.Release
	RecentReleases   []store.Release
	ReleaseCount     int
	FollowCount      int
	ImportJobs       []store.ImportJob
	// ArtistListQuery is the current pagination and filter state, echoed into
	// action forms so a redirect can return to the same view.
	ArtistListQuery string
	// ReturnPath is the local path an unauthenticated request was trying to
	// reach, carried through the login form so the member lands where they meant
	// to rather than on the dashboard.
	ReturnPath          string
	ListenBrainzArtists []store.Artist
	GenreBreakdown      []store.ArtistBreakdown
	CountryBreakdown    []store.ArtistBreakdown
	TypeBreakdown       []store.ArtistBreakdown
	GenreFilter         string
	CountryFilter       string
	TypeFilter          string
	ArtistPrevURL       string
	ArtistNextURL       string
	ArtistPageLinks     []PaginationLink
	ArtistPageStart     int
	ArtistPageEnd       int
	FilteredArtistCount int
	Query               string
}

// PageRelease contains release, calendar, and notification-hold projections.
type PageRelease struct {
	CalendarDays             []CalendarDay
	CalendarMonth            string
	CalendarPrevMonth        string
	CalendarNextMonth        string
	CalendarICSURL           string
	CalendarNotice           string
	CalendarFeedURL          string
	CalendarFeedExpiresAt    *time.Time
	CalendarFeedActive       bool
	NotificationHolds        []store.NotificationHold
	ReleaseNotificationHolds []store.NotificationHold
	ReleaseDetail            *store.ReleaseDetail
	ReleaseEvidenceIssues    []store.EvidenceIssue
	ReleaseUnavailable       bool
}

// PageAccount contains settings, destination, token, and artist-resolution
// projections used by account-management pages.
type PageAccount struct {
	Preferences       store.NotificationPreferences
	Resolutions       []store.ArtistResolution
	Resolution        *store.ArtistResolution
	Destinations      []store.Destination
	DestinationHealth map[int64]store.DestinationHealth
	History           []store.DeliveryHistory
	FollowRules       map[int64]store.FollowNotificationRule
	Import            *store.ImportJob
	GeneratedURL      string
	Token             string
	TokenKind         string
	TokenEmail        string
}

// PageAdmin contains household administration and operational projections.
type PageAdmin struct {
	AdminHistory           []store.AdminDeliveryHistory
	AdminDelivery          *store.AdminDeliveryHistory
	AppLogs                []logging.Entry
	AdminUsers             []store.AdminUser
	AdminArtists           []store.AdminArtist
	ProviderHealth         []store.ProviderHealth
	AdminDestinationHealth []store.AdminDestinationHealth
	ManualSyncs            []store.ManualSyncRequest
	Diagnostics            store.DiagnosticsSnapshot
	Retention              store.RetentionReport
	OperationalStatus      string
	OperationalReasons     []string
	OperationalSnapshots   []store.OperationalSnapshot
	RunnerStatus           jobs.RunnerStatus
	DiagnosticReport       string
	AdminPage              int
	AdminPages             int
	AdminPrevPage          int
	AdminNextPage          int
}

// PageCoverage contains source-confidence and evidence-review projections.
type PageCoverage struct {
	CoverageSummary          store.CoverageSummary
	CoverageArtists          []store.ArtistCoverage
	CoveragePage             int
	CoveragePages            int
	CoveragePrevURL          string
	CoverageNextURL          string
	CoveragePageLinks        []PaginationLink
	CoveragePageStart        int
	CoveragePageEnd          int
	AssuranceSummary         store.AssuranceSummary
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
}

// PageInbox contains inbox state, filters, and pagination.
type PageInbox struct {
	InboxItems       []store.ReleaseInboxItem
	InboxUnreadCount int
	InboxCount       int
	InboxState       string
	InboxSource      string
	InboxType        string
	InboxPage        int
	InboxPages       int
	InboxPrevURL     string
	InboxNextURL     string
	InboxPageLinks   []PaginationLink
	InboxPageStart   int
	InboxPageEnd     int
	InboxURL         string
}
