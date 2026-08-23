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
