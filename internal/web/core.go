package web

import (
	"context"
	"crypto/subtle"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"net/url"
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
	latestFailure := p.LastFailureAt != nil &&
		(p.LastSuccessAt == nil || !p.LastFailureAt.Before(*p.LastSuccessAt))
	if latestFailure {
		switch {
		case p.QuotaExceeded:
			return "quota limited"
		case p.RateLimited:
			return "rate limited"
		default:
			return "degraded"
		}
	}
	if p.LastSuccessAt != nil {
		return "healthy"
	}
	if p.LastFailureAt != nil {
		return "unavailable"
	}
	return "no success yet"
}

func providerHealthClass(p store.ProviderHealth) string {
	switch providerHealthStatus(p) {
	case "healthy":
		return "sent"
	case "quota limited", "rate limited":
		return "ambiguous"
	default:
		return "failed"
	}
}

func providerHealthTime(value any) string {
	t, ok := providerTimeValue(value)
	if !ok || t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05 MST")
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

func providerHealthPayloadFor(p store.ProviderHealth) providerHealthPayload {
	return providerHealthPayload{
		Provider: p.Provider, Status: providerHealthStatus(p), StatusClass: providerHealthClass(p),
		LastSuccessAt: p.LastSuccessAt, LastSuccessDisplay: providerHealthTime(p.LastSuccessAt),
		LastFailureAt: p.LastFailureAt, LastFailureDisplay: providerHealthTime(p.LastFailureAt),
		LastError: providerHealthError(p), NextCheckAt: p.NextCheckAt,
		NextCheckDisplay: providerHealthTime(p.NextCheckAt), UpdatedAt: p.UpdatedAt,
		UpdatedDisplay: providerHealthTime(&p.UpdatedAt), RateLimited: p.RateLimited,
		QuotaExceeded: p.QuotaExceeded,
	}
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
		"query": url.QueryEscape,
		"staticURL": func(path string) string {
			return "/static/" + strings.TrimLeft(path, "/") + "?v=" + url.QueryEscape(version.Current)
		},
		"shortDate": func(v string) string {
			if v == "" {
				return "Date unknown"
			}
			return v
		},
		"formatTime":          func(v time.Time) string { return v.Format("2006-01-02 15:04") },
		"compactCount":        compactCount,
		"followDeliveryLabel": followDeliveryLabel,
		"followRuleSummary":   followRuleSummary,
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
		"formatProviderTime":   providerHealthTime,
		"providerTimeAttr":     providerHealthTimeAttr,
		"timelineKindLabel":    timelineKindLabel,
		"timelineStatusClass":  timelineStatusClass,
		"providerHealthStatus": providerHealthStatus,
		"providerHealthClass":  providerHealthClass,
		"providerHealthError":  providerHealthError,
		"assuranceStatusLabel": assuranceStatusLabel,
		"assuranceStatusClass": assuranceStatusClass,
		"destinationHealthClass": func(status string) string {
			switch strings.ToLower(status) {
			case "healthy":
				return "sent"
			case "paused":
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
		"providerStatusClass": func(status string) string {
			switch status {
			case "healthy":
				return "sent"
			case "failed", "cooldown":
				return "failed"
			case "not_configured":
				return "pending"
			default:
				return "ambiguous"
			}
		},
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
		"releaseURL": func(r store.Release) string {
			switch r.TruthProvider {
			case "spotify":
				if r.SpotifyURL != "" {
					return r.SpotifyURL
				}
			case "itunes":
				if r.ITunesURL != "" {
					return r.ITunesURL
				}
			case "musicbrainz":
				if r.MusicBrainzURL != "" {
					return r.MusicBrainzURL
				}
			}
			if r.SpotifyURL != "" {
				return r.SpotifyURL
			}
			if r.ITunesURL != "" {
				return r.ITunesURL
			}
			return r.MusicBrainzURL
		},
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
		setupLimiter:     newFixedWindowLimiter(10, 15*time.Minute),
		loginLimiter:     newFixedWindowLimiter(20, 5*time.Minute),
		tokenLimiter:     newFixedWindowLimiter(20, 5*time.Minute),
		discoveryLimiter: newFixedWindowLimiter(30, 5*time.Minute),
		importLimiter:    newFixedWindowLimiter(5, time.Hour),
		providerLimiter:  newFixedWindowLimiter(30, 10*time.Minute),
		loginSlots:       make(chan struct{}, 8),
	}, nil
}

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
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Compress(5), middleware.Timeout(90*time.Second), a.requestLogging)
	r.Use(a.securityHeaders)
	r.Use(a.csrf)
	r.Use(a.session)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", a.ready)
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles)))
	r.Handle("/static/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		staticHandler.ServeHTTP(w, r)
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
			admin.Post("/admin/profile", a.profile)
			admin.Post("/admin/invite", a.createInvite)
			admin.Post("/admin/reset", a.createReset)
			admin.Post("/admin/users/{id}/delete", a.deleteUser)
			admin.Post("/admin/sync/retry", a.queueRetrySync)
			admin.Post("/admin/sync/artists/{id}", a.queueArtistSync)
			admin.Get("/admin/provider-health", a.providerHealth)
			admin.Get("/admin/diagnostics", a.diagnostics)
		})
	})
	return r
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
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		const name = "artist_csrf"
		var raw string
		if cookie, err := r.Cookie(name); err == nil {
			raw, _ = security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value)
		}
		if raw == "" {
			raw, _ = security.Token(24)
			http.SetCookie(w, &http.Cookie{
				Name: name, Value: security.SignedToken(a.cfg.SessionSecret, raw), Path: "/",
				HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode,
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
			http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	d := PageData{
		Title: title, Version: version.Current, CSRF: csrf,
		Message: r.URL.Query().Get("message"), SpotifyOn: a.spotify != nil,
	}
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
	d.ArtistPage, d.ArtistPages = page, pages
	if page > 1 {
		d.ArtistPrevPage = page - 1
		d.ArtistPrevURL = artistsPageURL(r, page-1)
	}
	if page < pages {
		d.ArtistNextPage = page + 1
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
	return failed
}

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
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, name+".html", data); err != nil {
		a.logger.Error("render template", "template", name, "error", err)
	}
}
func (a *App) ready(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Healthy(r.Context()); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
