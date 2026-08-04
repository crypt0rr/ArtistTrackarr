package version

// Current is overridden by release builds with -ldflags -X. Keeping a clear
// development fallback makes locally built binaries distinguishable from a
// published release.
var Current = "dev"

var UserAgent = "ArtistTrackarr/" + Current
