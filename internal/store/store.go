package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store keeps writes on a single connection while allowing read-only queries
// to use a small pool. SQLite WAL mode lets readers proceed while the writer
// is committing, which keeps dashboard requests from queueing behind provider
// synchronization work.
type Store struct {
	DB     *sql.DB
	Reader *sql.DB
}

func (s *Store) readerDB() *sql.DB {
	if s.Reader != nil {
		return s.Reader
	}
	// A few internal tests construct Store values around an existing database
	// handle. Keep those fixtures working without weakening production access.
	return s.DB
}

// beginWriteTx retries transient SQLite lock errors at the transaction
// boundary. The writer pool is intentionally one connection, but a reader
// may still be finishing a WAL checkpoint or a short-lived external process
// may hold the file lock.
func (s *Store) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		lastErr = err
		if !sqliteBusy(err) || attempt == 4 {
			break
		}
		delay := time.Duration(25*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func sqliteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "database is busy")
}

type User struct {
	ID           int64
	Email        string
	Username     string
	PasswordHash string
	Role         string
	Timezone     string
	ReminderTime string
	CreatedAt    time.Time
}

type NotificationPreferences struct {
	UserID        int64
	Albums        bool
	EPs           bool
	Singles       bool
	Announcements bool
	ReleaseDay    bool
	// DigestEnabled enables a compact upcoming-release digest delivered at
	// the user's existing reminder time.
	DigestEnabled   bool
	DigestFrequency string
	// HoldConflictingNotifications keeps warning/critical provider conflicts
	// out of deliveries until a household member explicitly reviews them.
	// It defaults to false so existing accounts retain immediate notifications.
	HoldConflictingNotifications bool
}

type AdminUser struct {
	ID               int64
	Email            string
	Username         string
	Role             string
	Timezone         string
	ReminderTime     string
	FollowCount      int
	DestinationCount int
	CreatedAt        time.Time
}

var (
	ErrAdminRequired                 = errors.New("administrator access is required")
	ErrCannotDeleteSelf              = errors.New("you cannot delete your own account")
	ErrLastAdmin                     = errors.New("the last administrator cannot be deleted")
	ErrInvalidUsername               = errors.New("username must be 3-32 characters using letters, numbers, dots, underscores, or hyphens")
	ErrUsernameTaken                 = errors.New("that username is already in use")
	ErrInvalidNotificationHoldAction = errors.New("invalid notification hold action")
)

type Session struct {
	User      User
	CSRFToken string
	ExpiresAt time.Time
}

type Artist struct {
	ID                 int64
	MBID               string
	Name               string
	SortName           string
	Type               string
	Country            string
	Disambiguation     string
	SpotifyID          string
	SpotifyURL         string
	SpotifyImageURL    string
	Genres             []string
	ListenCount        int64
	ListenUsers        int64
	ListenCheckedAt    *time.Time
	LastCheckedAt      *time.Time
	SpotifyNextCheckAt *time.Time
	BaselineSynced     bool
}

type ListenBrainzStats struct {
	ArtistID         int64
	MBID             string
	TotalListenCount int64
	TotalUserCount   int64
	CheckedAt        *time.Time
	NextCheckAt      *time.Time
	LastError        string
}

type ArtistBreakdown struct {
	Label string
	Count int
}

type Release struct {
	ID               int64
	MBID             string
	ArtistID         int64
	ArtistName       string
	Title            string
	PrimaryType      string
	SecondaryTypes   []string
	FirstReleaseDate string
	DatePrecision    int
	MusicBrainzURL   string
	SpotifyID        string
	SpotifyURL       string
	SpotifyImageURL  string
	ITunesID         string
	ITunesURL        string
	ITunesArtworkURL string
	// ArtistCreditRole records how the followed artist is credited by the
	// provider. Spotify can return releases through appears_on when the
	// artist is featured on another artist's release.
	ArtistCreditRole string
	Source           string
	SourceCount      int
	Sources          []string
	Confidence       string
	TruthState       string
	TruthProvider    string
	TruthProviderID  string
	TruthReason      string
	TruthUpdatedAt   *time.Time
	TruthIssueCount  int
	LastObservedAt   *time.Time
	FirstObservedAt  time.Time
}

// ReleaseTruthDecision is an explicit, reversible source choice for a
// release. It never overwrites provider observations or canonical metadata.
type ReleaseTruthDecision struct {
	ReleaseGroupID     int64
	State              string
	SelectedProvider   string
	SelectedProviderID string
	Reason             string
	DecidedByUserID    *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ReleaseDetail struct {
	Release
	Observations []ReleaseObservation
}

// CalendarRelease is an owner-scoped release projection used by the calendar
// page, ICS export, and digest builder. Held is deliberately derived from the
// requesting user's notification hold state and never changes shared release
// metadata.
type CalendarRelease struct {
	Release
	CalendarDate string
	Held         bool
}

// DigestDelivery is a queued delivery for one generated release digest. It
// uses the same encrypted destination and retry semantics as normal events,
// but keeps one aggregate message per digest period instead of attaching the
// message to an individual release event.
type DigestDelivery struct {
	ID          int64
	RunID       int64
	Destination Destination
	Title       string
	Body        string
	Attempts    int
	NextAttempt time.Time
}

// NotificationHold is an owner-scoped notification that was kept out of the
// delivery queue while provider evidence was conflicting. The original event
// content is retained so releasing a hold is deterministic and auditable.
type NotificationHold struct {
	ID               int64
	UserID           int64
	ReleaseGroupID   int64
	ArtistName       string
	ReleaseTitle     string
	EventType        string
	Title            string
	Body             string
	Reason           string
	IssueFingerprint string
	PlannedAt        time.Time
	Status           string
	CreatedAt        time.Time
	ReleasedAt       *time.Time
}

// ReleaseEvidence is the small normalized provider snapshot used to explain
// an evidence issue. Raw provider payloads and credentials are never stored.
type ReleaseEvidence struct {
	Provider         string
	ProviderID       string
	Title            string
	PrimaryType      string
	FirstReleaseDate string
	DatePrecision    int
	ProviderURL      string
	ArtistCreditRole string
	ObservedAt       time.Time
}

// EvidenceIssue describes a provider disagreement or a release that has not
// yet been confirmed by the canonical catalog. The review state is private to
// the requesting user and does not alter release or notification semantics.
type EvidenceIssue struct {
	ID             int64
	ReleaseGroupID int64
	ArtistID       int64
	ArtistName     string
	ReleaseTitle   string
	IssueType      string
	Severity       string
	Fingerprint    string
	Summary        string
	Evidence       []ReleaseEvidence
	Status         string
	ReviewState    string
	SnoozedUntil   *time.Time
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
	ResolvedAt     *time.Time
}

// ReleaseInboxItem is the latest alertable event for one followed release,
// together with the member's local review state. Inbox state never changes
// provider observations or notification delivery state.
type ReleaseInboxItem struct {
	Release
	EventType      string
	EventTitle     string
	EventCreatedAt time.Time
	State          string
	SnoozedUntil   *time.Time
}

type ReleaseObservation struct {
	Provider   string
	ProviderID string
	ObservedAt time.Time
}

type ReleaseBatch struct {
	Provider string
	Releases []Release
}

// ArtistProviderStatus records the last known state of one provider for one
// followed artist. It is intentionally separate from provider_health, which
// describes the process-wide provider cooldown and not an individual artist.
type ArtistProviderStatus struct {
	ArtistID      int64
	Provider      string
	Status        string
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	NextCheckAt   *time.Time
	ReleaseCount  int
	LastError     string
	UpdatedAt     time.Time
}

type ArtistCoverage struct {
	Artist               Artist
	OverallStatus        string
	ProviderStatuses     []ArtistProviderStatus
	ReleaseCount         int
	ConfirmedReleases    int
	SingleSourceReleases int
	FallbackReleases     int
	LastObservedAt       *time.Time
	NextCheckAt          *time.Time
}

type CoverageSummary struct {
	Artists              int
	FreshArtists         int
	AttentionArtists     int
	PendingArtists       int
	ConfirmedReleases    int
	SingleSourceReleases int
	FallbackReleases     int
}

// ITunesArtworkArtist identifies one artist whose existing iTunes releases
// are due for an artwork-only refresh. Artwork backfills deliberately operate
// on existing rows and never create releases or notification events.
type ITunesArtworkArtist struct {
	ID       int64
	Name     string
	Attempts int
}

type SpotifyPollingState struct {
	UnchangedChecks int
	LastChangeAt    *time.Time
}

type Destination struct {
	ID           int64
	UserID       int64
	Name         string
	Service      string
	EncryptedURL []byte
	Enabled      bool
}

// DestinationHealth is durable, owner-visible delivery state. It is kept
// separate from Destination so encrypted credentials and service metadata are
// never mixed into operational status projections.
type DestinationHealth struct {
	DestinationID       int64
	Status              string
	ConsecutiveFailures int
	PendingCount        int
	FailedCount         int
	LastSuccessAt       *time.Time
	LastFailureAt       *time.Time
	NextRetryAt         *time.Time
	LastError           string
	UpdatedAt           time.Time
}

type AdminDestinationHealth struct {
	DestinationID       int64
	UserEmail           string
	DestinationName     string
	Service             string
	Status              string
	ConsecutiveFailures int
	PendingCount        int
	FailedCount         int
	LastSuccessAt       *time.Time
	LastFailureAt       *time.Time
	NextRetryAt         *time.Time
	LastError           string
	UpdatedAt           time.Time
}

type Delivery struct {
	ID           int64
	EventID      int64
	Destination  Destination
	Title        string
	Body         string
	Attempts     int
	NextAttempt  time.Time
	EventType    string
	ReleaseTitle string
}

type DeliveryHistory struct {
	Title       string
	EventType   string
	Destination string
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	SentAt      *time.Time
}

type AdminDeliveryHistory struct {
	DeliveryID  int64
	UserEmail   string
	Title       string
	Body        string
	EventType   string
	Destination string
	Service     string
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	NextAttempt *time.Time
	SentAt      *time.Time
}

type ManualSyncRequest struct {
	ID          int64
	RequestedBy int64
	Scope       string
	ArtistID    *int64
	Status      string
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	LastError   string
}

type ProviderHealth struct {
	Provider      string
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	LastError     string
	NextCheckAt   *time.Time
	RateLimited   bool
	QuotaExceeded bool
	UpdatedAt     time.Time
}

type AdminArtist struct {
	ID   int64
	Name string
	MBID string
}

type ResolutionCandidate struct {
	MBID           string   `json:"mbid"`
	Name           string   `json:"name"`
	SortName       string   `json:"sort_name"`
	Type           string   `json:"type"`
	Country        string   `json:"country"`
	Disambiguation string   `json:"disambiguation"`
	Aliases        []string `json:"aliases,omitempty"`
	Genres         []string `json:"genres,omitempty"`
	Score          int      `json:"score"`
}

type ArtistResolution struct {
	ID          int64
	UserID      int64
	Provider    string
	ProviderID  string
	DisplayName string
	ProviderURL string
	ImageURL    string
	Status      string
	Candidates  []ResolutionCandidate
	Attempts    int
	NextAttempt *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type syncedRelease struct {
	release  Release
	isNew    bool
	provider string
}

const artistResolutionColumns = `id,user_id,provider,provider_id,display_name,provider_url,image_url,status,
	candidate_json,attempts,next_attempt_at,last_error,created_at,updated_at`

const releaseSelectColumns = `rg.id,rg.mbid,rg.artist_id,a.name,rg.title,rg.primary_type,
	rg.secondary_types,rg.first_release_date,rg.date_precision,rg.musicbrainz_url,
	rg.spotify_id,rg.spotify_url,rg.spotify_image_url,rg.itunes_id,rg.itunes_url,rg.itunes_artwork_url,rg.artist_credit_role,rg.source,rg.first_observed_at,
	(SELECT COUNT(DISTINCT po.provider) FROM provider_observations po WHERE po.release_group_id=rg.id),
	(SELECT GROUP_CONCAT(DISTINCT po.provider) FROM provider_observations po WHERE po.release_group_id=rg.id),
	(SELECT MAX(po.observed_at) FROM provider_observations po WHERE po.release_group_id=rg.id),
	COALESCE((SELECT state FROM release_truth_decisions td WHERE td.release_group_id=rg.id),''),
	COALESCE((SELECT selected_provider FROM release_truth_decisions td WHERE td.release_group_id=rg.id),''),
	COALESCE((SELECT selected_provider_id FROM release_truth_decisions td WHERE td.release_group_id=rg.id),''),
	COALESCE((SELECT reason FROM release_truth_decisions td WHERE td.release_group_id=rg.id),''),
	(SELECT updated_at FROM release_truth_decisions td WHERE td.release_group_id=rg.id),
	(SELECT COUNT(*) FROM release_evidence_issues ei WHERE ei.release_group_id=rg.id AND ei.status='open' AND ei.severity IN ('warning','critical'))`
