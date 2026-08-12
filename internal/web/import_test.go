package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestImportConcurrencyGateBoundsConcurrentUploads(t *testing.T) {
	a := &App{importSlots: make(chan struct{}, maxConcurrentImports)}
	for i := 0; i < maxConcurrentImports; i++ {
		if !a.acquireImportSlot(httptest.NewRecorder()) {
			t.Fatalf("slot %d was unexpectedly rejected", i)
		}
	}
	blocked := httptest.NewRecorder()
	if a.acquireImportSlot(blocked) {
		t.Fatal("third concurrent import was accepted")
	}
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked import status=%d, want %d", blocked.Code, http.StatusTooManyRequests)
	}
	if blocked.Header().Get("Retry-After") != "30" {
		t.Fatalf("blocked import retry-after=%q, want 30", blocked.Header().Get("Retry-After"))
	}
	a.releaseImportSlot()
	if !a.acquireImportSlot(httptest.NewRecorder()) {
		t.Fatal("released import slot was not reusable")
	}
	a.releaseImportSlot()
	a.releaseImportSlot()
}

func TestParseArtistTrackarrCSVRoundTripAndReorderedColumns(t *testing.T) {
	mbid := "11111111-1111-4111-8111-111111111111"
	spotifyID := "0OdUWJ0sBjDrqHygGUXeCF"
	input := "spotify_url,extra,musicbrainz_url,display_name,artist,spotify_id,musicbrainz_id\n" +
		"https://open.spotify.com/artist/" + spotifyID + ",ignored,https://musicbrainz.org/artist/" + mbid + ",\"Comma, Artist\",https://musicbrainz.org/artist/" + mbid + "," + spotifyID + "," + mbid + "\n"
	rows, err := parseArtistTrackarrCSV(strings.NewReader(input))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if rows[0].Reason != "" || rows[0].DisplayName != "Comma, Artist" || rows[0].MBID != mbid || rows[0].SpotifyID != spotifyID {
		t.Fatalf("unexpected parsed row=%#v", rows[0])
	}
}

func TestParseArtistTrackarrCSVKeepsInvalidRowsIndependent(t *testing.T) {
	validMBID := "11111111-1111-4111-8111-111111111111"
	valid := "https://musicbrainz.org/artist/" + validMBID
	input := "artist,display_name,musicbrainz_id,musicbrainz_url,spotify_id,spotify_url\n" +
		valid + ",Valid," + validMBID + "," + valid + ",,\n" +
		"https://musicbrainz.org/artist/not-an-id,Missing ID,,https://musicbrainz.org/artist/not-an-id,,\n"
	rows, err := parseArtistTrackarrCSV(strings.NewReader(input))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if rows[0].Reason != "" || rows[1].Reason == "" {
		t.Fatalf("expected one invalid row: %#v", rows)
	}
}

func TestParseArtistTrackarrCSVRejectsMissingHeadersAndTooManyRows(t *testing.T) {
	if _, err := parseArtistTrackarrCSV(strings.NewReader("artist,display_name\nfoo,bar\n")); err == nil {
		t.Fatal("missing required headers accepted")
	}
	var builder strings.Builder
	builder.WriteString("artist,display_name,musicbrainz_id,musicbrainz_url,spotify_id,spotify_url\n")
	for i := 0; i <= maxArtistImportRows; i++ {
		mbid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		builder.WriteString("https://musicbrainz.org/artist/" + mbid + ",Artist," + mbid + ",https://musicbrainz.org/artist/" + mbid + ",,\n")
	}
	if _, err := parseArtistTrackarrCSV(strings.NewReader(builder.String())); err == nil {
		t.Fatal("row limit was not enforced")
	}
}

func TestValidateArtistImportInputRejectsInvalidIdentities(t *testing.T) {
	mbid := "11111111-1111-4111-8111-111111111111"
	mbURL := "https://musicbrainz.org/artist/" + mbid
	base := store.ImportInput{SourceValue: mbid, DisplayName: "Artist", MBID: mbid, MBURL: mbURL}
	cases := []struct {
		name  string
		input store.ImportInput
		want  string
	}{
		{name: "missing display name", input: store.ImportInput{SourceValue: mbid, MBID: mbid, MBURL: mbURL}, want: "display name is required"},
		{name: "missing source", input: store.ImportInput{DisplayName: "Artist", MBID: mbid, MBURL: mbURL}, want: "artist source value is required"},
		{name: "invalid MBID", input: store.ImportInput{SourceValue: "bad", DisplayName: "Artist", MBID: "bad", MBURL: mbURL}, want: "invalid MusicBrainz ID"},
		{name: "invalid MusicBrainz URL", input: store.ImportInput{SourceValue: mbid, DisplayName: "Artist", MBID: mbid, MBURL: "http://musicbrainz.org/artist/" + mbid}, want: "invalid MusicBrainz artist URL"},
		{name: "source mismatch", input: store.ImportInput{SourceValue: "other", DisplayName: "Artist", MBID: mbid, MBURL: mbURL}, want: "artist source does not match"},
		{name: "Spotify URL without ID", input: func() store.ImportInput {
			input := base
			input.SpotifyURL = "https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF"
			return input
		}(), want: "Spotify ID is required"},
		{name: "invalid Spotify ID", input: func() store.ImportInput {
			input := base
			input.SpotifyID, input.SpotifyURL = "bad", "https://open.spotify.com/artist/bad"
			return input
		}(), want: "invalid Spotify artist ID"},
		{name: "Spotify ID without URL", input: func() store.ImportInput {
			input := base
			input.SpotifyID = "0OdUWJ0sBjDrqHygGUXeCF"
			return input
		}(), want: "Spotify URL is required"},
		{name: "invalid Spotify URL", input: func() store.ImportInput {
			input := base
			input.SpotifyID, input.SpotifyURL = "0OdUWJ0sBjDrqHygGUXeCF", "https://example.test/artist/0OdUWJ0sBjDrqHygGUXeCF"
			return input
		}(), want: "invalid Spotify artist URL"},
		{name: "Spotify URL mismatch", input: func() store.ImportInput {
			input := base
			input.SpotifyID, input.SpotifyURL = "0OdUWJ0sBjDrqHygGUXeCF", "https://open.spotify.com/artist/1OdUWJ0sBjDrqHygGUXeCF"
			return input
		}(), want: "invalid Spotify artist URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateArtistImportInput(tc.input); got == "" || !strings.Contains(got, tc.want) {
				t.Fatalf("validation error=%q, want containing %q", got, tc.want)
			}
		})
	}
}

func TestImportIdentityValidatorsRejectMalformedURLsAndIDs(t *testing.T) {
	mbid := "11111111-1111-4111-8111-111111111111"
	for _, value := range []string{
		"", "11111111_1111-4111-8111-111111111111", "11111111-1111-4111-8111-zzzzzzzzzzzz",
	} {
		if validMBID(value) {
			t.Fatalf("malformed MBID accepted: %q", value)
		}
	}
	for _, value := range []string{
		"http://musicbrainz.org/artist/" + mbid,
		"https://example.org/artist/" + mbid,
		"https://musicbrainz.org/release/" + mbid,
		"https://musicbrainz.org/artist/" + mbid + "?x=1",
		"https://musicbrainz.org/artist/" + mbid + "#fragment",
	} {
		if _, ok := validMusicBrainzArtistURL(value); ok {
			t.Fatalf("malformed MusicBrainz URL accepted: %q", value)
		}
	}
	for _, value := range []string{
		"http://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF",
		"https://example.org/artist/0OdUWJ0sBjDrqHygGUXeCF",
		"https://open.spotify.com/album/0OdUWJ0sBjDrqHygGUXeCF",
		"https://open.spotify.com/artist/bad",
		"https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF?si=1",
		"https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF#fragment",
	} {
		if _, ok := validSpotifyArtistURL(value); ok {
			t.Fatalf("malformed Spotify URL accepted: %q", value)
		}
	}
}
