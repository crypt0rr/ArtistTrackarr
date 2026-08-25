package store

import "testing"

func TestSymbolOnlyTitlesStayDistinguishable(t *testing.T) {
	// "+", "=", "-" and "÷" are real release names. Keeping only letters and
	// digits normalized every one of them to the empty string, so they compared
	// equal to each other and the identity match fell through to the date alone,
	// folding distinct releases into one release group and permanently losing
	// one along with its observations, calendar entry and notification.
	for _, title := range []string{"+", "=", "-", "÷", "!!!", "..."} {
		if got := normalizedReleaseTitle(title); got == "" {
			t.Fatalf("normalizedReleaseTitle(%q) is empty, so it matches every other symbol title", title)
		}
	}
	if normalizedReleaseTitle("+") == normalizedReleaseTitle("=") {
		t.Fatal(`"+" and "=" normalize alike and would be merged into one release group`)
	}
	// The same title from two providers must still merge.
	if normalizedReleaseTitle("+") != normalizedReleaseTitle(" + ") {
		t.Fatal(`the same symbol title from two providers no longer matches itself`)
	}
	// Ordinary titles are unaffected.
	if normalizedReleaseTitle("Hello World!") != normalizedReleaseTitle("hello  world") {
		t.Fatal("ordinary title normalization changed")
	}
}

func TestReleaseIdentityRejectsBlankTitles(t *testing.T) {
	// A genuinely blank title must not match another blank one on date alone.
	blank := Release{Title: "   ", FirstReleaseDate: "2026-01-02", DatePrecision: 3, PrimaryType: "Album"}
	other := Release{Title: "", FirstReleaseDate: "2026-01-02", DatePrecision: 3, PrimaryType: "Album"}
	if releaseIdentityMatches(blank, other) {
		t.Fatal("two blank titles matched on date alone")
	}
	// Two symbol titles that differ must not match either.
	plus := Release{Title: "+", FirstReleaseDate: "2026-01-02", DatePrecision: 3, PrimaryType: "Album"}
	equals := Release{Title: "=", FirstReleaseDate: "2026-01-02", DatePrecision: 3, PrimaryType: "Album"}
	if releaseIdentityMatches(plus, equals) {
		t.Fatal(`"+" and "=" matched, which would merge two distinct releases`)
	}
	// The same symbol title on the same date still matches.
	if !releaseIdentityMatches(plus, Release{Title: " + ", FirstReleaseDate: "2026-01-02", DatePrecision: 3, PrimaryType: "Album"}) {
		t.Fatal("the same symbol title from two providers no longer matches")
	}
}

// TestReleaseLinkHonoursTheConfirmedProvider is #272. There were three separate
// definitions of "the link for a release": the web template helper honoured a
// confirmed truth decision, while the ICS description and the digest and
// notification bodies used the same fallback chain with the TruthProvider switch
// missing. A household that had explicitly confirmed which source represents a
// release still had every alert and calendar entry link somewhere else.
func TestReleaseLinkHonoursTheConfirmedProvider(t *testing.T) {
	full := Release{
		SpotifyURL:     "https://open.spotify.com/album/x",
		ITunesURL:      "https://music.apple.com/us/album/x",
		MusicBrainzURL: "https://musicbrainz.org/release-group/x",
	}
	for _, test := range []struct {
		name     string
		provider string
		want     string
	}{
		{name: "confirmed iTunes wins over the default order", provider: "itunes", want: full.ITunesURL},
		{name: "confirmed MusicBrainz wins over the default order", provider: "musicbrainz", want: full.MusicBrainzURL},
		{name: "confirmed Spotify", provider: "spotify", want: full.SpotifyURL},
		{name: "no decision falls back to the preference order", provider: "", want: full.SpotifyURL},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := full
			release.TruthProvider = test.provider
			if got := ReleaseLink(release); got != test.want {
				t.Fatalf("ReleaseLink=%q, want %q", got, test.want)
			}
		})
	}

	// A decision whose provider has no URL must not produce an empty link.
	missing := Release{TruthProvider: "itunes", MusicBrainzURL: "https://musicbrainz.org/release-group/y"}
	if got := ReleaseLink(missing); got != missing.MusicBrainzURL {
		t.Fatalf("ReleaseLink=%q for a confirmed provider with no URL, want the fallback", got)
	}

	// The notification and digest bodies must agree with it.
	release := full
	release.TruthProvider = "itunes"
	if got := releaseExternalURL(release); got != release.ITunesURL {
		t.Fatalf("notification body link=%q, want the confirmed provider %q", got, release.ITunesURL)
	}
}
