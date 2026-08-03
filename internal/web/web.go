package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
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
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/crypt0rr/artist-tracker/internal/version"
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
	LegacySearch        bool
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

func New(cfg config.Config, s *store.Store, mb catalog.CatalogProvider, spotify catalog.SpotifyProvider,
	sender notify.NotificationSender, cipher *security.Cipher, art artwork.Provider,
	runner *jobs.Runner, logger *slog.Logger, itunesProviders ...catalog.ITunesProvider) (*App, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"join":  strings.Join,
		"lower": strings.ToLower,
		"query": url.QueryEscape,
		"shortDate": func(v string) string {
			if v == "" {
				return "Date unknown"
			}
			return v
		},
		"formatTime":           func(v time.Time) string { return v.Format("2006-01-02 15:04") },
		"formatProviderTime":   providerHealthTime,
		"providerTimeAttr":     providerHealthTimeAttr,
		"providerHealthStatus": providerHealthStatus,
		"providerHealthClass":  providerHealthClass,
		"providerHealthError":  providerHealthError,
		"releaseURL": func(r store.Release) string {
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
	var itunes catalog.ITunesProvider
	if len(itunesProviders) > 0 {
		itunes = itunesProviders[0]
	}
	return &App{
		cfg: cfg, store: s, mb: mb, spotify: spotify, sender: sender,
		itunes: itunes,
		cipher: cipher, artwork: art, jobs: runner, logger: logger, templates: tmpl,
	}, nil
}

func (a *App) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Use(a.securityHeaders)
	r.Use(a.csrf)
	r.Use(a.session)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", a.ready)
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
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
		private.Get("/releases/{id}", a.releaseDetail)
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
		private.Post("/destinations/{id}/delete", a.deleteDestination)
		private.Group(func(admin chi.Router) {
			admin.Use(a.requireAdmin)
			admin.Get("/admin", a.admin)
			admin.Post("/admin/profile", a.profile)
			admin.Post("/admin/invite", a.createInvite)
			admin.Post("/admin/reset", a.createReset)
			admin.Post("/admin/users/{id}/delete", a.deleteUser)
			admin.Post("/admin/sync/retry", a.queueRetrySync)
			admin.Post("/admin/sync/artists/{id}", a.queueArtistSync)
			admin.Get("/admin/provider-health", a.providerHealth)
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; img-src 'self' https://i.scdn.co https://*.mzstatic.com https://*.itunes.apple.com data:; style-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				MaxAge: int((24 * time.Hour).Seconds()),
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

func (a *App) data(r *http.Request, title string) PageData {
	d := PageData{
		Title: title, Version: version.Current, CSRF: r.Context().Value(csrfKey).(string),
		Message: r.URL.Query().Get("message"), SpotifyOn: a.spotify != nil,
	}
	if session, ok := currentSession(r); ok {
		u := session.User
		d.User = &u
	}
	return d
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

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	count, _ := a.store.UserCount(r.Context())
	if count > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, "setup", a.data(r, "Create administrator"), http.StatusOK)
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	count, _ := a.store.UserCount(r.Context())
	if count > 0 {
		http.Error(w, "setup has already completed", http.StatusConflict)
		return
	}
	if a.cfg.SetupToken == "" || subtle.ConstantTimeCompare([]byte(r.FormValue("setup_token")), []byte(a.cfg.SetupToken)) != 1 {
		d := a.data(r, "Create administrator")
		d.Error = "The setup token is incorrect."
		a.render(w, "setup", d, http.StatusForbidden)
		return
	}
	hash, err := security.HashPassword(r.FormValue("password"))
	if err == nil {
		username := strings.TrimSpace(r.FormValue("username"))
		if _, supplied := r.Form["username"]; supplied && username == "" {
			err = store.ErrInvalidUsername
		} else {
			_, err = a.store.CreateUser(r.Context(), r.FormValue("email"), hash, "admin", r.FormValue("timezone"), username)
		}
	}
	if err != nil {
		d := a.data(r, "Create administrator")
		d.Error = err.Error()
		a.render(w, "setup", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Administrator+created", http.StatusSeeOther)
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	count, _ := a.store.UserCount(r.Context())
	d := a.data(r, "Sign in")
	d.SetupNeeded = count == 0
	a.render(w, "login", d, http.StatusOK)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	key := a.clientIP(r) + "|" + email
	allowed, _ := a.store.LoginAllowed(r.Context(), key)
	if !allowed {
		d := a.data(r, "Sign in")
		d.Error = "Too many attempts. Try again in 15 minutes."
		a.render(w, "login", d, http.StatusTooManyRequests)
		return
	}
	user, err := a.store.UserByEmail(r.Context(), email)
	if err != nil || !security.CheckPassword(user.PasswordHash, r.FormValue("password")) {
		_ = a.store.RecordLoginFailure(r.Context(), key)
		time.Sleep(250 * time.Millisecond)
		d := a.data(r, "Sign in")
		d.Error = "Email or password is incorrect."
		a.render(w, "login", d, http.StatusUnauthorized)
		return
	}
	a.store.ClearLoginFailures(r.Context(), key)
	raw, _, err := a.store.CreateSession(r.Context(), user.ID, 30*24*time.Hour)
	if err != nil {
		a.logger.Error("create session", "user_id", user.ID, "error", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "artist_session", Value: security.SignedToken(a.cfg.SessionSecret, raw), Path: "/",
		HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("artist_session"); err == nil {
		if raw, ok := security.VerifySignedToken(a.cfg.SessionSecret, cookie.Value); ok {
			_ = a.store.DeleteSession(r.Context(), raw)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "artist_session", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Dashboard")
	d.FollowCount, _ = a.store.FollowedArtistCount(r.Context(), session.User.ID)
	location, err := time.LoadLocation(session.User.Timezone)
	if err != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format("2006-01-02")
	d.UpcomingReleases, d.RecentReleases, _ = a.store.DashboardReleases(
		r.Context(), session.User.ID, today, 20,
	)
	d.ReleaseCount = len(d.UpcomingReleases) + len(d.RecentReleases)
	d.History, _ = a.store.DeliveryHistory(r.Context(), session.User.ID, 10)
	d.Resolutions, _ = a.store.ArtistResolutions(r.Context(), session.User.ID)
	d.Preferences, _ = a.store.NotificationPreferences(r.Context(), session.User.ID)
	d.ListenBrainzArtists, _ = a.store.TopListenBrainzArtists(r.Context(), session.User.ID, 5)
	d.GenreBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "genre")
	d.CountryBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "country")
	d.TypeBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "type")
	a.render(w, "dashboard", d, http.StatusOK)
}

func (a *App) releaseDetail(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release details")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		a.logger.Debug("release detail unavailable", "release_id", chi.URLParam(r, "id"), "error", "invalid release ID")
		d.ReleaseUnavailable = true
		a.render(w, "release", d, http.StatusNotFound)
		return
	}
	detail, err := a.store.ReleaseDetail(r.Context(), session.User.ID, id)
	if err != nil {
		a.logger.Debug("release detail unavailable", "release_id", id, "error", err)
		d.ReleaseUnavailable = true
		a.render(w, "release", d, http.StatusNotFound)
		return
	}
	d.ReleaseDetail = &detail
	a.render(w, "release", d, http.StatusOK)
}

func (a *App) artists(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Artists")
	d.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	d.LegacySearch = r.URL.Query().Get("legacy") == "1"
	a.populateSearch(r.Context(), &d)
	d.GenreFilter = strings.TrimSpace(r.URL.Query().Get("genre"))
	d.CountryFilter = strings.TrimSpace(r.URL.Query().Get("country"))
	d.TypeFilter = strings.TrimSpace(r.URL.Query().Get("type"))
	d.Artists, _ = a.store.FollowedArtistsFiltered(r.Context(), session.User.ID, d.GenreFilter, d.CountryFilter, d.TypeFilter)
	d.GenreBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "genre")
	d.CountryBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "country")
	d.TypeBreakdown, _ = a.store.FollowedBreakdown(r.Context(), session.User.ID, "type")
	d.FollowCount = len(d.Artists)
	a.render(w, "artists", d, http.StatusOK)
}

func (a *App) syncArtist(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	following, err := a.store.IsFollowing(r.Context(), session.User.ID, id)
	if err != nil || !following {
		http.NotFound(w, r)
		return
	}
	if _, err := a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "artist", &id); err != nil {
		http.Redirect(w, r, "/artists?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/artists?message=Synchronization+queued", http.StatusSeeOther)
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	target := "/artists"
	if query := strings.TrimSpace(r.URL.Query().Get("q")); query != "" {
		target += "?q=" + url.QueryEscape(query) + "&legacy=1"
	} else {
		target += "?legacy=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// populateSearch keeps provider discovery in one place now that the Artists
// page is the single entry point for both discovery and the followed list.
func (a *App) populateSearch(ctx context.Context, d *PageData) {
	if d.Query == "" {
		return
	}
	if a.spotify != nil {
		results, err := a.spotify.SearchArtists(ctx, d.Query)
		if err == nil && len(results) > 0 {
			d.SpotifyResults = results
			return
		}
		if err != nil {
			a.logger.Warn("Spotify artist search failed", "query", d.Query, "error", err)
			if a.itunes == nil {
				d.ProviderNotice = "Spotify is temporarily unavailable; showing MusicBrainz results."
			} else {
				d.ProviderNotice = "Spotify is unavailable; trying Apple/iTunes discovery."
			}
		} else if a.itunes == nil {
			d.ProviderNotice = "No Spotify matches were found; showing MusicBrainz results."
		} else {
			d.ProviderNotice = "No Spotify matches were found; trying Apple/iTunes discovery."
		}
	}
	if a.itunes != nil {
		results, err := a.itunes.SearchArtists(ctx, d.Query)
		if err == nil && len(results) > 0 {
			d.ITunesResults = results
			return
		}
		if err != nil {
			a.logger.Warn("iTunes artist search failed", "query", d.Query, "error", err)
			d.ProviderNotice = "Spotify and Apple/iTunes discovery are unavailable; showing MusicBrainz results."
		} else {
			d.ProviderNotice = "No Spotify or Apple/iTunes matches were found; showing MusicBrainz results."
		}
	}
	results, err := a.mb.SearchArtists(ctx, d.Query, 10)
	if err != nil {
		a.logger.Warn("artist search failed", "query", d.Query, "error", err)
		d.Error = "MusicBrainz is temporarily unavailable. Please try your search again in a moment."
	} else {
		d.Results = results
	}
}

func (a *App) followITunes(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if a.itunes == nil {
		http.Error(w, "iTunes is unavailable", http.StatusBadRequest)
		return
	}
	itunesID := strings.TrimSpace(r.FormValue("itunes_id"))
	if !validProviderID(itunesID) {
		http.Error(w, "invalid iTunes artist ID", http.StatusBadRequest)
		return
	}
	artist, err := a.itunes.Artist(r.Context(), itunesID)
	if err != nil {
		a.logger.Warn("iTunes artist lookup failed", "itunes_id", itunesID, "error", err)
		http.Error(w, "iTunes artist could not be verified", http.StatusBadGateway)
		return
	}
	resolution, created, err := a.store.CreateArtistResolution(
		r.Context(), session.User.ID, "itunes", artist.ID, artist.Name, artist.URL, "",
	)
	if err != nil {
		http.Error(w, "artist could not be queued", http.StatusInternalServerError)
		return
	}
	if created && a.jobs != nil {
		go a.resolveArtistBatch([]store.ArtistResolution{resolution})
	}
	message := "Artist queued for identification"
	if !created {
		message = "Artist is already queued"
	}
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (a *App) followITunesBatch(w http.ResponseWriter, r *http.Request) {
	if a.itunes == nil {
		http.Error(w, "iTunes is unavailable", http.StatusBadRequest)
		return
	}
	session, _ := currentSession(r)
	values, err := selectedValues(r, "itunes_ids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var queued, existing, failed int
	var resolutions []store.ArtistResolution
	for _, value := range values {
		if !validProviderID(value) {
			failed++
			continue
		}
		artist, lookupErr := a.itunes.Artist(r.Context(), value)
		if lookupErr != nil {
			failed++
			continue
		}
		resolution, created, createErr := a.store.CreateArtistResolution(
			r.Context(), session.User.ID, "itunes", artist.ID, artist.Name, artist.URL, "",
		)
		if createErr != nil {
			failed++
			continue
		}
		if created {
			queued++
			resolutions = append(resolutions, resolution)
		} else {
			existing++
		}
	}
	if len(resolutions) > 0 && a.jobs != nil {
		go a.resolveArtistBatch(resolutions)
	}
	message := fmt.Sprintf("%d queued, %d already queued, %d failed", queued, existing, failed)
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func validProviderID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a *App) followSpotify(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if a.spotify == nil {
		http.Error(w, "Spotify is not configured", http.StatusBadRequest)
		return
	}
	spotifyID, ok := catalog.SpotifyID(r.FormValue("spotify_id"))
	if !ok {
		http.Error(w, "invalid Spotify artist ID", http.StatusBadRequest)
		return
	}
	spotifyArtist, err := a.spotify.Artist(r.Context(), spotifyID)
	if err != nil {
		a.logger.Warn("Spotify artist lookup failed", "spotify_id", spotifyID, "error", err)
		http.Error(w, "Spotify artist could not be verified", http.StatusBadGateway)
		return
	}
	resolution, created, err := a.store.CreateArtistResolution(
		r.Context(), session.User.ID, spotifyArtist.ID, spotifyArtist.Name, spotifyArtist.URL, spotifyArtist.ImageURL,
	)
	if err != nil {
		http.Error(w, "artist could not be queued", http.StatusInternalServerError)
		return
	}
	if created {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := a.jobs.ResolveArtistResolutionNow(ctx, resolution); err != nil {
				a.logger.Warn("immediate artist resolution failed", "resolution_id", resolution.ID, "error", err)
			}
		}()
	}
	message := "Artist queued for identification"
	if !created {
		message = "Artist is already queued"
	}
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (a *App) followSpotifyBatch(w http.ResponseWriter, r *http.Request) {
	if a.spotify == nil {
		http.Error(w, "Spotify is not configured", http.StatusBadRequest)
		return
	}
	session, _ := currentSession(r)
	values, err := selectedValues(r, "spotify_ids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var queued, existing, failed int
	var resolutions []store.ArtistResolution
	var spotifyByID map[string]catalog.SpotifyArtist
	batchLookupFailed := false
	if batchProvider, ok := a.spotify.(catalog.SpotifyBatchArtistProvider); ok {
		ids := make([]string, 0, len(values))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			spotifyID, valid := catalog.SpotifyID(value)
			if valid && !seen[spotifyID] {
				seen[spotifyID] = true
				ids = append(ids, spotifyID)
			}
		}
		if len(ids) > 0 {
			spotifyByID = make(map[string]catalog.SpotifyArtist, len(ids))
			artists, lookupErr := batchProvider.Artists(r.Context(), ids)
			if lookupErr != nil {
				batchLookupFailed = true
				a.logger.Warn("Spotify batch artist lookup failed", "count", len(ids), "error", lookupErr)
			} else {
				for _, artist := range artists {
					spotifyByID[artist.ID] = artist
				}
			}
		}
	}
	for _, value := range values {
		spotifyID, ok := catalog.SpotifyID(value)
		if !ok {
			failed++
			continue
		}
		var spotifyArtist catalog.SpotifyArtist
		if spotifyByID != nil {
			var found bool
			spotifyArtist, found = spotifyByID[spotifyID]
			if batchLookupFailed || !found {
				failed++
				continue
			}
		} else {
			var lookupErr error
			spotifyArtist, lookupErr = a.spotify.Artist(r.Context(), spotifyID)
			if lookupErr != nil {
				a.logger.Warn("Spotify batch artist lookup failed", "spotify_id", spotifyID, "error", lookupErr)
				failed++
				continue
			}
		}
		resolution, created, createErr := a.store.CreateArtistResolution(
			r.Context(), session.User.ID, spotifyArtist.ID, spotifyArtist.Name, spotifyArtist.URL, spotifyArtist.ImageURL,
		)
		if createErr != nil {
			failed++
			continue
		}
		if created {
			queued++
			resolutions = append(resolutions, resolution)
		} else {
			existing++
		}
	}
	if len(resolutions) > 0 && a.jobs != nil {
		go a.resolveSpotifyBatch(resolutions)
	}
	message := fmt.Sprintf("%d queued, %d already queued, %d failed", queued, existing, failed)
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (a *App) resolveSpotifyBatch(resolutions []store.ArtistResolution) {
	a.resolveArtistBatch(resolutions)
}

func (a *App) resolveArtistBatch(resolutions []store.ArtistResolution) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, resolution := range resolutions {
		if _, err := a.jobs.ResolveArtistResolutionNow(ctx, resolution); err != nil {
			a.logger.Warn("batch artist resolution failed", "resolution_id", resolution.ID, "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (a *App) follow(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	result, err := a.mb.ResolveArtist(r.Context(), r.FormValue("mbid"))
	if err != nil {
		http.Error(w, "artist could not be resolved", http.StatusBadRequest)
		return
	}
	if a.spotify != nil {
		candidates, _ := a.spotify.SearchArtists(r.Context(), result.Name)
		results := []catalog.ArtistResult{result}
		catalog.Enrich(results, candidates)
		result = results[0]
	}
	artist, err := a.store.UpsertArtist(r.Context(), result.StoreArtist())
	var added bool
	if err == nil {
		added, err = a.store.Follow(r.Context(), session.User.ID, artist.ID)
	}
	if err != nil {
		http.Error(w, "could not follow artist", http.StatusInternalServerError)
		return
	}
	if added && a.jobs != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := a.jobs.SyncArtistNow(ctx, artist); err != nil {
				a.logger.Warn("initial artist sync failed", "artist_id", artist.ID, "error", err)
			}
		}()
	}
	message := "Artist added"
	if !added {
		message = "Artist already followed"
	}
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (a *App) followBatch(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	values, err := selectedValues(r, "mbids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var added, existing, failed int
	var artists []store.Artist
	for _, mbid := range values {
		result, resolveErr := a.mb.ResolveArtist(r.Context(), mbid)
		if resolveErr != nil {
			failed++
			continue
		}
		if a.spotify != nil {
			candidates, _ := a.spotify.SearchArtists(r.Context(), result.Name)
			results := []catalog.ArtistResult{result}
			catalog.Enrich(results, candidates)
			result = results[0]
		}
		artist, storeErr := a.store.UpsertArtist(r.Context(), result.StoreArtist())
		if storeErr != nil {
			failed++
			continue
		}
		created, followErr := a.store.Follow(r.Context(), session.User.ID, artist.ID)
		if followErr != nil {
			failed++
			continue
		}
		if created {
			added++
			artists = append(artists, artist)
		} else {
			existing++
		}
	}
	if len(artists) > 0 && a.jobs != nil {
		go a.syncArtistBatch(artists)
	}
	message := fmt.Sprintf("%d added, %d already followed, %d failed", added, existing, failed)
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (a *App) syncArtistBatch(artists []store.Artist) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, artist := range artists {
		if err := a.jobs.SyncArtistNow(ctx, artist); err != nil {
			a.logger.Warn("initial batch artist sync failed", "artist_id", artist.ID, "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func selectedValues(r *http.Request, name string) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	for _, raw := range r.Form[name] {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, errors.New("select at least one artist")
	}
	if len(result) > 10 {
		return nil, errors.New("select no more than 10 artists")
	}
	return result, nil
}

func (a *App) unfollow(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	_ = a.store.Unfollow(r.Context(), session.User.ID, id)
	http.Redirect(w, r, "/artists?message=Artist+removed", http.StatusSeeOther)
}

func (a *App) artistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolution, err := a.store.ArtistResolution(r.Context(), session.User.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := a.data(r, "Review artist")
	d.Resolution = &resolution
	a.render(w, "resolution", d, http.StatusOK)
}

func (a *App) selectArtistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolution, err := a.store.ArtistResolution(r.Context(), session.User.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var selected *store.ResolutionCandidate
	for i := range resolution.Candidates {
		if resolution.Candidates[i].MBID == r.FormValue("mbid") {
			selected = &resolution.Candidates[i]
			break
		}
	}
	if resolution.Status != "review" || selected == nil {
		http.Error(w, "select one of the reviewed MusicBrainz artists", http.StatusBadRequest)
		return
	}
	if _, err := a.jobs.SelectArtistResolution(r.Context(), resolution, *selected); err != nil {
		http.Error(w, "artist could not be followed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?message=Artist+followed", http.StatusSeeOther)
}

func (a *App) cancelArtistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.CancelArtistResolution(r.Context(), session.User.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/?message=Pending+artist+cancelled", http.StatusSeeOther)
}

func (a *App) exportArtists(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	artists, err := a.store.FollowedArtists(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not export followed artists", http.StatusInternalServerError)
		return
	}
	filename := "artist-trackarr-watched-artists-" + time.Now().UTC().Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		"artist", "display_name", "musicbrainz_id", "musicbrainz_url", "spotify_id", "spotify_url",
	})
	for _, artist := range artists {
		musicBrainzURL := "https://musicbrainz.org/artist/" + artist.MBID
		_ = writer.Write([]string{
			musicBrainzURL, artist.Name, artist.MBID, musicBrainzURL, artist.SpotifyID, artist.SpotifyURL,
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		a.logger.Warn("write artist export failed", "user_id", session.User.ID, "error", err)
	}
}

func (a *App) destinations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	location := "/settings"
	if encoded := query.Encode(); encoded != "" {
		location += "?" + encoded
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (a *App) releaseGroupArt(w http.ResponseWriter, r *http.Request) {
	asset := a.artwork.Get(r.Context(), chi.URLParam(r, "mbid"))
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(asset.MaxAge.Seconds())))
	w.Header().Set("X-Artwork-Status", asset.Status)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.Data)))
	_, _ = w.Write(asset.Data)
}

func (a *App) addDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	input := notify.DestinationInput{
		Service: r.FormValue("service"), RawURL: r.FormValue("raw_url"), Host: r.FormValue("host"),
		Port: r.FormValue("port"), Username: r.FormValue("username"), Password: r.FormValue("password"),
		Token: r.FormValue("token"), Target: r.FormValue("target"), From: r.FormValue("from"),
		To: r.FormValue("to"), Topic: r.FormValue("topic"),
	}
	serviceURL, err := notify.BuildURL(input)
	if err == nil {
		err = a.sender.Validate(serviceURL)
	}
	var encrypted []byte
	if err == nil {
		encrypted, err = a.cipher.Encrypt(serviceURL)
	}
	if err == nil {
		err = a.store.AddDestination(r.Context(), session.User.ID, r.FormValue("name"), input.Service, encrypted)
	}
	if err != nil {
		d := a.data(r, "Settings")
		d.Error = err.Error()
		d.Destinations, _ = a.store.Destinations(r.Context(), session.User.ID)
		d.Preferences, _ = a.store.NotificationPreferences(r.Context(), session.User.ID)
		a.render(w, "settings", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings?message=Destination+added", http.StatusSeeOther)
}

func (a *App) testDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	destination, err := a.store.Destination(r.Context(), session.User.ID, id)
	if err == nil {
		var serviceURL string
		serviceURL, err = a.cipher.Decrypt(destination.EncryptedURL)
		if err == nil {
			err = a.sender.Send(r.Context(), serviceURL, "ArtistTrackarr test", "Your notification destination is working.")
		}
	}
	if err != nil {
		http.Redirect(w, r, "/settings?message="+url.QueryEscape("Test failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?message=Test+sent", http.StatusSeeOther)
}

func (a *App) renameDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.RenameDestination(r.Context(), session.User.ID, id, r.FormValue("name")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		d := a.data(r, "Settings")
		d.Error = err.Error()
		d.Destinations, _ = a.store.Destinations(r.Context(), session.User.ID)
		d.Preferences, _ = a.store.NotificationPreferences(r.Context(), session.User.ID)
		a.render(w, "settings", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings?message=Destination+renamed", http.StatusSeeOther)
}

func (a *App) deleteDestination(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	_ = a.store.DeleteDestination(r.Context(), session.User.ID, id)
	http.Redirect(w, r, "/settings?message=Destination+deleted", http.StatusSeeOther)
}

func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if err := a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time")); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?message=Reminder+settings+updated", http.StatusSeeOther)
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Settings")
	d.Preferences, _ = a.store.NotificationPreferences(r.Context(), session.User.ID)
	d.Destinations, _ = a.store.Destinations(r.Context(), session.User.ID)
	a.render(w, "settings", d, http.StatusOK)
}

func (a *App) settingsProfile(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	var err error
	if _, supplied := r.Form["username"]; supplied {
		err = a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time"), r.FormValue("username"))
	} else {
		err = a.store.UpdateProfile(r.Context(), session.User.ID, r.FormValue("timezone"), r.FormValue("reminder_time"))
	}
	if err != nil {
		d := a.data(r, "Settings")
		d.Error = err.Error()
		d.Preferences, _ = a.store.NotificationPreferences(r.Context(), session.User.ID)
		d.Destinations, _ = a.store.Destinations(r.Context(), session.User.ID)
		a.render(w, "settings", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings?message=Settings+updated", http.StatusSeeOther)
}

func (a *App) settingsPreferences(w http.ResponseWriter, r *http.Request) {
	if err := a.savePreferences(w, r, "/settings"); err != nil {
		return
	}
}

func (a *App) updatePreferences(w http.ResponseWriter, r *http.Request) {
	a.savePreferences(w, r, "/settings")
}

func (a *App) savePreferences(w http.ResponseWriter, r *http.Request, redirectPath string) error {
	session, _ := currentSession(r)
	p := store.NotificationPreferences{UserID: session.User.ID,
		Albums: r.FormValue("albums") == "on", EPs: r.FormValue("eps") == "on", Singles: r.FormValue("singles") == "on",
		Announcements: r.FormValue("announcements") == "on", ReleaseDay: r.FormValue("release_day") == "on"}
	if err := a.store.UpdateNotificationPreferences(r.Context(), p); err != nil {
		http.Redirect(w, r, redirectPath+"?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return err
	}
	http.Redirect(w, r, redirectPath+"?message=Notification+preferences+updated", http.StatusSeeOther)
	return nil
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	a.render(w, "admin", a.adminData(r), http.StatusOK)
}

func (a *App) providerHealth(w http.ResponseWriter, r *http.Request) {
	health, err := a.store.ProviderHealth(r.Context())
	if err != nil {
		http.Error(w, "provider health unavailable", http.StatusInternalServerError)
		return
	}
	response := make([]providerHealthPayload, 0, len(health))
	for _, provider := range health {
		response = append(response, providerHealthPayloadFor(provider))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Debug("provider health response interrupted", "error", err)
	}
}

func (a *App) adminData(r *http.Request) PageData {
	const pageSize = 50
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	count, _ := a.store.AdminDeliveryHistoryCount(r.Context())
	pages := (count + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	d := a.data(r, "Household administration")
	d.AppLogs, _ = a.store.ApplicationLogs(r.Context(), 200)
	if len(d.AppLogs) == 0 {
		if snapshotter, ok := a.logger.Handler().(interface{ Snapshot() []logging.Entry }); ok {
			d.AppLogs = snapshotter.Snapshot()
		}
	}
	d.AdminUsers, _ = a.store.AdminUsers(r.Context())
	d.AdminArtists, _ = a.store.AdminArtists(r.Context())
	d.ProviderHealth, _ = a.store.ProviderHealth(r.Context())
	d.ManualSyncs, _ = a.store.ManualSyncRequests(r.Context(), 20)
	d.AdminHistory, _ = a.store.AdminDeliveryHistory(r.Context(), pageSize, (page-1)*pageSize)
	d.AdminPage, d.AdminPages = page, pages
	if page > 1 {
		d.AdminPrevPage = page - 1
	}
	if page < pages {
		d.AdminNextPage = page + 1
	}
	return d
}

func (a *App) queueRetrySync(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if _, err := a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "retry", nil); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?message=Retry+sync+queued", http.StatusSeeOther)
}
func (a *App) queueArtistSync(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err = a.store.ArtistByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err = a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "artist", &id); err != nil {
		http.Redirect(w, r, "/admin?message="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?message=Artist+sync+queued", http.StatusSeeOther)
}

func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	userID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || userID < 1 {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteUser(r.Context(), session.User.ID, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, store.ErrCannotDeleteSelf) || errors.Is(err, store.ErrLastAdmin) {
			d := a.adminData(r)
			d.Error = err.Error()
			a.render(w, "admin", d, http.StatusBadRequest)
			return
		}
		a.logger.Error("delete user failed", "acting_user_id", session.User.ID, "user_id", userID, "error", err)
		http.Error(w, "user could not be deleted", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?message=User+deleted", http.StatusSeeOther)
}

func (a *App) createInvite(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, "a valid email is required", http.StatusBadRequest)
		return
	}
	if _, err := a.store.UserByEmail(r.Context(), email); err == nil {
		http.Error(w, "that user already exists", http.StatusConflict)
		return
	}
	raw, err := a.store.CreateAuthToken(r.Context(), "invite", email, nil, session.User.ID, 48*time.Hour)
	if err != nil {
		http.Error(w, "could not create invitation", http.StatusBadRequest)
		return
	}
	d := a.adminData(r)
	d.GeneratedURL = a.cfg.PublicURL.ResolveReference(&url.URL{Path: "/invite/" + raw}).String()
	d.TokenKind, d.TokenEmail = "Invitation", strings.TrimSpace(r.FormValue("email"))
	a.render(w, "admin", d, http.StatusOK)
}

func (a *App) createReset(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	user, err := a.store.UserByEmail(r.Context(), r.FormValue("email"))
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	raw, err := a.store.CreateAuthToken(r.Context(), "reset", user.Email, &user.ID, session.User.ID, time.Hour)
	if err != nil {
		http.Error(w, "could not create reset", http.StatusBadRequest)
		return
	}
	d := a.adminData(r)
	d.GeneratedURL = a.cfg.PublicURL.ResolveReference(&url.URL{Path: "/reset/" + raw}).String()
	d.TokenKind, d.TokenEmail = "Password reset", user.Email
	a.render(w, "admin", d, http.StatusOK)
}

func (a *App) tokenForm(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := a.data(r, map[string]string{"invite": "Accept invitation", "reset": "Reset password"}[kind])
		d.Token, d.TokenKind = chi.URLParam(r, "token"), kind
		a.render(w, "token", d, http.StatusOK)
	}
}

func (a *App) acceptInvite(w http.ResponseWriter, r *http.Request) {
	hash, err := security.HashPassword(r.FormValue("password"))
	if err == nil {
		username := strings.TrimSpace(r.FormValue("username"))
		if _, supplied := r.Form["username"]; supplied && username == "" {
			err = store.ErrInvalidUsername
		} else {
			err = a.store.CreateUserFromInvite(r.Context(), chi.URLParam(r, "token"), hash, username, r.FormValue("timezone"))
		}
	}
	if err != nil {
		d := a.data(r, "Accept invitation")
		d.Error, d.Token, d.TokenKind = "Invitation is invalid, expired, or already used: "+err.Error(), chi.URLParam(r, "token"), "invite"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Account+created", http.StatusSeeOther)
}

func (a *App) acceptReset(w http.ResponseWriter, r *http.Request) {
	hash, err := security.HashPassword(r.FormValue("password"))
	var userID *int64
	if err == nil {
		_, userID, err = a.store.ConsumeAuthToken(r.Context(), chi.URLParam(r, "token"), "reset")
		if err == nil && userID == nil {
			err = errors.New("reset token has no user")
		}
		if err == nil {
			err = a.store.UpdatePassword(r.Context(), *userID, hash)
		}
	}
	if err != nil {
		d := a.data(r, "Reset password")
		d.Error, d.Token, d.TokenKind = "Reset link is invalid, expired, or already used: "+err.Error(), chi.URLParam(r, "token"), "reset"
		a.render(w, "token", d, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/login?message=Password+updated", http.StatusSeeOther)
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		if first := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
