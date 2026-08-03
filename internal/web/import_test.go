package web

import (
	"fmt"
	"strings"
	"testing"
)

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
