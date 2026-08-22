package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/store"
	"time"
)

func TestICSEscapeEscapesCalendarControlCharacters(t *testing.T) {
	input := "title\r\nnext\rbare\ncomma,semi;slash\\"
	got := icsEscape(input)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("escaped value contains raw calendar line breaks: %q", got)
	}
	want := `title\nnext\nbare\ncomma\,semi\;slash\\`
	if got != want {
		t.Fatalf("icsEscape() = %q, want %q", got, want)
	}
}

func TestICSLineFoldingAndURIEscaping(t *testing.T) {
	var builder strings.Builder
	writeICSLine(&builder, "SUMMARY:"+strings.Repeat("é", 80))
	lines := strings.Split(strings.TrimSuffix(builder.String(), "\r\n"), "\r\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], " ") {
		t.Fatalf("folded lines=%q", lines)
	}
	for _, line := range lines {
		if len([]byte(line)) > 75 {
			t.Fatalf("line length=%d exceeds 75 octets: %q", len([]byte(line)), line)
		}
	}
	builder.Reset()
	writeICSLine(&builder, "DESCRIPTION:"+strings.Repeat("x", 160))
	for _, line := range strings.Split(strings.TrimSuffix(builder.String(), "\r\n"), "\r\n") {
		if len([]byte(line)) > 75 {
			t.Fatalf("ASCII folded line length=%d exceeds 75 octets", len([]byte(line)))
		}
	}
	uri := icsEscapeURI("https://example.test/a b?x=hello world&name=é#frag ment\nINJECT")
	if uri != "https://example.test/a%20b?x=hello%20world&name=%C3%A9#frag%20mentINJECT" {
		t.Fatalf("URI escape=%q", uri)
	}
	if malformed := icsEscapeURI("https://example.test/a?x=bad%zz"); malformed != "https://example.test/a?x=bad%25zz" {
		t.Fatalf("malformed URI escape=%q", malformed)
	}
}

func TestReleaseExternalLinkPrefersProviderLinks(t *testing.T) {
	if got := releaseExternalLink(store.CalendarRelease{Release: store.Release{SpotifyURL: "https://open.spotify.com/album/1", ITunesURL: "https://music.apple.com/album/1", MusicBrainzURL: "https://musicbrainz.org/release-group/1"}}); got != "https://open.spotify.com/album/1" {
		t.Fatalf("Spotify link preference = %q", got)
	}
	if got := releaseExternalLink(store.CalendarRelease{Release: store.Release{ITunesURL: "https://music.apple.com/album/1", MusicBrainzURL: "https://musicbrainz.org/release-group/1"}}); got != "https://music.apple.com/album/1" {
		t.Fatalf("iTunes link preference = %q", got)
	}
	if got := releaseExternalLink(store.CalendarRelease{Release: store.Release{MusicBrainzURL: "https://musicbrainz.org/release-group/1"}}); got != "https://musicbrainz.org/release-group/1" {
		t.Fatalf("MusicBrainz link fallback = %q", got)
	}
}

func TestCalendarReleaseStatus(t *testing.T) {
	cases := []struct {
		name    string
		release store.CalendarRelease
		want    string
	}{
		{name: "held", release: store.CalendarRelease{Held: true}, want: "held for review"},
		{name: "truth", release: store.CalendarRelease{Release: store.Release{TruthIssueCount: 1}}, want: "review required"},
		{name: "confirmed", release: store.CalendarRelease{Release: store.Release{Confidence: "confirmed"}}, want: "confirmed"},
		{name: "multiple", release: store.CalendarRelease{Release: store.Release{SourceCount: 2}}, want: "confirmed"},
		{name: "single", release: store.CalendarRelease{}, want: "single source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calendarReleaseStatus(tc.release); got != tc.want {
				t.Fatalf("calendarReleaseStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCalendarMonthBoundsUseLocalDatesAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	from, to := calendarMonthBounds(time.Date(2026, time.March, 18, 12, 0, 0, 0, location), location)
	if got := from.Format("2006-01-02 15:04 MST"); got != "2026-03-01 00:00 EST" {
		t.Fatalf("March start=%q", got)
	}
	if got := to.Format("2006-01-02 15:04 MST"); got != "2026-03-31 00:00 EDT" {
		t.Fatalf("March end=%q, want local March 31 midnight after spring-forward", got)
	}

	from, to = calendarMonthBounds(time.Date(2024, time.February, 18, 12, 0, 0, 0, location), location)
	if from.Format("2006-01-02") != "2024-02-01" || to.Format("2006-01-02") != "2024-02-29" {
		t.Fatalf("leap-year bounds=%s..%s, want 2024-02-01..2024-02-29", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestCalendarMonthBoundsDefaultsNilLocationToUTC(t *testing.T) {
	from, to := calendarMonthBounds(time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC), nil)
	if from.Location() != time.UTC || to.Location() != time.UTC {
		t.Fatalf("nil location returned from=%v to=%v", from.Location(), to.Location())
	}
	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("UTC bounds=%s..%s, want 2026-08-01..2026-08-31", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestCalendarFallsBackForInvalidInputsAndMarksToday(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `UPDATE users SET timezone=? WHERE id=?`, "not/a-timezone", user.ID); err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "calendar-today-artist", Name: "Today Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if err := database.ApplyReleaseBatches(ctx, artist, []store.ReleaseBatch{{
		Provider: "itunes",
		Releases: []store.Release{{
			MBID: "itunes:calendar-today", ITunesID: "calendar-today", Title: "Today Release",
			PrimaryType: "Single", FirstReleaseDate: today, DatePrecision: 3,
			ITunesURL: "https://music.apple.com/us/album/today-release",
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/calendar?month=not-a-month")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(page, "Today") ||
		!strings.Contains(page, "Today Release") || !strings.Contains(page, "Release calendar") {
		t.Fatalf("calendar fallback status/body=%d %q", response.StatusCode, page)
	}
}

func TestCalendarFeedFailureDoesNotLogTheRawToken(t *testing.T) {
	// /calendar/feed/{token} carries a year-long unauthenticated read
	// credential in its path, and "path" is not a sensitive key, so logging
	// r.URL.Path would put the token into stdout, the persisted
	// application_logs table, and the admin diagnostics panel.
	database, err := store.Open(filepath.Join(t.TempDir(), "feed-logging.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A closed database makes the token lookup fail rather than simply miss,
	// which is the branch that logs.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app := &App{store: database, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	const token = "feedtoken0123456789abcdef0123456789abcd"
	router := chi.NewRouter()
	router.Get("/calendar/feed/{token}", app.calendarFeed)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/calendar/feed/"+token, nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	written := logs.String()
	if strings.Contains(written, token) {
		t.Fatalf("raw feed token reached the log: %s", written)
	}
	if !strings.Contains(written, "calendar feed token lookup failed") {
		t.Fatalf("the failure was not logged at all: %s", written)
	}
}
