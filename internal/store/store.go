package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
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
	// dataDir is set for stores opened through Open and is intentionally empty
	// for lightweight test fixtures that wrap an existing *sql.DB. Operational
	// marker files in this directory contain only non-sensitive backup/recovery
	// timestamps and status labels.
	dataDir             string
	readerMu            sync.RWMutex
	healthMu            sync.RWMutex
	retentionMu         sync.RWMutex
	pollInterval        time.Duration
	spotifyPollInterval time.Duration
	retentionPolicy     RetentionPolicy
	closeOnce           sync.Once
	closeErr            error
}

// RetentionPolicy describes the bounded operational state that may be
// removed by maintenance. Notification events, delivery queue rows, inbox
// state, blocked work, and delivery-attempt audit records are intentionally
// not part of this policy and are retained indefinitely.
type RetentionPolicy struct {
	ApplicationLogsDays int
	TransientStateDays  int
	// HistoryReviewDays is a non-destructive review threshold for notification
	// and delivery history. Those records are never removed automatically; the
	// threshold only tells administrators when a retention decision is due.
	HistoryReviewDays int
}

// DefaultRetentionPolicy is conservative and matches the existing automatic
// maintenance windows. It is exposed so the administrator report and tests
// can describe exactly what an explicit cleanup would remove.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{ApplicationLogsDays: 7, TransientStateDays: 30, HistoryReviewDays: 365}
}

func normalizeRetentionPolicy(policy RetentionPolicy) RetentionPolicy {
	defaults := DefaultRetentionPolicy()
	if policy.ApplicationLogsDays <= 0 {
		policy.ApplicationLogsDays = defaults.ApplicationLogsDays
	}
	if policy.TransientStateDays <= 0 {
		policy.TransientStateDays = defaults.TransientStateDays
	}
	if policy.HistoryReviewDays <= 0 {
		policy.HistoryReviewDays = defaults.HistoryReviewDays
	}
	return policy
}

// SetRetentionPolicy updates the maintenance windows used by reports and
// explicit administrator cleanup. A zero value restores the safe defaults.
func (s *Store) SetRetentionPolicy(policy RetentionPolicy) {
	s.retentionMu.Lock()
	s.retentionPolicy = normalizeRetentionPolicy(policy)
	s.retentionMu.Unlock()
}

func (s *Store) retention() RetentionPolicy {
	s.retentionMu.RLock()
	policy := s.retentionPolicy
	s.retentionMu.RUnlock()
	return normalizeRetentionPolicy(policy)
}

// RetentionPolicy returns the effective windows used by scheduled and
// explicit maintenance.
func (s *Store) RetentionPolicy() RetentionPolicy {
	return s.retention()
}

// SetProviderHealthCadences supplies the configured polling intervals used by
// diagnostics when deciding whether a successful provider check is stale. A
// zero value keeps the conservative historical defaults for tests and
// internal callers that construct a Store directly.
func (s *Store) SetProviderHealthCadences(pollInterval, spotifyInterval time.Duration) {
	s.healthMu.Lock()
	s.pollInterval = pollInterval
	s.spotifyPollInterval = spotifyInterval
	s.healthMu.Unlock()
}

func (s *Store) providerHealthStaleAfter(provider string) time.Duration {
	s.healthMu.RLock()
	pollInterval, spotifyInterval := s.pollInterval, s.spotifyPollInterval
	s.healthMu.RUnlock()
	return ProviderHealthStaleAfterCadence(provider, pollInterval, spotifyInterval)
}

func (s *Store) readerDB() *sql.DB {
	s.readerMu.RLock()
	defer s.readerMu.RUnlock()
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
		if err := waitWriteRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// withWriteTx executes one logical write transaction with bounded retry for
// transient SQLite contention. The closure is replayed in its entirety after
// rollback, so a busy/locked error at an intermediate statement or during
// commit cannot leave callers with a partially applied operation. Callers must
// keep the closure limited to database work; external side effects belong
// after this helper returns successfully.
func (s *Store) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.beginWriteTx(ctx)
		if err != nil {
			// beginWriteTx already exhausted its bounded transaction-start
			// retries. Replaying the closure cannot help when no transaction
			// could be opened, so preserve that error directly.
			return err
		}
		err = fn(tx)
		if err != nil {
			_ = tx.Rollback()
			lastErr = err
		} else if err = tx.Commit(); err != nil {
			_ = tx.Rollback()
			lastErr = err
		} else {
			return nil
		}
		if !sqliteBusy(lastErr) || attempt == 4 {
			return lastErr
		}
		if err := waitWriteRetry(ctx, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

// withWriteTxResult is the result-bearing form used by transactional store
// operations. The result is only published after a successful commit, so a
// replay cannot expose values from a rolled-back attempt.
func withWriteTxResult[T any](s *Store, ctx context.Context, fn func(*sql.Tx) (T, error)) (T, error) {
	var zero T
	var result T
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		value, err := fn(tx)
		if err == nil {
			result = value
		}
		return err
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

// execWriteContext applies the same bounded busy/locked retry policy to
// single-statement writes that do not need a multi-statement transaction.
// Keeping this behind Store prevents individual persistence paths from
// accidentally bypassing SQLite contention handling.
func (s *Store) execWriteContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var result sql.Result
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		result, lastErr = s.DB.ExecContext(ctx, query, args...)
		if lastErr == nil || !sqliteBusy(lastErr) || attempt == 4 {
			return result, lastErr
		}
		if err := waitWriteRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return result, lastErr
}

func waitWriteRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(25*(1<<attempt)) * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sqliteBusy(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// SQLite result codes use the low byte for the primary result. BUSY
		// (5) and LOCKED (6) are retryable even when an extended code is set.
		code := sqliteErr.Code() & 0xff
		if code == 5 || code == 6 {
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "database is busy")
}

// parseStoredTime is used for non-null persisted timestamps whose value is
// required for a meaningful projection. Returning the corruption error keeps
// stale/zero times from being presented as trustworthy operational state.
func parseStoredTime(value, field string) (time.Time, error) {
	t, err := parseTime(strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid persisted %s: %w", field, err)
	}
	return t, nil
}

func parseStoredNullableTime(value sql.NullString, field string) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	t, err := parseStoredTime(value.String, field)
	if err != nil {
		return nil, err
	}
	return &t, nil
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

// FollowNotificationRule controls how one followed artist contributes to a
// member's notification stream. The inherit mode preserves the account-wide
// notification and digest preferences; other modes let a member make a noisy
// or especially important follow more specific without changing shared
// release data.
type FollowNotificationRule struct {
	UserID          int64
	ArtistID        int64
	DeliveryMode    string
	IncludePrimary  bool
	IncludeFeatured bool
	Albums          bool
	EPs             bool
	Singles         bool
	Compilations    bool
	Announcements   bool
	ReleaseDay      bool
	PausedUntil     *time.Time
	UpdatedAt       time.Time
}

const (
	FollowDeliveryInherit   = "inherit"
	FollowDeliveryImmediate = "immediate"
	FollowDeliveryDigest    = "digest"
	FollowDeliveryOff       = "off"
)

func defaultFollowNotificationRule(userID, artistID int64, now time.Time) FollowNotificationRule {
	return FollowNotificationRule{
		UserID: userID, ArtistID: artistID, DeliveryMode: FollowDeliveryInherit,
		IncludePrimary: true, IncludeFeatured: true, Albums: true, EPs: true,
		Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
		UpdatedAt: now,
	}
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
	ErrManualSyncQueueFull           = errors.New("manual synchronization queue is full; try again later")
	ErrDestinationLimit              = errors.New("notification destination limit reached; remove an existing destination before adding another")
	ErrArtistResolutionLimit         = errors.New("artist identification limit reached; review or cancel an existing item before adding another")
	ErrSetupCompleted                = errors.New("setup has already completed")
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
	// FollowedArtists lists the member's followed artists associated with this
	// release. It is populated on detail/message projections and is not used as
	// the canonical release artist identity.
	FollowedArtists []string
	// Credits contains provider-specific credit evidence for this release. It
	// is populated on newly observed releases and on release-detail views; the
	// release-level ArtistCreditRole remains the compatibility projection used
	// by existing notification rules and queries.
	Credits          []ReleaseCredit
	GuestCreditCount int
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

// ReleaseCredit records why a followed artist is associated with a release.
// A release can have several provider-backed credits (for example a primary
// Spotify credit and a guest MusicBrainz recording credit). Credit evidence is
// append-only apart from its last-seen timestamp so provider outages never
// erase a previously trustworthy relationship.
type ReleaseCredit struct {
	ID             int64
	ReleaseGroupID int64
	ArtistID       int64
	Provider       string
	ProviderID     string
	Role           string
	TrackTitle     string
	CreditName     string
	ProviderURL    string
	Confidence     string
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
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
	Credits      []ReleaseCredit
	Timeline     []ReleaseTimelineEntry
	// TruthDecidedByYou and TruthDecidedByAnotherMember describe who recorded
	// the shared truth decision. release_truth_decisions holds one row per
	// release with no user dimension, so without this a decision another member
	// recorded - including its free-text reason - reads as the viewer's own.
	TruthDecidedByYou           bool
	TruthDecidedByAnotherMember bool
}

// CalendarRelease is an owner-scoped release projection used by the calendar
// page, ICS export, and digest builder. Held is deliberately derived from the
// requesting user's notification hold state and never changes shared release
// metadata.
type CalendarRelease struct {
	Release
	CalendarDate string
	Held         bool
	// FollowedAssociations carries the owner-scoped artist identities that
	// make this release visible. The canonical release artist remains in
	// Release.ArtistID/ArtistName, while digest eligibility must evaluate the
	// notification rule for every followed credit (including guest/featured
	// appearances).
	FollowedAssociations []FollowedArtistAssociation
}

// FollowedArtistAssociation is an owner-scoped release association used by
// calendar and digest projections. Label is presentation-ready; Role is the
// deterministic credit role for the followed artist.
type FollowedArtistAssociation struct {
	ArtistID int64
	Label    string
	Role     string
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
	ClaimOwner  string
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

// ReleaseTimelineEntry is a redacted, owner-scoped explanation of the
// observations and decisions that shaped a release. It deliberately contains
// normalized summaries only; provider payloads, credentials, and notification
// bodies never enter the timeline projection.
type ReleaseTimelineEntry struct {
	Kind       string
	Provider   string
	Role       string
	Status     string
	Summary    string
	OccurredAt time.Time
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

// ArtistIdentityStatus tracks whether a canonical MusicBrainz identifier has
// been verified with the provider. Imported identifiers start pending and
// are retried with a bounded schedule; terminal failures are excluded from
// automatic polling until an explicit manual sync resets them.
type ArtistIdentityStatus struct {
	ArtistID    int64
	Status      string
	Attempts    int
	NextCheckAt *time.Time
	LastError   string
	UpdatedAt   time.Time
}

type ArtistCoverage struct {
	Artist                 Artist
	OverallStatus          string
	AssuranceStatus        string
	AssuranceReason        string
	LastSuccessfulProvider string
	ProviderStatuses       []ArtistProviderStatus
	ReleaseCount           int
	ConfirmedReleases      int
	SingleSourceReleases   int
	FallbackReleases       int
	LastObservedAt         *time.Time
	NextCheckAt            *time.Time
}

type CoverageSummary struct {
	Artists                 int
	FreshArtists            int
	AttentionArtists        int
	PendingArtists          int
	ConfirmedReleases       int
	SingleSourceReleases    int
	FallbackReleases        int
	HealthyArtists          int
	DelayedArtists          int
	DegradedArtists         int
	PendingAssuranceArtists int
}

// AssuranceSummary is the actionable, freshness-oriented view of a user's
// watchlist. AtRisk contains a small severity-ranked subset for dashboard
// rendering; the complete coverage page remains paginated separately.
type AssuranceSummary struct {
	Total    int
	Healthy  int
	Delayed  int
	Degraded int
	Pending  int
	AtRisk   []ArtistCoverage
}

// DiagnosticsSnapshot contains safe operational counters for the admin
// support view. It intentionally excludes provider error text, credentials,
// notification bodies, and destination URLs.
type DiagnosticsSnapshot struct {
	CheckedAt       time.Time
	DatabaseHealthy bool
	// DatabaseHealthState distinguishes a readable database from one that is
	// read-only, full, unavailable, or otherwise unable to persist writes.
	// It is live diagnostic state and is not persisted in hourly snapshots.
	DatabaseHealthState DatabaseHealthState
	SchemaVersion       int
	FollowedArtists     int
	Releases            int
	QueuedSyncs         int
	RunningSyncs        int
	// DueSyncArtists is the total number of distinct followed artists whose
	// normal or Spotify schedule is due. It is intentionally separate from
	// the runner's per-tick batch size, which is capped for fairness.
	DueSyncArtists    int
	OldestDueSyncAt   *time.Time
	PendingDeliveries int
	FailedDeliveries  int
	RecentLogEntries  int
	// DroppedLogEntries and LogWriteFailures qualify RecentLogEntries. They are
	// filled in by the process that owns the application-log sink rather than
	// by the diagnostics query, because the counters live in that sink and not
	// in the database - a write failure is precisely the case where the
	// database cannot be asked.
	DroppedLogEntries uint64
	LogWriteFailures  uint64
	OldestQueueAt     *time.Time
	// FutureDeliveries flags pending work parked far beyond the normal retry
	// horizon. This is a safe clock-skew signal; it does not alter admission.
	FutureDeliveries       int
	EarliestFutureDelivery *time.Time
	StaleClaims            int
	PausedDestinations     int
	ProviderFailures       int
	// These live-derived timestamps let operational health distinguish a
	// transient provider/digest condition from one that has persisted beyond
	// its documented warning threshold. They are not persisted in the bounded
	// hourly snapshot table.
	OldestProviderFailureAt *time.Time
	DigestBacklog           int
	OldestDigestBacklogAt   *time.Time
	DatabaseBytes           int64
	// DatabaseFreeBytes is the SQLite freelist space that can be reused by
	// future writes. It is intentionally separate from DatabaseBytes because
	// deleting rows does not necessarily shrink the database file.
	DatabaseFreeBytes int64
	LastBackupAt      *time.Time
	LastRestoreAt     *time.Time
	LastRestoreResult string
	Providers         []DiagnosticsProvider
}

// ArtistSyncBacklog describes the complete normal/Spotify due queue. It is
// intentionally separate from the bounded per-tick batch returned by
// ArtistsDue so operators can distinguish a healthy small batch from a
// household backlog waiting behind the scheduler cap.
type ArtistSyncBacklog struct {
	Count       int
	OldestDueAt *time.Time
}

// OperationalSnapshot is a bounded, redacted history of the administrator
// diagnostics. It contains counters and timestamps only; provider payloads,
// credentials, notification bodies, and destination URLs are never stored.
type OperationalSnapshot struct {
	ID                 int64
	CapturedAt         time.Time
	Status             string
	RunnerStatus       string
	DatabaseHealthy    bool
	SchemaVersion      int
	FollowedArtists    int
	Releases           int
	QueuedSyncs        int
	RunningSyncs       int
	PendingDeliveries  int
	FailedDeliveries   int
	RecentLogEntries   int
	OldestQueueAt      *time.Time
	StaleClaims        int
	PausedDestinations int
	ProviderFailures   int
	DigestBacklog      int
	DatabaseBytes      int64
	LastBackupAt       *time.Time
	LastRestoreAt      *time.Time
	LastRestoreResult  string
}

// DiagnosticsProvider is the redacted provider projection used by support
// reports. Error text and all credentials remain outside this type.
type DiagnosticsProvider struct {
	Provider    string
	Status      string
	NextCheckAt *time.Time
}

// ITunesArtworkArtist identifies one artist whose existing iTunes releases
// are due for an artwork-only refresh. Artwork backfills deliberately operate
// on existing rows and never create releases or notification events.
type ITunesArtworkArtist struct {
	ID       int64
	MBID     string
	Name     string
	Attempts int
}

type SpotifyPollingState struct {
	UnchangedChecks int
	LastChangeAt    *time.Time
}

type Destination struct {
	ID               int64
	UserID           int64
	Name             string
	Service          string
	EncryptedURL     []byte
	Enabled          bool
	TransportStatus  string
	TransportMessage string
}

// DestinationHealth is durable, owner-visible delivery state. It is kept
// separate from Destination so encrypted credentials and service metadata are
// never mixed into operational status projections.
type DestinationHealth struct {
	DestinationID       int64
	Status              string
	ConsecutiveFailures int
	PendingCount        int
	BlockedCount        int
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
	BlockedCount        int
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
	ClaimOwner   string
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
	DeliveryID int64
	// DeliveryKind distinguishes normal notification deliveries from digest
	// deliveries. Both are part of the household audit, but they live in
	// separate queue tables and therefore require different detail routes.
	DeliveryKind string
	UserEmail    string
	Title        string
	Body         string
	EventType    string
	Destination  string
	Service      string
	Status       string
	Attempts     int
	LastError    string
	CreatedAt    time.Time
	NextAttempt  *time.Time
	SentAt       *time.Time
}

// AdminDeliveryExportCursor identifies the last row emitted by the stable
// delivery-audit export ordering. Cursors are intentionally opaque to the web
// layer apart from their fields; they are never persisted or exposed to users.
type AdminDeliveryExportCursor struct {
	CreatedAt    string
	EventID      int64
	DeliveryID   int64
	DeliveryKind string
}

// ArtistExportCursor identifies the last row emitted by the stable followed
// artist export ordering. The normalized name keys mirror the SQL ORDER BY
// expression so concurrent inserts cannot make OFFSET pages skip or repeat
// existing artists.
type ArtistExportCursor struct {
	Name     string
	SortName string
	ID       int64
}

type ManualSyncRequest struct {
	ID             int64
	RequestedBy    int64
	Scope          string
	ArtistID       *int64
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	LastError      string
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	AttemptCount   int
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
	release   Release
	isNew     bool
	creditNew bool
	provider  string
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
	(SELECT COUNT(*) FROM release_evidence_issues ei WHERE ei.release_group_id=rg.id AND ei.status='open' AND ei.severity IN ('warning','critical')),
	(SELECT COUNT(*) FROM release_credits rc WHERE rc.release_group_id=rg.id AND rc.role='guest')`
