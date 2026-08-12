package version

// Current is overridden for release images with -ldflags. Local builds are
// intentionally identified as dev so they cannot be mistaken for a release.
var Current = "dev"

var UserAgent = "ArtistTrackarr/" + Current
