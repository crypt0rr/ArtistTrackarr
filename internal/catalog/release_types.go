package catalog

import (
	"strings"
	"unicode"
)

// classifyReleaseType applies the shared provider-independent heuristic used
// for Spotify and iTunes releases. Provider APIs do not expose a consistent
// EP/single classification, so explicit title labels win, followed by track
// count. A provider's kind is only used when track count is unavailable.
// Compilations remain albums regardless of their track count.
func classifyReleaseType(kind, group, title string, trackCount int) (string, []string, bool) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	group = strings.ToLower(strings.TrimSpace(group))
	if kind == "podcast" || kind == "audiobook" {
		return "", nil, false
	}
	if kind == "compilation" || group == "compilation" {
		return "Album", []string{"Compilation"}, true
	}
	if containsReleaseWord(title, "single") {
		return "Single", nil, true
	}
	if containsReleaseWord(title, "ep") {
		return "EP", nil, true
	}
	switch {
	case trackCount == 1:
		return "Single", nil, true
	case trackCount >= 2 && trackCount <= 6:
		return "EP", nil, true
	case trackCount >= 7:
		return "Album", nil, true
	case kind == "album":
		return "Album", nil, true
	case kind == "single":
		return "Single", nil, true
	default:
		return "", nil, false
	}
}

// containsReleaseWord performs a case-insensitive, Unicode-aware word-boundary
// match. This intentionally does not classify titles such as “episode”,
// “epic”, or “epilogue” as an EP.
func containsReleaseWord(title, word string) bool {
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if part == word {
			return true
		}
	}
	return false
}
