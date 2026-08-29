package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/crypt0rr/artist-tracker/internal/artwork"
	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/crypt0rr/artist-tracker/internal/version"
)

func providerHealthStatus(p store.ProviderHealth) string {
	return store.ProviderHealthStatus(p, time.Now().UTC(), store.ProviderHealthStaleAfter(p.Provider))
}

func providerHealthStatusFor(p store.ProviderHealth, cfg config.Config) string {
	return store.ProviderHealthStatus(p, time.Now().UTC(),
		store.ProviderHealthStaleAfterCadence(p.Provider, cfg.PollInterval, cfg.SpotifyPollInterval))
}

func providerHealthClass(p store.ProviderHealth) string {
	switch providerHealthStatus(p) {
	case "healthy":
		return "sent"
	case "quota limited", "rate limited":
		return "ambiguous"
	case "stale":
		return "ambiguous"
	default:
		return "failed"
	}
}

func formatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const unit = int64(1024)
	if value < unit {
		return strconv.FormatInt(value, 10) + " B"
	}
	amount := float64(value)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	index := 0
	for amount >= float64(unit) && index < len(units)-1 {
		amount /= float64(unit)
		index++
	}
	return strconv.FormatFloat(math.Round(amount*10)/10, 'f', -1, 64) + " " + units[index]
}

func displayLocation(timezones ...string) *time.Location {
	if len(timezones) == 0 || strings.TrimSpace(timezones[0]) == "" {
		return time.Local
	}
	location, err := time.LoadLocation(strings.TrimSpace(timezones[0]))
	if err != nil {
		// Timezone validation happens when profiles are written, but legacy
		// databases can contain invalid values. UTC keeps their display
		// deterministic without affecting stored timestamps or scheduling.
		return time.UTC
	}
	return location
}

// formatTime renders a stored UTC instant in the reader's timezone. The zone
// abbreviation is included so a member can tell which zone a timestamp is in,
// matching providerHealthTime; stored timestamps and scheduling stay in UTC.
func formatTime(value time.Time, timezones ...string) string {
	if len(timezones) == 0 {
		return value.Format("2006-01-02 15:04 MST")
	}
	return value.In(displayLocation(timezones...)).Format("2006-01-02 15:04 MST")
}

func providerHealthTime(value any, timezones ...string) string {
	t, ok := providerTimeValue(value)
	if !ok || t.IsZero() {
		return ""
	}
	return t.In(displayLocation(timezones...)).Format("2006-01-02 15:04:05 MST")
}

func providerHealthTimeAttr(value any) string {
	t, ok := providerTimeValue(value)
	if !ok || t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func providerTimeValue(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed, true
	case *time.Time:
		if typed == nil {
			return time.Time{}, false
		}
		return *typed, true
	default:
		return time.Time{}, false
	}
}

func providerHealthError(p store.ProviderHealth) string {
	message := strings.TrimSpace(p.LastError)
	if p.RateLimited || p.QuotaExceeded {
		lower := strings.ToLower(message)
		if index := strings.Index(lower, "; retry after "); index >= 0 {
			message = strings.TrimSpace(message[:index])
		}
	}
	return message
}

func providerStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy":
		return "Healthy"
	case "failed":
		return "Failed"
	case "degraded":
		return "Degraded"
	case "cooldown":
		return "Cooling down"
	case "not_configured":
		return "Not configured"
	case "standby", "skipped":
		return "Standby"
	case "not_found":
		return "Not found"
	case "ambiguous":
		return "Needs review"
	case "deferred":
		return "Deferred"
	default:
		return "Pending"
	}
}

func providerStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy":
		return "sent"
	case "failed", "degraded", "cooldown":
		return "failed"
	case "not_configured":
		return "pending"
	default:
		return "ambiguous"
	}
}

func assuranceStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy":
		return "Healthy"
	case "delayed":
		return "Delayed"
	case "degraded":
		return "Degraded"
	default:
		return "Pending"
	}
}

func assuranceStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy":
		return "sent"
	case "degraded":
		return "failed"
	case "delayed":
		return "ambiguous"
	default:
		return "pending"
	}
}

func providerHealthPayloadFor(p store.ProviderHealth, timezones ...string) providerHealthPayload {
	return providerHealthPayload{
		Provider: p.Provider, Status: providerHealthStatus(p), StatusClass: providerHealthClass(p),
		LastSuccessAt: p.LastSuccessAt, LastSuccessDisplay: providerHealthTime(p.LastSuccessAt, timezones...),
		LastFailureAt: p.LastFailureAt, LastFailureDisplay: providerHealthTime(p.LastFailureAt, timezones...),
		LastError: providerHealthError(p), NextCheckAt: p.NextCheckAt,
		NextCheckDisplay: providerHealthTime(p.NextCheckAt, timezones...), UpdatedAt: p.UpdatedAt,
		UpdatedDisplay: providerHealthTime(&p.UpdatedAt, timezones...), RateLimited: p.RateLimited,
		QuotaExceeded: p.QuotaExceeded,
	}
}

func providerHealthPayloadForConfig(p store.ProviderHealth, cfg config.Config, timezones ...string) providerHealthPayload {
	payload := providerHealthPayloadFor(p, timezones...)
	payload.Status = providerHealthStatusFor(p, cfg)
	switch payload.Status {
	case "healthy":
		payload.StatusClass = "sent"
	case "quota limited", "rate limited", "stale":
		payload.StatusClass = "ambiguous"
	default:
		payload.StatusClass = "failed"
	}
	return payload
}

// compactCount makes large aggregate counts easier to scan while the exact
// value remains available through the surrounding element's title/label.
func compactCount(value int64) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	amount := float64(value)
	if amount < 0 {
		sign, amount = "-", -amount
	}
	units := []string{"", "k", "M", "B", "T"}
	unit := 0
	for amount >= 1000 && unit < len(units)-1 {
		amount /= 1000
		unit++
	}
	rounded := math.Round(amount*10) / 10
	if rounded >= 1000 && unit < len(units)-1 {
		amount /= 1000
		unit++
		rounded = math.Round(amount*10) / 10
	}
	formatted := strconv.FormatFloat(rounded, 'f', -1, 64)
	return sign + formatted + units[unit]
}

func New(cfg config.Config, s *store.Store, mb catalog.CatalogProvider, spotify catalog.SpotifyProvider,
	sender notify.NotificationSender, cipher *security.Cipher, art artwork.Provider,
	runner *jobs.Runner, logger *slog.Logger, itunesProvider catalog.ITunesProvider) (*App, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"join":  strings.Join,
		"lower": strings.ToLower,
		// QueryEscape returns a complete query component. Marking that
		// component as template.URL prevents html/template from escaping the
		// percent signs a second time (which would turn "hip hop" into the
		// literal filter "hip+hop"). The value is safe because QueryEscape
		// emits only URL query characters.
		"query": queryURL,
		"staticURL": func(path string) string {
			asset := strings.TrimLeft(path, "/")
			cacheKey := version.Current + "-" + staticAssetVersion(asset)
			return "/static/" + asset + "?v=" + url.QueryEscape(cacheKey)
		},
		"shortDate": func(v string) string {
			if v == "" {
				return "Date unknown"
			}
			return v
		},
		"formatTime":          formatTime,
		"compactCount":        compactCount,
		"followDeliveryLabel": followDeliveryLabel,
		"followRuleSummary":   followRuleSummary,
		"followRulePaused":    followRulePaused,
		"calendarStatus":      calendarReleaseStatus,
		"calendarStatusClass": func(release store.CalendarRelease) string {
			status := calendarReleaseStatus(release)
			switch status {
			case "confirmed":
				return "sent"
			case "held for review", "review required":
				return "ambiguous"
			default:
				return "pending"
			}
		},
		"formatProviderTime":     providerHealthTime,
		"providerTimeAttr":       providerHealthTimeAttr,
		"formatBytes":            formatBytes,
		"databaseHealthLabel":    databaseHealthLabel,
		"operationalStatusLabel": store.DiagnosticStatusLabel,
		"operationalStatusClass": func(status string) string {
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "healthy":
				return "sent"
			case "unavailable":
				return "failed"
			default:
				return "ambiguous"
			}
		},
		"timelineKindLabel":    timelineKindLabel,
		"timelineStatusClass":  timelineStatusClass,
		"providerHealthStatus": func(p store.ProviderHealth) string { return providerHealthStatusFor(p, cfg) },
		"providerHealthClass": func(p store.ProviderHealth) string {
			switch providerHealthStatusFor(p, cfg) {
			case "healthy":
				return "sent"
			case "quota limited", "rate limited", "stale":
				return "ambiguous"
			default:
				return "failed"
			}
		},
		"providerHealthError":  providerHealthError,
		"assuranceStatusLabel": assuranceStatusLabel,
		"assuranceStatusClass": assuranceStatusClass,
		"destinationHealthClass": func(status string) string {
			switch strings.ToLower(status) {
			case "healthy":
				return "sent"
			case "paused", "unsupported":
				return "failed"
			default:
				return "ambiguous"
			}
		},
		"destinationHealthLabel": func(status string) string {
			switch strings.ToLower(status) {
			case "healthy":
				return "Healthy"
			case "paused":
				return "Paused"
			case "degraded":
				return "Degraded"
			case "unsupported":
				return "Unsupported"
			default:
				return "Unknown"
			}
		},
		"coverageStatusLabel": func(status string) string {
			switch status {
			case "fresh":
				return "Fresh"
			case "confirmed":
				return "Confirmed"
			case "fallback":
				return "Fallback only"
			case "attention":
				return "Needs attention"
			default:
				return "Pending"
			}
		},
		"coverageStatusClass": func(status string) string {
			switch status {
			case "fresh", "confirmed":
				return "sent"
			case "fallback":
				return "ambiguous"
			case "attention":
				return "failed"
			default:
				return "pending"
			}
		},
		"providerStatusClass": providerStatusClass,
		"providerStatusLabel": providerStatusLabel,
		"providerLabel": func(provider string) string {
			switch strings.ToLower(provider) {
			case "spotify":
				return "Spotify"
			case "itunes":
				return "iTunes"
			default:
				return "MusicBrainz"
			}
		},
		"evidenceIssueTypeLabel": func(issueType string) string {
			switch strings.ToLower(issueType) {
			case "date_conflict":
				return "Date conflict"
			case "title_conflict":
				return "Title conflict"
			case "type_conflict":
				return "Type conflict"
			case "missing_canonical":
				return "Needs canonical confirmation"
			default:
				return "Provider issue"
			}
		},
		"evidenceIssueSeverityClass": func(severity string) string {
			switch strings.ToLower(severity) {
			case "critical":
				return "failed"
			case "warning":
				return "ambiguous"
			default:
				return "pending"
			}
		},
		"releaseConfidenceLabel": func(r store.Release) string {
			switch r.Confidence {
			case "confirmed":
				return "Confirmed"
			case "canonical":
				return "Canonical"
			case "spotify":
				return "Spotify only"
			case "itunes":
				return "iTunes only"
			default:
				return "Unconfirmed"
			}
		},
		"releaseConfidenceClass": func(r store.Release) string {
			switch r.Confidence {
			case "confirmed", "canonical":
				return "sent"
			case "spotify", "itunes":
				return "ambiguous"
			default:
				return "pending"
			}
		},
		"releaseTruthLabel": func(r store.Release) string {
			switch r.TruthState {
			case "confirmed":
				if r.TruthProvider != "" {
					return "Confirmed: " + providerDisplayLabel(r.TruthProvider)
				}
				return "Confirmed"
			case "verified":
				return "Verified"
			case "fallback_confirmed":
				return "Fallback confirmed"
			case "needs_review":
				return "Needs review"
			case "canonical":
				return "Canonical"
			default:
				switch r.Source {
				case "spotify":
					return "Spotify only"
				case "itunes":
					return "iTunes only"
				}
				return "Observed"
			}
		},
		"releaseTruthClass": func(r store.Release) string {
			switch r.TruthState {
			case "confirmed", "verified", "canonical":
				return "sent"
			case "needs_review":
				return "failed"
			case "fallback_confirmed":
				return "ambiguous"
			default:
				if r.Source == "spotify" || r.Source == "itunes" {
					return "ambiguous"
				}
				return "pending"
			}
		},
		"releaseURL": func(r store.Release) string { return store.ReleaseLink(r) },
		"releaseArtwork": func(r store.Release) string {
			if strings.TrimSpace(r.SpotifyImageURL) != "" {
				return r.SpotifyImageURL
			}
			if strings.TrimSpace(r.ITunesArtworkURL) != "" {
				return r.ITunesArtworkURL
			}
			return "/art/release-group/" + url.PathEscape(r.MBID)
		},
		"releaseArtworkFallback": func(r store.Release) string {
			return "/art/release-group/" + url.PathEscape(r.MBID)
		},
		"releaseUsesITunesArtwork": func(r store.Release) bool {
			return strings.TrimSpace(r.SpotifyImageURL) == "" && strings.TrimSpace(r.ITunesArtworkURL) != ""
		},
		"releaseCreditLabel": func(r store.Release) string {
			if strings.EqualFold(strings.TrimSpace(r.ArtistCreditRole), "featured") {
				if r.GuestCreditCount > 0 {
					return "Guest appearance"
				}
				return "Featured appearance"
			}
			return ""
		},
		"creditRoleLabel": func(role string) string {
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "guest":
				return "Guest credit"
			case "featured":
				return "Featured appearance"
			default:
				return "Primary credit"
			}
		},
		"sourceLabel": func(r store.Release) string {
			switch r.Source {
			case "spotify":
				return "Spotify"
			case "itunes":
				return "iTunes"
			case "both":
				return "Multiple sources"
			default:
				return "MusicBrainz"
			}
		},
		"resolutionProviderLabel": func(provider string) string {
			if strings.EqualFold(provider, "itunes") {
				return "Apple/iTunes"
			}
			return "Spotify"
		},
		"initial": func(v string) string {
			for _, r := range v {
				return string(r)
			}
			return "?"
		},
	}).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{
		cfg: cfg, store: s, mb: mb, spotify: spotify, sender: sender,
		itunes: itunesProvider,
		cipher: cipher, artwork: art, jobs: runner, logger: logger, templates: tmpl,
		setupLimiter:            newFixedWindowLimiter(10, 15*time.Minute),
		loginLimiter:            newFixedWindowLimiter(20, 5*time.Minute),
		tokenLimiter:            newFixedWindowLimiter(20, 5*time.Minute),
		discoveryLimiter:        newFixedWindowLimiter(30, 5*time.Minute),
		importLimiter:           newFixedWindowLimiter(5, time.Hour),
		importSlots:             make(chan struct{}, maxConcurrentImports),
		providerLimiter:         newFixedWindowLimiter(30, 10*time.Minute),
		artworkLimiter:          newFixedWindowLimiter(120, time.Minute),
		destinationTestLimiter:  newFixedWindowLimiter(5, 15*time.Minute),
		destinationRetryLimiter: newFixedWindowLimiter(10, 15*time.Minute),
		calendarFeedLimiter:     newFixedWindowLimiter(60, time.Minute),
		passwordChangeLimiter:   newFixedWindowLimiter(10, 15*time.Minute),
		loginSlots:              make(chan struct{}, 8),
	}, nil
}

// queryURL returns one safely escaped query-component value for use in a
// template URL. QueryEscape emits only URL query characters, so marking the
// complete component as template.URL prevents html/template from escaping its
// percent signs a second time.
func queryURL(value string) template.URL { return template.URL(url.QueryEscape(value)) }

func providerDisplayLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "spotify":
		return "Spotify"
	case "itunes":
		return "iTunes"
	default:
		return "MusicBrainz"
	}
}

func timelineKindLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "observation":
		return "Provider observation"
	case "credit":
		return "Artist credit"
	case "evidence":
		return "Evidence review"
	case "decision":
		return "Truth decision"
	case "notification":
		return "Notification"
	case "delivery":
		return "Delivery"
	case "hold":
		return "Notification hold"
	case "rule":
		return "Follow rule"
	case "inbox":
		return "Inbox state"
	default:
		return "Release activity"
	}
}

func timelineStatusClass(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch {
	case status == "sent", status == "confirmed", status == "observed", status == "active", status == "recorded", status == "released", status == "read":
		return "sent"
	case status == "failed", status == "discarded", strings.Contains(status, "critical"):
		return "failed"
	case status == "pending", status == "held", status == "snoozed", strings.Contains(status, "warning"):
		return "ambiguous"
	default:
		return "pending"
	}
}
func (a *App) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, a.recoverPanic, middleware.Compress(5), middleware.Timeout(90*time.Second), a.requestLogging)
	r.Use(a.securityHeaders)
	r.Use(a.csrf)
	r.Use(a.session)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", a.ready)
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles)))
	r.Handle("/static/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		// chi's compression middleware wraps this handler.  A byte range from
		// http.FileServer refers to the uncompressed representation, so serving
		// it through gzip would produce an invalid Content-Range response.  The
		// assets are small and immutable; disable ranges at this boundary and
		// let the normal full-response compression path handle them safely.
		if r.Header.Get("Range") != "" {
			r = r.Clone(r.Context())
			r.Header = r.Header.Clone()
			r.Header.Del("Range")
		}
		staticHandler.ServeHTTP(&staticResponseWriter{ResponseWriter: w}, r)
	}))
	r.Get("/setup", a.setupForm)
	r.Post("/setup", a.setup)
	r.Get("/login", a.loginForm)
	r.Post("/login", a.login)
	r.Get("/invite/{token}", a.tokenForm("invite"))
	r.Post("/invite/{token}", a.acceptInvite)
	r.Get("/reset/{token}", a.tokenForm("reset"))
	r.Post("/reset/{token}", a.acceptReset)

	r.Group(func(private chi.Router) {
		private.Use(a.requireUser)
		private.Get("/", a.dashboard)
		private.Post("/logout", a.logout)
		private.Get("/artists", a.artists)
		private.Get("/calendar", a.calendar)
		private.Get("/calendar.ics", a.calendarICS)
		private.Post("/settings/calendar-feed", a.calendarFeedAction)
		private.Get("/inbox", a.inbox)
		private.Post("/inbox/{id}/{action}", a.inboxStateAction)
		private.Post("/notifications/holds/{id}/{action}", a.notificationHoldAction)
		private.Get("/coverage", a.coverage)
		private.Get("/coverage/issues", a.evidenceIssues)
		private.Post("/coverage/issues/{id}/{action}", a.evidenceIssueStateAction)
		private.Post("/coverage/artists/{id}/sync", a.queueCoverageSync)
		private.Get("/releases/{id}", a.releaseDetail)
		private.Post("/releases/{id}/truth", a.releaseTruthAction)
		private.Get("/artists/search", a.search)
		private.Get("/settings", a.settings)
		private.Post("/settings/profile", a.settingsProfile)
		private.Post("/settings/password", a.settingsPassword)
		private.Post("/settings/preferences", a.settingsPreferences)
		private.Post("/artists/follow", a.follow)
		private.Post("/artists/follow/batch", a.followBatch)
		private.Post("/artists/follow/spotify", a.followSpotify)
		private.Post("/artists/follow/spotify/batch", a.followSpotifyBatch)
		private.Post("/artists/follow/itunes", a.followITunes)
		private.Post("/artists/follow/itunes/batch", a.followITunesBatch)
		private.Post("/artists/notification-rules/batch", a.updateArtistNotificationRuleBatch)
		private.Post("/artists/{id}/notification-rule", a.updateArtistNotificationRule)
		private.Post("/artists/{id}/notification-rule/pause", a.pauseArtistNotifications)
		private.Post("/artists/{id}/notification-rule/resume", a.resumeArtistNotifications)
		private.Post("/artists/{id}/delete", a.unfollow)
		private.Post("/artists/{id}/sync", a.syncArtist)
		private.Get("/artist-resolutions/{id}", a.artistResolution)
		private.Post("/artist-resolutions/{id}", a.selectArtistResolution)
		private.Post("/artist-resolutions/{id}/cancel", a.cancelArtistResolution)
		private.Get("/artists/export", a.exportArtists)
		private.Post("/artists/import", a.importArtists)
		private.Get("/artists/imports/{id}", a.artistImport)
		private.Post("/artists/imports/{id}/resume", a.resumeArtistImport)
		private.Get("/art/release-group/{mbid}", a.releaseGroupArt)
		private.Get("/destinations", a.destinations)
		private.Post("/preferences", a.updatePreferences)
		private.Post("/destinations", a.addDestination)
		private.Post("/destinations/{id}/rename", a.renameDestination)
		private.Post("/destinations/{id}/test", a.testDestination)
		private.Post("/destinations/{id}/retry", a.retryDestination)
		private.Post("/destinations/{id}/delete", a.deleteDestination)
		private.Group(func(admin chi.Router) {
			admin.Use(a.requireAdmin)
			admin.Get("/admin", a.admin)
			admin.Get("/admin/deliveries/{id}", a.adminDeliveryDetail)
			admin.Get("/admin/digest-deliveries/{id}", a.adminDigestDeliveryDetail)
			admin.Post("/admin/profile", a.profile)
			admin.Post("/admin/invite", a.createInvite)
			admin.Post("/admin/reset", a.createReset)
			admin.Post("/admin/users/{id}/delete", a.deleteUser)
			admin.Post("/admin/sync/retry", a.queueRetrySync)
			admin.Post("/admin/sync/artists/{id}", a.queueArtistSync)
			admin.Get("/admin/provider-health", a.providerHealth)
			admin.Get("/admin/diagnostics", a.diagnostics)
			admin.Get("/admin/diagnostics.json", a.diagnosticsJSON)
			admin.Post("/admin/deliveries/repair-clock-skew", a.repairClockSkewedDeliveries)
			admin.Get("/admin/retention/export", a.exportAdminDeliveryHistory)
			admin.Post("/admin/retention/cleanup", a.cleanupRetention)
		})
	})
	r.Get("/calendar/feed/{token}", a.calendarFeed)
	return r
}

// staticResponseWriter prevents http.FileServer from advertising byte ranges
// for compressed embedded assets. The handler strips incoming ranges, but
// FileServer otherwise adds Accept-Ranges: bytes when it commits the response.
type staticResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *staticResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Del("Content-Range")
	w.ResponseWriter.WriteHeader(status)
}

func (w *staticResponseWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.ResponseWriter.Write(data)
}
func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(), payment=()")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if a.cfg.PublicURL != nil && strings.EqualFold(a.cfg.PublicURL.Scheme, "https") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; img-src 'self' https://i.scdn.co https://*.mzstatic.com https://*.itunes.apple.com data:; style-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// recoverPanic replaces chi's Recoverer so a handler panic reaches the same
// places every other failure does. chi writes to its own standard-library
// logger, so a panic bypassed structured logging, the redaction applied to
// every other message, the persisted application log that the admin
// diagnostics page reads, and the metrics - leaving an operator with a 500
// response and no record anywhere of what happened.
//
// The route pattern is logged rather than the request path: a path can carry a
// credential, as /calendar/feed/{token} does.
func (a *App) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// The client went away mid-response; net/http expects this to
			// propagate rather than be reported as a server fault. Match with
			// errors.Is so a wrapped ErrAbortHandler propagates too.
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			message := notify.RedactError(fmt.Errorf("%v", recovered))
			if len(message) > 512 {
				message = message[:512] + "..."
			}
			stack := notify.RedactError(errors.New(string(debug.Stack())))
			if len(stack) > 2048 {
				stack = stack[:2048] + "..."
			}
			route := "unknown"
			if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
				route = rctx.RoutePattern()
			}
			if a.logger != nil {
				a.logger.Error("handler panic recovered",
					"scope", "http handler", "method", r.Method, "route", route,
					"panic", message, "stack", stack)
			}
			http.Error(w, "something went wrong", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(wrapped, r)
		route := "unknown"
		if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
			if pattern := routeContext.RoutePattern(); pattern != "" {
				route = pattern
			}
		}
		if a.logger != nil {
			a.logger.Debug("http request completed",
				"request_id", middleware.GetReqID(r.Context()),
				"method", r.Method, "route", route, "status", wrapped.Status(),
				"bytes", wrapped.BytesWritten(), "duration", time.Since(started).String())
		}
	})
}
func (a *App) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/calendar/feed/") {
			next.ServeHTTP(w, r)
			return
		}
		const name = "artist_csrf"
		var raw string
		if cookie, err := r.Cookie(name); err == nil {
			var valid bool
			raw, valid = security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value)
			if !valid {
				raw = ""
			}
		}
		if raw == "" {
			raw, _ = security.Token(24)
			// Lax, matching artist_session deliberately. Strict withheld this
			// cookie on cross-site top-level navigations that still carried the
			// session cookie, and the branch above cannot tell "no token yet"
			// from "token withheld", so it minted a fresh one and silently
			// invalidated the token held by every page already open. Arriving
			// from a homelab dashboard tile, a webmail tab, or this app's own
			// ICS event links - which point at {PublicURL}/releases/{id} - cost
			// the member their in-progress form input and a bare 403.
			//
			// Strict was not what stopped forgery here: Lax still withholds the
			// cookie on a cross-site POST, so the compare below fails exactly as
			// before, and artist_session being Lax already means a cross-site
			// POST is unauthenticated regardless.
			http.SetCookie(w, &http.Cookie{
				Name: name, Value: security.SignedToken(a.cfg.SessionSecret, raw), Path: "/",
				HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
				MaxAge: int(sessionLifetime.Seconds()),
			})
		}
		if r.Method == http.MethodPost {
			var err error
			if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
				err = r.ParseMultipartForm(2 << 20)
			} else {
				err = r.ParseForm()
			}
			if err != nil || subtle.ConstantTimeCompare([]byte(r.FormValue("_csrf")), []byte(raw)) != 1 {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, raw)))
	})
}
func (a *App) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("artist_session")
		if err == nil {
			raw, ok := security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value)
			if ok {
				session, err := a.store.Session(r.Context(), raw)
				if err == nil {
					r = r.WithContext(context.WithValue(r.Context(), sessionKey, session))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := currentSession(r); !ok {
			// Remember what was asked for. The application publishes an external
			// deep link - every ICS event carries a URL: property resolving to
			// {PublicURL}/releases/{id} - and the whole point of the revocable
			// feed token is that the calendar is read on devices separate from
			// the browser session, so following one of those links from a phone
			// used to land on the dashboard with no way back to the release.
			//
			// Only safe local paths are carried, through the same allowlisting
			// helper the rest of the app uses, and only for GET: replaying a POST
			// after a login prompt would repeat a side effect the member never
			// re-confirmed.
			target := "/login"
			if r.Method == http.MethodGet {
				if next := localReturnPath(r.URL.RequestURI(), "", ""); next != "" && next != "/" {
					target += "?next=" + url.QueryEscape(next)
				}
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := currentSession(r)
		if !ok || session.User.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func currentSession(r *http.Request) (store.Session, bool) {
	session, ok := r.Context().Value(sessionKey).(store.Session)
	return session, ok
}

const sessionLifetime = 30 * 24 * time.Hour

// localReturnPath accepts only an absolute path on this application. It
// rejects scheme-relative URLs, hosts, control characters, and paths outside
// the intended workflow so form redirects cannot become open redirects.
func localReturnPath(value, prefix, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\\r\n") || strings.HasPrefix(value, "//") || !strings.HasPrefix(value, "/") {
		return fallback
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path == "" || strings.HasPrefix(parsed.Path, "//") {
		return fallback
	}
	if prefix != "" && parsed.Path != prefix && !strings.HasPrefix(parsed.Path, strings.TrimSuffix(prefix, "/")+"/") {
		return fallback
	}
	return parsed.RequestURI()
}

func (a *App) data(r *http.Request, title string) PageData {
	csrf, _ := r.Context().Value(csrfKey).(string)
	returnPath := localReturnPath(r.URL.Query().Get("next"), "", "")
	message := ""
	if raw := r.URL.Query().Get("message"); raw != "" {
		if value, ok := security.VerifySignedToken(a.cfg.SessionSecret, raw); ok {
			message = sanitizeStatusMessage(value)
		}
	}
	d := PageData{PageMeta: PageMeta{
		Title: title, Version: version.Current, CSRF: csrf,
		Message: message, SpotifyOn: a.spotify != nil,
	}}
	d.ReturnPath = returnPath
	if session, ok := currentSession(r); ok {
		d.User = &UserView{
			ID: session.User.ID, Email: session.User.Email, Username: session.User.Username,
			Role: session.User.Role, Timezone: session.User.Timezone, ReminderTime: session.User.ReminderTime,
		}
		if count, err := a.store.ReleaseInboxUnreadCount(r.Context(), session.User.ID, time.Now().UTC()); err != nil {
			// The badge is optional navigation chrome. Keep the page usable when
			// its count lookup is temporarily unavailable, while retaining a
			// structured diagnostic for operators.
			a.logger.Error("navigation inbox count lookup failed", "path", r.URL.Path,
				"request_id", middleware.GetReqID(r.Context()), "error", err)
		} else {
			d.InboxUnreadCount = count
		}
	}
	return d
}

// statusQuery creates an integrity-protected flash value for redirects. A
// plain query parameter would let anyone make the trusted status banner claim
// that an action succeeded (or inject provider-controlled text into it). The
// message remains short-lived in the URL, but only values signed with the
// application session secret are rendered by data.
func (a *App) statusQuery(message string) string {
	message = sanitizeStatusMessage(message)
	return "message=" + url.QueryEscape(security.SignedToken(a.cfg.SessionSecret, message))
}

// sanitizeStatusMessage keeps redirect banners useful while preventing an
// arbitrary query string from becoming an unbounded reflected status block.
// Templates still escape the result; this additionally removes control
// characters and caps the amount of text rendered in every page header.
func sanitizeStatusMessage(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	clean := runes[:0]
	for _, r := range runes {
		if r < 0x20 || r == 0x7f {
			continue
		}
		clean = append(clean, r)
		if len(clean) == 240 {
			break
		}
	}
	return strings.TrimSpace(string(clean))
}

// safeActionMessage keeps form validation useful without reflecting storage,
// provider, or driver errors into a redirect banner. Store errors are normally
// detailed enough for operators through the structured log, but action
// responses should only expose the small set of stable validation outcomes a
// user can correct.
func safeActionMessage(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "The requested item is no longer available."
	case errors.Is(err, store.ErrCannotDeleteSelf), errors.Is(err, store.ErrLastAdmin),
		errors.Is(err, store.ErrManualSyncQueueFull), errors.Is(err, store.ErrDestinationLimit),
		errors.Is(err, store.ErrArtistResolutionLimit), errors.Is(err, store.ErrInvalidUsername),
		errors.Is(err, store.ErrUsernameTaken), errors.Is(err, store.ErrInvalidNotificationHoldAction):
		return err.Error()
	case errors.Is(err, store.ErrInvalidReleaseTruthProvider), errors.Is(err, store.ErrReleaseTruthProviderUnavailable):
		return "That provider is not available for this release."
	case errors.Is(err, notify.ErrUnsupportedTransport):
		return "This notification transport is not supported."
	}
	switch strings.TrimSpace(err.Error()) {
	case "a valid email address is required", "invalid IANA timezone", "reminder time must use HH:MM",
		"password must be at least 12 characters", "destination name is required",
		"destination name must be 80 characters or fewer", "invalid follow notification delivery mode",
		"select at least one followed artist", "user is required":
		return strings.TrimSpace(err.Error())
	default:
		return fallback
	}
}

// pageStoreError keeps database/provider details in structured logs while
// giving the user a stable, non-sensitive page-level error. The caller uses
// the return value to send an HTTP 500 for incomplete page loads.
func (a *App) pageStoreError(r *http.Request, d *PageData, page, operation string, err error) bool {
	if err == nil {
		return false
	}
	a.logger.Error("page data lookup failed", "page", page, "operation", operation,
		"path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()), "error", err)
	if d.Error == "" {
		d.Error = "We couldn't load this page right now. Please try again."
	}
	return true
}
func (a *App) loadSettingsData(r *http.Request, d *PageData) bool {
	session, ok := currentSession(r)
	if !ok {
		return false
	}
	failed := false
	var err error
	d.Preferences, err = a.store.NotificationPreferences(r.Context(), session.User.ID)
	failed = a.pageStoreError(r, d, "Settings", "notification preferences", err) || failed
	d.Destinations, err = a.store.Destinations(r.Context(), session.User.ID)
	failed = a.pageStoreError(r, d, "Settings", "notification destinations", err) || failed
	d.DestinationHealth, err = a.store.DestinationHealthByUser(r.Context(), session.User.ID)
	failed = a.pageStoreError(r, d, "Settings", "destination health", err) || failed
	feed, feedErr := a.store.CalendarFeedTokenStatus(r.Context(), session.User.ID)
	if errors.Is(feedErr, sql.ErrNoRows) {
		return failed
	}
	failed = a.pageStoreError(r, d, "Settings", "calendar feed token", feedErr) || failed
	if feedErr == nil {
		d.CalendarFeedExpiresAt = &feed.ExpiresAt
		d.CalendarFeedActive = feed.Active
	}
	return failed
}
func (a *App) loadArtistsData(r *http.Request, d *PageData) bool {
	session, ok := currentSession(r)
	if !ok {
		return false
	}
	const pageSize = 50
	d.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	if d.Query != "" {
		key := strconv.FormatInt(session.User.ID, 10) + "|" + a.clientIP(r)
		if a.discoveryLimiter != nil && !a.discoveryLimiter.Allow(key) {
			d.Error = "Artist search is temporarily rate limited. Please try again in a few minutes."
		} else {
			a.populateSearch(r.Context(), d)
		}
	}
	d.GenreFilter = strings.TrimSpace(r.URL.Query().Get("genre"))
	d.CountryFilter = strings.TrimSpace(r.URL.Query().Get("country"))
	d.TypeFilter = strings.TrimSpace(r.URL.Query().Get("type"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	failed := false
	var err error
	d.FollowCount, err = a.store.FollowedArtistCount(r.Context(), session.User.ID)
	failed = a.pageStoreError(r, d, "Artists", "followed artist count", err) || failed
	d.FilteredArtistCount, err = a.store.FollowedArtistsFilteredCount(r.Context(), session.User.ID, d.GenreFilter, d.CountryFilter, d.TypeFilter)
	failed = a.pageStoreError(r, d, "Artists", "filtered artist count", err) || failed
	pages := (d.FilteredArtistCount + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	// Only the URLs and the page links are rendered; the bare page numbers were
	// computed and never referenced by any template.
	if page > 1 {
		d.ArtistPrevURL = artistsPageURL(r, page-1)
	}
	if page < pages {
		d.ArtistNextURL = artistsPageURL(r, page+1)
	}
	if d.FilteredArtistCount > 0 {
		d.ArtistPageStart = (page-1)*pageSize + 1
		d.ArtistPageEnd = page * pageSize
		if d.ArtistPageEnd > d.FilteredArtistCount {
			d.ArtistPageEnd = d.FilteredArtistCount
		}
	}
	if pages > 1 {
		d.ArtistPageLinks = make([]PaginationLink, 0, pages)
		for number := 1; number <= pages; number++ {
			d.ArtistPageLinks = append(d.ArtistPageLinks, PaginationLink{
				Number: number, URL: artistsPageURL(r, number), Current: number == page,
			})
		}
	}
	d.Artists, err = a.store.FollowedArtistsFilteredPage(r.Context(), session.User.ID, d.GenreFilter, d.CountryFilter, d.TypeFilter, pageSize, (page-1)*pageSize)
	failed = a.pageStoreError(r, d, "Artists", "followed artist list", err) || failed
	artistIDs := make([]int64, 0, len(d.Artists))
	for _, artist := range d.Artists {
		artistIDs = append(artistIDs, artist.ID)
	}
	d.FollowRules, err = a.store.FollowNotificationRules(r.Context(), session.User.ID, artistIDs)
	failed = a.pageStoreError(r, d, "Artists", "follow notification rules", err) || failed
	d.GenreBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "genre")
	failed = a.pageStoreError(r, d, "Artists", "genre breakdown", err) || failed
	d.CountryBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "country")
	failed = a.pageStoreError(r, d, "Artists", "country breakdown", err) || failed
	d.TypeBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "type")
	failed = a.pageStoreError(r, d, "Artists", "artist type breakdown", err) || failed
	// Recent imports live beside the import form because that is where a member
	// looks after an import stops reporting. Without the listing a job that a
	// restart marked resumable was unreachable: Resume is addressed by job id
	// and nothing ever showed one.
	d.ArtistListQuery = currentArtistListQuery(d, page)
	d.ImportJobs, err = a.store.RecentImportJobs(r.Context(), session.User.ID, recentImportJobLimit)
	failed = a.pageStoreError(r, d, "Artists", "recent imports", err) || failed
	return failed
}

// currentArtistListQuery encodes the view a member is looking at so action
// forms can carry it and the redirect afterwards can restore it. Only the keys
// the artists page itself reads are included, and the first page is omitted
// because it is the default.
func currentArtistListQuery(d *PageData, page int) string {
	values := url.Values{}
	// "q" is excluded on purpose; see artistListQuery. It drives discovery, not
	// the followed list, so carrying it through an action re-runs provider
	// searches for a purely local change.
	for key, value := range map[string]string{
		"genre":   d.GenreFilter,
		"country": d.CountryFilter,
		"type":    d.TypeFilter,
	} {
		if value != "" {
			values.Set(key, value)
		}
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return values.Encode()
}

// recentImportJobLimit bounds the import history shown on the artists page. It
// is a convenience list for finding a job again, not an audit log; the detail
// page holds the per-row outcome.
const recentImportJobLimit = 5

func artistsPageURL(r *http.Request, page int) string {
	values := make(url.Values)
	for _, key := range []string{"q", "genre", "country", "type"} {
		if value := strings.TrimSpace(r.URL.Query().Get(key)); value != "" {
			values.Set(key, value)
		}
	}
	values.Set("page", strconv.Itoa(page))
	return "/artists?" + values.Encode()
}
func (a *App) render(w http.ResponseWriter, name string, data PageData, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Authenticated pages contain household data and must never be reused from
	// a browser or shared cache after a session changes. Static assets use the
	// separate immutable handler above.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, name+".html", data); err != nil {
		a.logger.Error("render template", "template", name, "error", err)
	}
}
func (a *App) ready(w http.ResponseWriter, r *http.Request) {
	if err := a.readyCheck(r.Context()); err != nil {
		var healthErr *store.DatabaseHealthError
		state := store.DatabaseUnavailable
		if errors.As(err, &healthErr) && healthErr != nil && healthErr.State != "" {
			state = healthErr.State
		}
		w.Header().Set("X-ArtistTrackarr-Database", string(state))
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	// Readiness is a bounded unauthenticated database/schema/write probe.
	// Detailed queue, provider, backup, and runner state belongs behind the
	// authenticated diagnostics/admin pages and must not be exposed through
	// probe headers.
	w.Header().Set("X-ArtistTrackarr-Database", "healthy")
	w.WriteHeader(http.StatusNoContent)
}
