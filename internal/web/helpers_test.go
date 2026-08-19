package web

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/config"
	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
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
		Title: "Template smoke", Version: "0.46.2", User: &UserView{ID: user.ID, Email: user.Email, Username: user.Username, Role: user.Role, Timezone: user.Timezone, ReminderTime: user.ReminderTime}, CSRF: "csrf", SetupNeeded: true, Query: "template",
		Artists: []store.Artist{artist}, Results: []catalog.ArtistResult{{MBID: artist.MBID, Name: artist.Name, Type: artist.Type, Country: artist.Country, Aliases: []string{"Template"}}},
		SpotifyResults: []catalog.SpotifyArtist{{ID: "spotify-template", Name: artist.Name, URL: "https://open.spotify.com/artist/template"}},
		ITunesResults:  []catalog.ITunesArtist{{ID: "123", Name: artist.Name, URL: "https://music.apple.com/us/artist/template/123"}}, UpcomingReleases: []store.Release{release}, RecentReleases: []store.Release{release},
		CalendarDays: []CalendarDay{{Date: "2026-09-01", Label: "September 1", Today: false, Releases: []store.CalendarRelease{{Release: release, CalendarDate: "2026-09-01"}}}}, CalendarMonth: "September 2026", CalendarPrevMonth: "2026-08", CalendarNextMonth: "2026-10", CalendarICSURL: "/calendar.ics",
		ReleaseCount: 1, Preferences: store.NotificationPreferences{UserID: user.ID, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true, DigestEnabled: true, DigestFrequency: "daily"}, NotificationHolds: []store.NotificationHold{{ID: 1, Title: "Held", Reason: "Review"}}, ReleaseNotificationHolds: []store.NotificationHold{{ID: 1, Title: "Held", Reason: "Review"}},
		ReleaseDetail: &store.ReleaseDetail{Release: release, Observations: []store.ReleaseObservation{{Provider: "spotify", ProviderID: "spotify-template", ObservedAt: now}}}, ReleaseEvidenceIssues: []store.EvidenceIssue{{ID: 1, ReleaseGroupID: release.ID, ArtistName: artist.Name, ReleaseTitle: release.Title, IssueType: "date_conflict", Severity: "warning", Summary: "Review", ReviewState: "unread", Evidence: []store.ReleaseEvidence{{Provider: "spotify", ProviderID: "spotify-template", Title: release.Title, PrimaryType: "Album", FirstReleaseDate: release.FirstReleaseDate, ProviderURL: release.SpotifyURL}}}},
		Resolutions: []store.ArtistResolution{*resolution}, Resolution: resolution, Destinations: []store.Destination{{ID: 1, UserID: user.ID, Name: "Primary", Service: "ntfy", Enabled: true}}, History: []store.DeliveryHistory{{Title: "Template", EventType: "announcement", Destination: "Primary", Status: "sent", CreatedAt: now}},
		AdminHistory: []store.AdminDeliveryHistory{{DeliveryID: 1, UserEmail: user.Email, Title: "Template", Body: "Body", EventType: "announcement", Destination: "Primary", Service: "ntfy", Status: "sent", Attempts: 1, CreatedAt: now}}, AdminDelivery: &store.AdminDeliveryHistory{UserEmail: user.Email, Title: "Template", Body: "Body", EventType: "announcement", Destination: "Primary", Service: "ntfy", Status: "sent", Attempts: 1},
		AppLogs: []logging.Entry{{Time: now, Level: "INFO", Message: "template smoke", Attributes: []logging.Field{{Key: "count", Value: "1"}}}}, AdminUsers: []store.AdminUser{{ID: user.ID, Email: user.Email, Username: user.Username, Role: user.Role, Timezone: user.Timezone, ReminderTime: user.ReminderTime, CreatedAt: now, FollowCount: 1, DestinationCount: 1}}, AdminArtists: []store.AdminArtist{{ID: artist.ID, Name: artist.Name, MBID: artist.MBID}}, ProviderHealth: []store.ProviderHealth{{Provider: "spotify", LastSuccessAt: &providerTime, UpdatedAt: now}}, ManualSyncs: []store.ManualSyncRequest{{ID: 1, Scope: "artist", Status: "queued", CreatedAt: now}}, Import: &store.ImportJob{ID: 1, UserID: user.ID, CreatedAt: now, Added: 1, Rows: []store.ImportRow{{SourceValue: artist.MBID, DisplayName: artist.Name, Status: "added"}}},
		FollowCount: 1, ListenBrainzArtists: []store.Artist{artist}, GenreBreakdown: []store.ArtistBreakdown{{Label: "Pop", Count: 1}}, CountryBreakdown: []store.ArtistBreakdown{{Label: "NL", Count: 1}}, TypeBreakdown: []store.ArtistBreakdown{{Label: "Person", Count: 1}}, CoverageSummary: store.CoverageSummary{Artists: 1, FreshArtists: 1, ConfirmedReleases: 1}, CoverageArtists: []store.ArtistCoverage{{Artist: artist, OverallStatus: "confirmed", ReleaseCount: 1, ConfirmedReleases: 1, ProviderStatuses: []store.ArtistProviderStatus{{Provider: "spotify", Status: "healthy", ReleaseCount: 1, LastSuccessAt: &providerTime}}}}, CoveragePage: 1, CoveragePages: 1, CoveragePageStart: 1, CoveragePageEnd: 1,
		EvidenceIssues: []store.EvidenceIssue{{ID: 1, ReleaseGroupID: release.ID, ArtistName: artist.Name, ReleaseTitle: release.Title, IssueType: "date_conflict", Severity: "warning", Summary: "Review", ReviewState: "unread", LastSeenAt: now}}, EvidenceIssueCount: 1, EvidenceIssueUnreadCount: 1, EvidenceIssueStatus: "open", EvidenceIssueState: "unread", EvidenceIssuePage: 1, EvidenceIssuePages: 1, EvidenceIssuePageStart: 1, EvidenceIssuePageEnd: 1, EvidenceIssueURL: "/coverage/issues",
		InboxItems: []store.ReleaseInboxItem{{Release: release, EventType: "release_day", EventTitle: "Template", EventCreatedAt: now, State: "unread"}}, InboxUnreadCount: 1, InboxCount: 1, InboxState: "unread", InboxPage: 1, InboxPages: 1, InboxPageStart: 1, InboxPageEnd: 1, InboxURL: "/inbox",
		AdminPage: 1, AdminPages: 1, ArtistPage: 1, ArtistPages: 1, ArtistPageStart: 1, ArtistPageEnd: 1, FilteredArtistCount: 1, GeneratedURL: "https://example.test/token", Token: "token", TokenKind: "invite", TokenEmail: user.Email,
	}
	for _, name := range []string{"login", "setup", "token", "admin", "admin_delivery", "artists", "calendar", "coverage", "evidence_issues", "dashboard", "inbox", "release", "resolution", "settings", "destinations", "import"} {
		var output bytes.Buffer
		if err := app.templates.ExecuteTemplate(&output, name+".html", data); err != nil {
			t.Errorf("template %s failed: %v", name, err)
		}
		if output.Len() == 0 {
			t.Errorf("template %s rendered an empty page", name)
		}
	}
}
