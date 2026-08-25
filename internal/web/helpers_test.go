package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
	"github.com/crypt0rr/artist-tracker/internal/version"
)

func TestProviderHealthPresentationHelpers(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		p    store.ProviderHealth
		want string
	}{
		{name: "quota", p: store.ProviderHealth{LastFailureAt: &now, QuotaExceeded: true}, want: "quota limited"},
		{name: "rate", p: store.ProviderHealth{LastFailureAt: &now, RateLimited: true}, want: "rate limited"},
		{name: "degraded", p: store.ProviderHealth{LastFailureAt: &now}, want: "degraded"},
		{name: "healthy", p: store.ProviderHealth{LastSuccessAt: &now}, want: "healthy"},
		{name: "failure without success", p: store.ProviderHealth{LastFailureAt: &now, LastSuccessAt: nil}, want: "degraded"},
		{name: "new", p: store.ProviderHealth{}, want: "no success yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerHealthStatus(tc.p); got != tc.want {
				t.Fatalf("providerHealthStatus()=%q, want %q", got, tc.want)
			}
			class := providerHealthClass(tc.p)
			if tc.want == "healthy" && class != "sent" {
				t.Fatalf("healthy class=%q", class)
			}
			if tc.want != "healthy" && class == "sent" {
				t.Fatalf("non-healthy class=%q", class)
			}
		})
	}
	if got := providerDisplayLabel("spotify"); got != "Spotify" {
		t.Fatalf("Spotify label=%q", got)
	}
	if got := providerDisplayLabel("itunes"); got != "iTunes" {
		t.Fatalf("iTunes label=%q", got)
	}
	if got := providerDisplayLabel("musicbrainz"); got != "MusicBrainz" {
		t.Fatalf("MusicBrainz label=%q", got)
	}
	if got := providerHealthTime(nil); got != "" || providerHealthTimeAttr((*time.Time)(nil)) != "" {
		t.Fatalf("nil provider time should be empty: %q", got)
	}
	if got := providerHealthTime(now); !strings.Contains(got, "2026") && now.Year() == 2026 {
		t.Fatalf("provider time=%q", got)
	}
}

func TestSanitizeStatusMessageBoundsAndRemovesControls(t *testing.T) {
	got := sanitizeStatusMessage("  hello\nworld\t" + strings.Repeat("x", 300) + "  ")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("status message retained control characters: %q", got)
	}
	if len([]rune(got)) != 240 {
		t.Fatalf("status message rune length=%d, want 240", len([]rune(got)))
	}
}

func TestStatusQueryRejectsUnsignedMessagesAndAcceptsSignedMessages(t *testing.T) {
	app := &App{cfg: config.Config{SessionSecret: "status test session secret with at least 32 bytes"}}
	unsigned := httptest.NewRequest("GET", "http://example.test/?message=Action+completed+by+attacker", nil)
	if got := app.data(unsigned, "Status").Message; got != "" {
		t.Fatalf("unsigned status message=%q, want empty", got)
	}

	signed := httptest.NewRequest("GET", "http://example.test/?"+app.statusQuery("Action completed"), nil)
	if got := app.data(signed, "Status").Message; got != "Action completed" {
		t.Fatalf("signed status message=%q, want %q", got, "Action completed")
	}

	tampered := httptest.NewRequest("GET", "http://example.test/?"+app.statusQuery("Action completed")+"x", nil)
	if got := app.data(tampered, "Status").Message; got != "" {
		t.Fatalf("tampered status message=%q, want empty", got)
	}
}

func TestQueryURLDoesNotDoubleEscapeFilterValues(t *testing.T) {
	if got := string(queryURL("hip hop & R")); got != "hip+hop+%26+R" {
		t.Fatalf("queryURL()=%q, want %q", got, "hip+hop+%26+R")
	}
}

func TestSafeActionMessagePreservesValidationAndHidesInternalErrors(t *testing.T) {
	if got := safeActionMessage(store.ErrInvalidUsername, "fallback"); !strings.Contains(got, "username") {
		t.Fatalf("validation message=%q, want username guidance", got)
	}
	if got := safeActionMessage(errors.New("UNIQUE constraint failed: users.username secret-token"), "settings could not be saved"); got != "settings could not be saved" {
		t.Fatalf("internal error was reflected: %q", got)
	}
	if got := safeActionMessage(sql.ErrNoRows, "fallback"); got != "The requested item is no longer available." {
		t.Fatalf("not-found message=%q", got)
	}
}

func TestProviderStatusPresentationLabelsNegativeAndStandbyResults(t *testing.T) {
	cases := map[string]string{
		"healthy":        "Healthy",
		"degraded":       "Degraded",
		"standby":        "Standby",
		"not_found":      "Not found",
		"ambiguous":      "Needs review",
		"not_configured": "Not configured",
	}
	for status, want := range cases {
		if got := providerStatusLabel(status); got != want {
			t.Fatalf("providerStatusLabel(%q)=%q, want %q", status, got, want)
		}
	}
	if got := providerStatusClass("not_found"); got != "ambiguous" {
		t.Fatalf("negative status class=%q, want ambiguous", got)
	}
	if got := providerStatusClass("standby"); got != "ambiguous" {
		t.Fatalf("standby status class=%q, want ambiguous", got)
	}
}

func TestProviderHealthPresentationUsesConfiguredCadence(t *testing.T) {
	old := time.Now().UTC().Add(-5 * time.Hour)
	cfg := config.Config{PollInterval: time.Hour, SpotifyPollInterval: 2 * time.Hour}
	status := providerHealthStatusFor(store.ProviderHealth{Provider: "spotify", LastSuccessAt: &old}, cfg)
	if status != "stale" {
		t.Fatalf("Spotify status=%q, want stale after two configured checks", status)
	}
}

func TestLocalReturnPathRejectsExternalAndOutOfScopeValues(t *testing.T) {
	tests := []struct {
		value, prefix, want string
	}{
		{value: "/inbox?state=unread", prefix: "/inbox", want: "/inbox?state=unread"},
		{value: "https://evil.example/", prefix: "/", want: "/"},
		{value: "//evil.example/", prefix: "/", want: "/"},
		{value: "/\\\\evil.example", prefix: "/", want: "/"},
		{value: "/artists", prefix: "/inbox", want: "/inbox"},
		{value: "/inbox-other", prefix: "/inbox", want: "/inbox"},
	}
	for _, test := range tests {
		if got := localReturnPath(test.value, test.prefix, test.want); got != test.want {
			t.Errorf("localReturnPath(%q,%q)=%q, want %q", test.value, test.prefix, got, test.want)
		}
	}
}

func TestProviderTimeValueAcceptsSupportedShapes(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	if got, ok := providerTimeValue(now); !ok || !got.Equal(now) {
		t.Fatalf("time value=%v ok=%v", got, ok)
	}
	if got, ok := providerTimeValue(&now); !ok || !got.Equal(now) {
		t.Fatalf("pointer time value=%v ok=%v", got, ok)
	}
	var nilTime *time.Time
	if _, ok := providerTimeValue(nilTime); ok {
		t.Fatal("nil time pointer was accepted")
	}
	if _, ok := providerTimeValue("2026-08-07"); ok {
		t.Fatal("unsupported time value was accepted")
	}
}

func TestUserTimezoneFormattingUsesLocalDateAndHandlesLegacyValues(t *testing.T) {
	instant := time.Date(2026, time.July, 1, 1, 30, 0, 0, time.UTC)
	if got := formatTime(instant, "America/Los_Angeles"); got != "2026-06-30 18:30 PDT" {
		t.Fatalf("Pacific display=%q, want 2026-06-30 18:30 PDT", got)
	}
	if got := providerHealthTime(instant, "Asia/Tokyo"); !strings.Contains(got, "2026-07-01 10:30:00 JST") {
		t.Fatalf("Tokyo provider display=%q", got)
	}
	if got := formatTime(instant, "Not/AZone"); got != "2026-07-01 01:30 UTC" {
		t.Fatalf("invalid timezone display=%q, want UTC fallback", got)
	}
	payload := providerHealthPayloadForConfig(store.ProviderHealth{Provider: "spotify", LastSuccessAt: &instant, UpdatedAt: instant}, config.Config{}, "America/Los_Angeles")
	if !strings.Contains(payload.LastSuccessDisplay, "2026-06-30 18:30:00 PDT") || !strings.Contains(payload.UpdatedDisplay, "2026-06-30 18:30:00 PDT") {
		t.Fatalf("provider payload timezone display=%q/%q", payload.LastSuccessDisplay, payload.UpdatedDisplay)
	}
}

func TestProviderHealthErrorHidesStaleRetryCountdown(t *testing.T) {
	quota := store.ProviderHealth{
		RateLimited: true,
		LastError:   "Spotify returned 429; retry after 20m",
	}
	if got := providerHealthError(quota); got != "Spotify returned 429" {
		t.Fatalf("quota error=%q", got)
	}
	plain := store.ProviderHealth{LastError: "provider unavailable"}
	if got := providerHealthError(plain); got != "provider unavailable" {
		t.Fatalf("plain error=%q", got)
	}
}

func TestPageFilterAndURLHelpers(t *testing.T) {
	for _, value := range []string{"album", "ep", "single", "read", "dismissed"} {
		if inboxFilter(value, "album", "ep", "single", "read", "dismissed") != value {
			t.Fatalf("inboxFilter(%q) rejected valid value", value)
		}
	}
	if inboxFilter("invalid", "album") != "" || evidenceFilter("INVALID", "open") != "" {
		t.Fatal("invalid filter was accepted")
	}
	if got := inboxPageURL("unread", "spotify", "album", 1); !strings.Contains(got, "source=spotify") || strings.Contains(got, "page=") {
		t.Fatalf("first inbox URL=%q", got)
	}
	if got := inboxPageURL("", "", "", 2); got != "/inbox?page=2" {
		t.Fatalf("paged inbox URL=%q", got)
	}
	if got := inboxReadRedirect("/inbox?state=unread&source=spotify&type=album&page=4"); got != "/inbox?source=spotify&state=unread&type=album" {
		t.Fatalf("inbox read redirect=%q", got)
	}
	if got := evidenceIssuePageURL("confirmed", "read", "date", "high", 2); !strings.Contains(got, "status=confirmed") || !strings.Contains(got, "page=2") {
		t.Fatalf("evidence URL=%q", got)
	}
	if got := coveragePageURL(3); got != "/coverage?page=3" {
		t.Fatalf("coverage URL=%q", got)
	}
	request := httptest.NewRequest("GET", "/artists?q=the+beatles&genre=rock", nil)
	if got := artistsPageURL(request, 2); !strings.Contains(got, "q=the+beatles") || !strings.Contains(got, "page=2") {
		t.Fatalf("artists URL=%q", got)
	}
	for _, value := range []string{"1", "123456", "000"} {
		if !validProviderID(value) {
			t.Fatalf("valid provider ID %q rejected", value)
		}
	}
	for _, value := range []string{"", "abc", "1.2", "-1"} {
		if validProviderID(value) {
			t.Fatalf("invalid provider ID %q accepted", value)
		}
	}
}

func TestSelectedValuesDeduplicatesAndBoundsBatchInputs(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    []string
		wantErr string
	}{
		{name: "deduplicates and trims", values: []string{" one ", "one", "", "two"}, want: []string{"one", "two"}},
		{name: "empty", values: nil, wantErr: "select at least one artist"},
		{name: "too many", values: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, wantErr: "select no more than 10 artists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/artists/follow/batch", nil)
			r.Form = url.Values{"mbids": tc.values}
			got, err := selectedValues(r, "mbids")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("selectedValues error=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("selectedValues=%#v err=%v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestPageStoreErrorProvidesGenericMessage(t *testing.T) {
	app := &App{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest("GET", "/artists", nil)
	data := PageData{}
	if !app.pageStoreError(request, &data, "Artists", "artist list", errors.New("database is locked")) {
		t.Fatal("pageStoreError did not report failure")
	}
	if data.Error == "" || strings.Contains(data.Error, "database") {
		t.Fatalf("page error exposed internal details: %q", data.Error)
	}
	if app.pageStoreError(request, &data, "Artists", "artist list", nil) {
		t.Fatal("nil page error reported failure")
	}
}

func TestProviderHealthReturnsGenericErrorWhenStoreIsUnavailable(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "provider-health-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	app := &App{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodGet, "/admin/provider-health", nil)
	response := httptest.NewRecorder()
	app.providerHealth(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("provider health status=%d body=%q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "database") {
		t.Fatalf("provider health leaked storage details: %q", response.Body.String())
	}
}

func TestTemplatesRenderRepresentativePageData(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "template-smoke.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	cfg := config.Config{SessionSecret: "template smoke session secret with at least 32 bytes", EncryptionKey: "template smoke encryption key with at least 32 bytes"}
	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, database, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	user := &store.User{ID: 1, Email: "template@example.com", Username: "template-user", Role: "admin", Timezone: "UTC", ReminderTime: "09:00", CreatedAt: now}
	artist := store.Artist{ID: 1, MBID: "template-artist", Name: "Template Artist", Type: "Person", Country: "NL", Genres: []string{"Pop"}, ListenCount: 1234, ListenUsers: 56}
	release := store.Release{ID: 1, MBID: "template-release", ArtistID: 1, ArtistName: artist.Name, Title: "Template Album", PrimaryType: "Album", SecondaryTypes: []string{"Live"}, FirstReleaseDate: "2026-09-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/template-release", SpotifyID: "spotify-template", SpotifyURL: "https://open.spotify.com/album/template", ITunesID: "123", ITunesURL: "https://music.apple.com/us/album/template/123", ITunesArtworkURL: "https://is1.mzstatic.com/image/250x250bb.jpg", Source: "both", Confidence: "confirmed", TruthState: "confirmed", TruthProvider: "spotify", FirstObservedAt: now, LastObservedAt: &now}
	providerTime := now.Add(-time.Hour)
	resolution := &store.ArtistResolution{ID: 1, UserID: user.ID, Provider: "spotify", ProviderID: "spotify-template", DisplayName: artist.Name, ProviderURL: "https://open.spotify.com/artist/template", Status: "review", Candidates: []store.ResolutionCandidate{{MBID: artist.MBID, Name: artist.Name, Type: artist.Type, Country: artist.Country, Aliases: []string{"Template"}}}}
	data := PageData{
		PageMeta: PageMeta{
			Title: "Template smoke", Version: version.Current, User: &UserView{ID: user.ID, Email: user.Email, Username: user.Username, Role: user.Role, Timezone: user.Timezone, ReminderTime: user.ReminderTime}, CSRF: "csrf", SetupNeeded: true,
		},
		PageDiscovery: PageDiscovery{
			Query: "template", Artists: []store.Artist{artist}, Results: []catalog.ArtistResult{{MBID: artist.MBID, Name: artist.Name, Type: artist.Type, Country: artist.Country, Aliases: []string{"Template"}}},
			SpotifyResults: []catalog.SpotifyArtist{{ID: "spotify-template", Name: artist.Name, URL: "https://open.spotify.com/artist/template"}},
			ITunesResults:  []catalog.ITunesArtist{{ID: "123", Name: artist.Name, URL: "https://music.apple.com/us/artist/template/123"}}, UpcomingReleases: []store.Release{release}, RecentReleases: []store.Release{release},
			ReleaseCount: 1, FollowCount: 1, ListenBrainzArtists: []store.Artist{artist}, GenreBreakdown: []store.ArtistBreakdown{{Label: "Pop", Count: 1}}, CountryBreakdown: []store.ArtistBreakdown{{Label: "NL", Count: 1}}, TypeBreakdown: []store.ArtistBreakdown{{Label: "Person", Count: 1}},
			ArtistPageStart: 1, ArtistPageEnd: 1, FilteredArtistCount: 1,
		},
		PageRelease: PageRelease{
			CalendarDays: []CalendarDay{{Date: "2026-09-01", Label: "September 1", Today: false, Releases: []store.CalendarRelease{{Release: release, CalendarDate: "2026-09-01"}}}}, CalendarMonth: "September 2026", CalendarPrevMonth: "2026-08", CalendarNextMonth: "2026-10", CalendarICSURL: "/calendar.ics",
			NotificationHolds: []store.NotificationHold{{ID: 1, Title: "Held", Reason: "Review"}}, ReleaseNotificationHolds: []store.NotificationHold{{ID: 1, Title: "Held", Reason: "Review"}},
			ReleaseDetail: &store.ReleaseDetail{Release: release, Observations: []store.ReleaseObservation{{Provider: "spotify", ProviderID: "spotify-template", ObservedAt: now}}}, ReleaseEvidenceIssues: []store.EvidenceIssue{{ID: 1, ReleaseGroupID: release.ID, ArtistName: artist.Name, ReleaseTitle: release.Title, IssueType: "date_conflict", Severity: "warning", Summary: "Review", ReviewState: "unread", Evidence: []store.ReleaseEvidence{{Provider: "spotify", ProviderID: "spotify-template", Title: release.Title, PrimaryType: "Album", FirstReleaseDate: release.FirstReleaseDate, ProviderURL: release.SpotifyURL}}}},
		},
		PageAccount: PageAccount{
			Preferences: store.NotificationPreferences{UserID: user.ID, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true, DigestEnabled: true, DigestFrequency: "daily"}, Resolutions: []store.ArtistResolution{*resolution}, Resolution: resolution, Destinations: []store.Destination{{ID: 1, UserID: user.ID, Name: "Primary", Service: "ntfy", Enabled: true}}, History: []store.DeliveryHistory{{Title: "Template", EventType: "announcement", Destination: "Primary", Status: "sent", CreatedAt: now}}, Import: &store.ImportJob{ID: 1, UserID: user.ID, CreatedAt: now, Added: 1, Rows: []store.ImportRow{{SourceValue: artist.MBID, DisplayName: artist.Name, Status: "added"}}},
			GeneratedURL: "https://example.test/token", Token: "token", TokenKind: "invite", TokenEmail: user.Email,
		},
		PageAdmin: PageAdmin{
			AdminHistory: []store.AdminDeliveryHistory{{DeliveryID: 1, UserEmail: user.Email, Title: "Template", Body: "Body", EventType: "announcement", Destination: "Primary", Service: "ntfy", Status: "sent", Attempts: 1, CreatedAt: now}}, AdminDelivery: &store.AdminDeliveryHistory{UserEmail: user.Email, Title: "Template", Body: "Body", EventType: "announcement", Destination: "Primary", Service: "ntfy", Status: "sent", Attempts: 1},
			AppLogs: []logging.Entry{{Time: now, Level: "INFO", Message: "template smoke", Attributes: []logging.Field{{Key: "count", Value: "1"}}}}, AdminUsers: []store.AdminUser{{ID: user.ID, Email: user.Email, Username: user.Username, Role: user.Role, Timezone: user.Timezone, ReminderTime: user.ReminderTime, CreatedAt: now, FollowCount: 1, DestinationCount: 1}}, AdminArtists: []store.AdminArtist{{ID: artist.ID, Name: artist.Name, MBID: artist.MBID}}, ProviderHealth: []store.ProviderHealth{{Provider: "spotify", LastSuccessAt: &providerTime, UpdatedAt: now}}, ManualSyncs: []store.ManualSyncRequest{{ID: 1, Scope: "artist", Status: "queued", CreatedAt: now}},
			AdminPage: 1, AdminPages: 1,
		},
		PageCoverage: PageCoverage{
			CoverageSummary: store.CoverageSummary{Artists: 1, FreshArtists: 1, ConfirmedReleases: 1}, CoverageArtists: []store.ArtistCoverage{{Artist: artist, OverallStatus: "confirmed", ReleaseCount: 1, ConfirmedReleases: 1, ProviderStatuses: []store.ArtistProviderStatus{{Provider: "spotify", Status: "healthy", ReleaseCount: 1, LastSuccessAt: &providerTime}}}}, CoveragePage: 1, CoveragePages: 1, CoveragePageStart: 1, CoveragePageEnd: 1,
			EvidenceIssues: []store.EvidenceIssue{{ID: 1, ReleaseGroupID: release.ID, ArtistName: artist.Name, ReleaseTitle: release.Title, IssueType: "date_conflict", Severity: "warning", Summary: "Review", ReviewState: "unread", LastSeenAt: now}}, EvidenceIssueCount: 1, EvidenceIssueUnreadCount: 1, EvidenceIssueStatus: "open", EvidenceIssueState: "unread", EvidenceIssuePage: 1, EvidenceIssuePages: 1, EvidenceIssuePageStart: 1, EvidenceIssuePageEnd: 1, EvidenceIssueURL: "/coverage/issues",
		},
		PageInbox: PageInbox{
			InboxItems: []store.ReleaseInboxItem{{Release: release, EventType: "release_day", EventTitle: "Template", EventCreatedAt: now, State: "unread"}}, InboxUnreadCount: 1, InboxCount: 1, InboxState: "unread", InboxPage: 1, InboxPages: 1, InboxPageStart: 1, InboxPageEnd: 1, InboxURL: "/inbox",
		},
	}
	for _, name := range []string{"login", "setup", "token", "admin", "admin_delivery", "artists", "calendar", "coverage", "evidence_issues", "dashboard", "inbox", "release", "resolution", "settings", "import"} {
		var output bytes.Buffer
		if err := app.templates.ExecuteTemplate(&output, name+".html", data); err != nil {
			t.Errorf("template %s failed: %v", name, err)
		}
		if output.Len() == 0 {
			t.Errorf("template %s rendered an empty page", name)
		}
	}
}

// TestDeliveryLogRendersTheFailureDetailItLoads closes the gap between what
// DeliveryHistory fetches and what a member is shown. The query already
// selects the attempt count, the sent time and a redacted last_error, and the
// dashboard already loads ten rows of it - but the template printed only a red
// "failed" badge, so a member who is not the household admin had no way to
// learn why their own release alert failed. The reason was in hand and already
// safe to display.
func TestDeliveryLogRendersTheFailureDetailItLoads(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "delivery-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	cfg := config.Config{SessionSecret: "delivery log session secret with at least 32 bytes", EncryptionKey: "delivery log encryption key with at least 32 bytes"}
	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, database, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	sent := now.Add(3 * time.Minute)
	data := PageData{
		PageMeta: PageMeta{
			Title: "Dashboard", User: &UserView{ID: 1, Email: "member@example.com", Role: "member", Timezone: "UTC"}, CSRF: "csrf",
		},
		PageAccount: PageAccount{History: []store.DeliveryHistory{
			{
				Title: "Failed Album", EventType: "announcement", Destination: "Primary",
				Status: "failed", Attempts: 4, LastError: "destination refused the notification",
				CreatedAt: now,
			},
			{
				Title: "Delivered Album", EventType: "announcement", Destination: "Primary",
				Status: "sent", Attempts: 1, CreatedAt: now, SentAt: &sent,
			},
		}},
	}
	var output bytes.Buffer
	if err := app.templates.ExecuteTemplate(&output, "dashboard.html", data); err != nil {
		t.Fatal(err)
	}
	page := output.String()
	if !strings.Contains(page, "destination refused the notification") {
		t.Fatalf("the delivery log shows a failure with no reason: %s", page)
	}
	if !strings.Contains(page, "4 attempts") {
		t.Fatalf("the delivery log does not show the attempt count it loaded: %s", page)
	}
	if !strings.Contains(page, formatTime(sent, "UTC")) {
		t.Fatalf("the delivery log does not show when a sent notification went out: %s", page)
	}
	// A single-attempt success should stay quiet rather than adding noise.
	if strings.Contains(page, "1 attempts") {
		t.Fatalf("the delivery log labels an unremarkable single attempt: %s", page)
	}
}

// TestCalendarFeedURLCanBeCopied pins a copy affordance onto the one value in
// the product that is shown exactly once. The panel tells a member "Copy this
// URL now" because the token is never displayed again, but offered no way to
// copy it - leaving hand-selecting a long opaque URL from a readonly input as
// the only option, on a value where a partial selection silently produces a
// feed that never loads. The admin invite link, which has the same
// show-once-then-gone shape, has had a Copy button all along.
func TestCalendarFeedURLCanBeCopied(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "feed-copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	cfg := config.Config{SessionSecret: "feed copy session secret with at least 32 bytes", EncryptionKey: "feed copy encryption key with at least 32 bytes"}
	cipher, err := security.NewCipher(cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, database, fakeCatalog{}, nil, fakeSender{}, cipher, fakeArtwork{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	data := PageData{
		PageMeta: PageMeta{Title: "Settings", User: &UserView{ID: 1, Email: "member@example.com", Role: "member", Timezone: "UTC"}, CSRF: "csrf"},
		PageRelease: PageRelease{
			CalendarFeedURL:       "https://tracker.example/calendar/feed/opaque-token-value.ics",
			CalendarFeedActive:    true,
			CalendarFeedExpiresAt: &expires,
		},
	}
	var output bytes.Buffer
	if err := app.templates.ExecuteTemplate(&output, "settings.html", data); err != nil {
		t.Fatal(err)
	}
	page := output.String()
	if !strings.Contains(page, "data-copy") {
		t.Fatalf("the show-once feed URL has no copy control: %s", page)
	}
	// The control has to address this field, or it copies whatever other
	// [data-copy-value] the page happens to render.
	if !strings.Contains(page, `data-copy-target="#calendar-feed-url"`) ||
		!strings.Contains(page, `id="calendar-feed-url"`) {
		t.Fatalf("the copy control is not bound to the feed URL field: %s", page)
	}
}

// TestLapsedPauseIsNotReportedAsPaused covers the pair of defects in
// followRuleSummary. paused_until is never cleared once it lapses, so a nil
// check reported a follow as paused forever after one 7-day pause while the
// delivery engine had already resumed delivering - and the Pause control was
// replaced by "Resume now", whose label promises the opposite of what the
// member wants. The expiry was also the last raw-UTC clock render in the
// product, in Go where the guard that globs *.html cannot reach it.
func TestLapsedPauseIsNotReportedAsPaused(t *testing.T) {
	past := time.Now().UTC().Add(-48 * time.Hour)
	future := time.Now().UTC().Add(48 * time.Hour)

	lapsed := store.FollowNotificationRule{DeliveryMode: store.FollowDeliveryImmediate, PausedUntil: &past}
	if followRulePaused(lapsed) {
		t.Fatal("a lapsed pause still reports as active, so the page removes the Pause control")
	}
	if got := followRuleSummary(lapsed, "UTC"); got != "Immediate only" {
		t.Fatalf("lapsed pause summary=%q, want the delivery mode", got)
	}

	active := store.FollowNotificationRule{DeliveryMode: store.FollowDeliveryImmediate, PausedUntil: &future}
	if !followRulePaused(active) {
		t.Fatal("an active pause reports as not paused")
	}
	summary := followRuleSummary(active, "America/Los_Angeles")
	if !strings.HasPrefix(summary, "Paused until ") {
		t.Fatalf("active pause summary=%q", summary)
	}
	// It must carry a zone label, and render in the reader's zone rather than UTC.
	if !strings.HasSuffix(summary, " PDT") && !strings.HasSuffix(summary, " PST") {
		t.Fatalf("pause expiry %q carries no reader-timezone zone label", summary)
	}
	if got := followRuleSummary(active, "UTC"); !strings.HasSuffix(got, " UTC") {
		t.Fatalf("pause expiry %q carries no zone label", got)
	}
	// A rule that was never paused is unaffected.
	never := store.FollowNotificationRule{DeliveryMode: store.FollowDeliveryDigest}
	if followRulePaused(never) || followRuleSummary(never, "UTC") != "Digest only" {
		t.Fatalf("unpaused rule summary=%q", followRuleSummary(never, "UTC"))
	}
}

// TestArtistsPageOffersPauseAfterAPauseLapses drives the template, because the
// control the member actually needs is chosen there and it tested the same nil
// condition independently of the summary line.
func TestArtistsPageOffersPauseAfterAPauseLapses(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "lapsed-pause-artist", Name: "Lapsed Pause Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	lapsed := time.Now().UTC().Add(-72 * time.Hour)
	if err := database.PauseFollowNotificationRule(ctx, user.ID, artist.ID, &lapsed); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/artists")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(raw)
	if strings.Contains(page, "Paused until") {
		t.Fatalf("the page reports a lapsed pause as still active: %s", page)
	}
	if !strings.Contains(page, "/notification-rule/pause") {
		t.Fatalf("the Pause control is missing after the pause lapsed: %s", page)
	}
}

// TestEveryParsedTemplateIsRendered is #288. destinations.html sat in the
// template set, parsed on every start, with no handler rendering it - its route
// is a pure redirect to /settings. A template nobody renders is a maintenance
// cost that looks like a feature.
func TestEveryParsedTemplateIsRendered(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(".", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := os.ReadFile(filepath.Join(".", "core.go"))
	if err != nil {
		t.Fatal(err)
	}
	sources := string(handlers)
	for _, file := range []string{"dashboard.go", "artists.go", "admin.go", "settings.go", "calendar.go",
		"inbox.go", "evidence.go", "coverage.go", "auth.go", "import.go", "truth.go"} {
		data, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			continue
		}
		sources += string(data)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".html")
		if name == "partials" {
			// Definitions only; included by name from other templates.
			continue
		}
		if !strings.Contains(sources, `"`+name+`"`) {
			t.Errorf("templates/%s is parsed but no handler renders it", entry.Name())
		}
	}
}
