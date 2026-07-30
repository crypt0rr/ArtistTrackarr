package catalog

import (
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestAlbumEPNormalizer(t *testing.T) {
	input := []store.Release{
		{MBID: "album", PrimaryType: "Album", SecondaryTypes: []string{"Live"}},
		{MBID: "ep", PrimaryType: "EP"},
		{MBID: "single", PrimaryType: "Single"},
		{MBID: "album", PrimaryType: "Album"},
	}
	got := (AlbumEPNormalizer{}).Normalize(input)
	if len(got) != 2 || got[0].MBID != "album" || got[1].MBID != "ep" {
		t.Fatalf("unexpected normalized releases: %#v", got)
	}
}

func TestSpotifyID(t *testing.T) {
	const id = "0OdUWJ0sBjDrqHygGUXeCF"
	for _, value := range []string{id, "spotify:artist:" + id, "https://open.spotify.com/artist/" + id} {
		if got, ok := SpotifyID(value); !ok || got != id {
			t.Fatalf("SpotifyID(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := SpotifyID("https://example.com/artist/" + id); ok {
		t.Fatal("accepted non-Spotify URL")
	}
}
