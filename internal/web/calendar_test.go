package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
