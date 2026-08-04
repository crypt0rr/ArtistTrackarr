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
	cfg       config.Config
	store     *store.Store
	mb        catalog.CatalogProvider
	spotify   catalog.SpotifyProvider
	itunes    catalog.ITunesProvider
	sender    notify.NotificationSender
	cipher    *security.Cipher
	artwork   artwork.Provider
	jobs      *jobs.Runner
	logger    *slog.Logger
	templates *template.Template
}

type PageData struct {
	Title               string
	Version             string
	User                *store.User
	CSRF                string
	Error               string
	Message             string
	SetupNeeded         bool
	Artists             []store.Artist
	Results             []catalog.ArtistResult
	SpotifyResults      []catalog.SpotifyArtist
	ITunesResults       []catalog.ITunesArtist
	UpcomingReleases    []store.Release
	RecentReleases      []store.Release
	ReleaseCount        int
	Preferences         store.NotificationPreferences
	ReleaseDetail       *store.ReleaseDetail
	ReleaseUnavailable  bool
	Resolutions         []store.ArtistResolution
	Resolution          *store.ArtistResolution
	Destinations        []store.Destination
	History             []store.DeliveryHistory
	AdminHistory        []store.AdminDeliveryHistory
	AppLogs             []logging.Entry
	AdminUsers          []store.AdminUser
	AdminArtists        []store.AdminArtist
	ProviderHealth      []store.ProviderHealth
	ManualSyncs         []store.ManualSyncRequest
	Import              *store.ImportJob
	FollowCount         int
	ListenBrainzArtists []store.Artist
	GenreBreakdown      []store.ArtistBreakdown
	CountryBreakdown    []store.ArtistBreakdown
	TypeBreakdown       []store.ArtistBreakdown
	GenreFilter         string
	CountryFilter       string
	TypeFilter          string
	AdminPage           int
	AdminPages          int
	AdminPrevPage       int
	AdminNextPage       int
	Query               string
	GeneratedURL        string
	Token               string
	TokenKind           string
	TokenEmail          string
	SpotifyOn           bool
	ProviderNotice      string
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
