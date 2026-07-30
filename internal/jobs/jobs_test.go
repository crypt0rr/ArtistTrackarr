package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

type resolutionCatalog struct {
	external     []catalog.ArtistResult
	externalErr  error
	search       []catalog.ArtistResult
	searchErr    error
	releases     []store.Release
	releaseErr   error
	releaseCalls atomic.Int32
}

func (f *resolutionCatalog) SearchArtists(context.Context, string, int) ([]catalog.ArtistResult, error) {
	return f.search, f.searchErr
}

func (f *resolutionCatalog) ResolveArtist(context.Context, string) (catalog.ArtistResult, error) {
	return catalog.ArtistResult{}, errors.New("not implemented")
}

func (f *resolutionCatalog) ResolveExternalArtist(context.Context, string) ([]catalog.ArtistResult, error) {
	return f.external, f.externalErr
}

func (f *resolutionCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	f.releaseCalls.Add(1)
	return f.releases, f.releaseErr
}

type spotifyReleaseCatalog struct {
	releases []store.Release
	err      error
	calls    atomic.Int32
}

func (f *spotifyReleaseCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	f.calls.Add(1)
	return f.releases, f.err
}

func resolutionTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func testRunner(database *store.Store, provider catalog.CatalogProvider) *Runner {
	return New(
		database, provider, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestExactSpotifyResolutionCreatesFollowAndOnboardingEvent(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	resolution, _, _ := database.CreateArtistResolution(
		ctx, userID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example",
	)
	provider := &resolutionCatalog{
		external: []catalog.ArtistResult{{MBID: "artist-mbid", Name: "Example", SortName: "Example"}},
		releases: []store.Release{{
			MBID: "release-mbid", Title: "Next", PrimaryType: "Album",
			FirstReleaseDate: time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02"),
			DatePrecision:    3, MusicBrainzURL: "https://musicbrainz.org/release-group/release-mbid",
		}},
	}
	status, err := testRunner(database, provider).ResolveArtistResolutionNow(ctx, resolution)
	if err != nil || status != "followed" {
		t.Fatalf("resolution status=%q err=%v", status, err)
	}
	followed, err := database.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 1 || followed[0].SpotifyID != "spotify-id" {
		t.Fatalf("followed artists=%#v err=%v", followed, err)
	}
	var events int
	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 || provider.releaseCalls.Load() != 1 {
		t.Fatalf("events=%d release calls=%d", events, provider.releaseCalls.Load())
	}
}

func TestUnlinkedSpotifyArtistRequiresReview(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	resolution, _, _ := database.CreateArtistResolution(
		ctx, userID, "spotify-id", "Shared Name", "https://open.spotify.com/artist/spotify-id", "",
	)
	provider := &resolutionCatalog{search: []catalog.ArtistResult{
		{MBID: "candidate-one", Name: "Shared Name", Type: "Person"},
		{MBID: "candidate-two", Name: "Shared Name", Type: "Group"},
	}}
	status, err := testRunner(database, provider).ResolveArtistResolutionNow(ctx, resolution)
	if err != nil || status != "review" {
		t.Fatalf("resolution status=%q err=%v", status, err)
	}
	saved, err := database.ArtistResolution(ctx, userID, resolution.ID)
	if err != nil || saved.Status != "review" || len(saved.Candidates) != 2 {
		t.Fatalf("saved resolution=%#v err=%v", saved, err)
	}
	followed, _ := database.FollowedArtists(ctx, userID)
	if len(followed) != 0 {
		t.Fatalf("unsafe name match created follows: %#v", followed)
	}
}

func TestResolutionFailureSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	resolution, _, _ := database.CreateArtistResolution(
		ctx, userID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "",
	)
	before := time.Now().UTC()
	status, err := testRunner(database, &resolutionCatalog{externalErr: io.ErrUnexpectedEOF}).
		ResolveArtistResolutionNow(ctx, resolution)
	if err != nil || status != "pending" {
		t.Fatalf("resolution status=%q err=%v", status, err)
	}
	saved, err := database.ArtistResolution(ctx, userID, resolution.ID)
	if err != nil || saved.Attempts != 1 || saved.NextAttempt == nil ||
		saved.NextAttempt.Before(before.Add(59*time.Second)) ||
		saved.LastError != "MusicBrainz is temporarily unavailable." {
		t.Fatalf("retry resolution=%#v err=%v", saved, err)
	}
}

func TestExistingFollowCompletesWithoutAnotherInitialSync(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := database.UpsertArtist(ctx, store.Artist{MBID: "artist-mbid", Name: "Example"})
	database.Follow(ctx, userID, artist.ID)
	resolution, _, _ := database.CreateArtistResolution(
		ctx, userID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "",
	)
	provider := &resolutionCatalog{
		external: []catalog.ArtistResult{{MBID: "artist-mbid", Name: "Example"}},
		releases: []store.Release{{MBID: "release-mbid", Title: "Should not sync", PrimaryType: "Album"}},
	}
	status, err := testRunner(database, provider).ResolveArtistResolutionNow(ctx, resolution)
	if err != nil || status != "followed" || provider.releaseCalls.Load() != 0 {
		t.Fatalf("status=%q err=%v release calls=%d", status, err, provider.releaseCalls.Load())
	}
}

func TestSyncContinuesWithSpotifyWhenMusicBrainzFails(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	database.Follow(ctx, userID, artist.ID)
	spotify := &spotifyReleaseCatalog{releases: []store.Release{{
		MBID: "spotify:album-id", SpotifyID: "album-id", Title: "Spotify Album", PrimaryType: "Album",
		FirstReleaseDate: "2026-08-01", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/album-id", Source: "spotify",
	}}}
	runner := New(
		database, &resolutionCatalog{releaseErr: io.ErrUnexpectedEOF}, catalog.AlbumEPNormalizer{},
		nil, nil, 6*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), WithSpotify(spotify),
	)
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].Source != "spotify" ||
		releases[0].SpotifyID != "album-id" || spotify.calls.Load() != 1 {
		t.Fatalf("releases=%#v Spotify calls=%d err=%v", releases, spotify.calls.Load(), err)
	}
}

func TestArtistResolutionRetryDelayIsBounded(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 1, want: time.Minute},
		{attempts: 2, want: 5 * time.Minute},
		{attempts: 3, want: 15 * time.Minute},
		{attempts: 4, want: time.Hour},
		{attempts: 5, want: 6 * time.Hour},
		{attempts: 20, want: 6 * time.Hour},
	}
	for _, test := range tests {
		if got := artistResolutionRetryDelay(test.attempts); got != test.want {
			t.Fatalf("attempt %d delay=%s want=%s", test.attempts, got, test.want)
		}
	}
}
