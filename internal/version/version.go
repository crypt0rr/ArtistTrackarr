package version

// Current is the source-controlled application version shown in the UI and
// sent in provider User-Agent headers. It is bumped with every release so
// local and published images share the same version identity.
const Current = "0.63.2"

const UserAgent = "ArtistTrackarr/" + Current
