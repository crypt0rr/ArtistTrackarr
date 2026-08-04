package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/notify"
	"github.com/crypt0rr/artist-tracker/internal/security"
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

type itunesReleaseCatalog struct {
	releases []store.Release
	err      error
	calls    atomic.Int32
}

type parallelTestSender struct {
	active atomic.Int32
	max    atomic.Int32
	calls  atomic.Int32
	delay  time.Duration
}

type failingSender struct {
	err error
}

func (s failingSender) Validate(string) error { return nil }

func (s failingSender) Send(context.Context, string, string, string) error { return s.err }

var _ notify.NotificationSender = (*parallelTestSender)(nil)

func (s *parallelTestSender) Validate(string) error { return nil }

func (s *parallelTestSender) Send(ctx context.Context, _, _, _ string) error {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	s.calls.Add(1)
	for {
		current := s.max.Load()
		if active <= current || s.max.CompareAndSwap(current, active) {
			break
		}
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *itunesReleaseCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	f.calls.Add(1)
	return f.releases, f.err
}

func resolutionTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func testRunner(database *store.Store, provider catalog.CatalogProvider) *Runner {
	return New(
		database, provider, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestWakeIsCoalesced(t *testing.T) {
	runner := &Runner{}
	runner.initLifecycle()
	for range 100 {
		runner.Wake()
	}
	select {
	case <-runner.wake:
	default:
		t.Fatal("Wake did not enqueue a signal")
	}
	select {
	case <-runner.wake:
		t.Fatal("Wake queued more than one signal")
	default:
	}
}

func TestBackgroundTaskGuardPreventsOverlap(t *testing.T) {
	runner := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var guard sync.Mutex
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	runner.startTask(context.Background(), "test", &guard, func(context.Context) {
		calls.Add(1)
		close(started)
		<-release
	})
	<-started
	runner.startTask(context.Background(), "test", &guard, func(context.Context) { calls.Add(1) })
	close(release)
	runner.tasks.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("task calls=%d, want 1", got)
	}
}

func TestRunnerShutdownWaitsForTrackedTasks(t *testing.T) {
	runner := testRunner(resolutionTestStore(t), &resolutionCatalog{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var guard sync.Mutex
	started := make(chan struct{})
	runner.startTask(ctx, "shutdown-test", &guard, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	})
	go runner.Run(ctx)
	<-started
	cancel()
	select {
	case <-runner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not finish tracked work during shutdown")
	}
}

func TestDeliveryUsesBoundedWorkerPool(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "delivery@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "delivery-artist", Name: "Delivery Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("delivery test secret with at least 32 chars")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test://destination")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(ctx, userID, "Test", "generic", encrypted); err != nil {
		t.Fatal(err)
	}
	destinations, err := database.Destinations(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	for i := range 8 {
		releaseResult, err := database.DB.Exec(`INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("delivery-release-%d", i), artist.ID,
			fmt.Sprintf("Release %d", i), "Album", "[]", "2026-01-01", 3,
			"https://musicbrainz.org/release-group/example", base.Format(time.RFC3339Nano), base.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		releaseID, _ := releaseResult.LastInsertId()
		eventResult, err := database.DB.Exec(`INSERT INTO notification_events
			(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
			userID, releaseID, "announcement", fmt.Sprintf("Event %d", i), "body", base.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		eventID, _ := eventResult.LastInsertId()
		if _, err := database.DB.Exec(`INSERT INTO deliveries
			(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
			eventID, destinations[0].ID, "pending", 0, base.Format(time.RFC3339Nano), ""); err != nil {
			t.Fatal(err)
		}
	}
	sender := &parallelTestSender{delay: 40 * time.Millisecond}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, sender, cipher, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	summary, err := runner.deliver(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Attempted != 8 || summary.Sent != 8 || summary.Failed != 0 {
		t.Fatalf("delivery summary=%#v", summary)
	}
	if sender.max.Load() < 2 || sender.max.Load() > 4 || sender.calls.Load() != 8 {
		t.Fatalf("sender concurrency max=%d calls=%d", sender.max.Load(), sender.calls.Load())
	}
	var pending int
	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending deliveries=%d", pending)
	}
}

func TestDeliveryFailureSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "retry-delivery@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "retry-delivery-artist", Name: "Retry Delivery Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("retry delivery test secret with at least 32 chars")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test://retry-destination")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(ctx, userID, "Retry", "generic", encrypted); err != nil {
		t.Fatal(err)
	}
	destinations, err := database.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	releaseResult, err := database.DB.Exec(`INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "retry-delivery-release", artist.ID, "Retry Release", "Album", "[]", "2026-01-01", 3,
		"https://musicbrainz.org/release-group/retry-delivery-release", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := releaseResult.LastInsertId()
	eventResult, err := database.DB.Exec(`INSERT INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Retry title", "Retry body", now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := eventResult.LastInsertId()
	if _, err := database.DB.Exec(`INSERT INTO deliveries
		(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destinations[0].ID, "pending", 0, now.Format(time.RFC3339Nano), ""); err != nil {
		t.Fatal(err)
	}

	runner := New(database, nil, catalog.AlbumEPNormalizer{}, failingSender{err: errors.New("temporary delivery failure")}, cipher, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := time.Now().UTC()
	summary, err := runner.deliver(ctx, before)
	if err != nil || summary.Attempted != 1 || summary.Sent != 0 || summary.Failed != 1 {
		t.Fatalf("delivery summary=%#v err=%v", summary, err)
	}
	var status, nextAttempt, lastError string
	var attempts int
	if err := database.DB.QueryRow(`SELECT status,attempts,next_attempt_at,last_error FROM deliveries WHERE event_id=?`, eventID).
		Scan(&status, &attempts, &nextAttempt, &lastError); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, nextAttempt)
	if err != nil || status != "pending" || attempts != 1 || lastError != "temporary delivery failure" || next.Before(before.Add(59*time.Second)) {
		t.Fatalf("retry row status=%q attempts=%d next=%q last_error=%q parsed=%v err=%v", status, attempts, nextAttempt, lastError, next, err)
	}
}

func TestExactSpotifyResolutionCreatesFollowAndOnboardingEvent(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	resolution, _, _ := database.CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example")
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
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	resolution, _, _ := database.CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Shared Name", "https://open.spotify.com/artist/spotify-id", "")
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
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	resolution, _, _ := database.CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "")
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
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{MBID: "artist-mbid", Name: "Example"})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	resolution, _, _ := database.CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "")
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
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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

func TestSpotifyIsPrimaryReleaseSourceWhenAvailable(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	mb := &resolutionCatalog{releases: []store.Release{{
		MBID: "old-mbid", Title: "Old MusicBrainz Album", PrimaryType: "Album",
		FirstReleaseDate: "2018-01-01", DatePrecision: 3,
	}}}
	spotify := &spotifyReleaseCatalog{releases: []store.Release{{
		MBID: "spotify:new", SpotifyID: "new", Title: "New Spotify Album", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-31", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/new", Source: "spotify",
	}}}
	itunes := &itunesReleaseCatalog{err: errors.New("iTunes should not be called")}
	runner := New(database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithSpotify(spotify), WithITunes(itunes))
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	if mb.releaseCalls.Load() != 0 || spotify.calls.Load() != 1 || itunes.calls.Load() != 0 {
		t.Fatalf("MusicBrainz calls=%d Spotify calls=%d iTunes calls=%d", mb.releaseCalls.Load(), spotify.calls.Load(), itunes.calls.Load())
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].Title != "New Spotify Album" {
		t.Fatalf("primary releases=%#v err=%v", releases, err)
	}
}

func TestSpotifyFailureFallsBackToITunesBeforeMusicBrainz(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "spotify-artist",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	mb := &resolutionCatalog{releases: []store.Release{{
		MBID: "mb-release", Title: "MusicBrainz fallback", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
	}}}
	spotify := &spotifyReleaseCatalog{err: errors.New("spotify unavailable")}
	itunes := &itunesReleaseCatalog{releases: []store.Release{{
		MBID: "itunes:itunes-release", ITunesID: "itunes-release", ITunesURL: "https://music.apple.com/us/album/example/1", Title: "iTunes release",
		PrimaryType: "Album", FirstReleaseDate: "2026-08-02", DatePrecision: 3, Source: "itunes",
	}}}
	runner := New(database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithSpotify(spotify), WithITunes(itunes))
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	if spotify.calls.Load() != 1 || itunes.calls.Load() != 1 || mb.releaseCalls.Load() != 0 {
		t.Fatalf("provider order Spotify=%d iTunes=%d MusicBrainz=%d", spotify.calls.Load(), itunes.calls.Load(), mb.releaseCalls.Load())
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].Source != "itunes" || releases[0].Title != "iTunes release" {
		t.Fatalf("iTunes fallback releases=%#v err=%v", releases, err)
	}
}

func TestITunesFailureFallsBackToMusicBrainz(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{MBID: "artist-mbid", Name: "Example"})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	itunes := &itunesReleaseCatalog{err: errors.New("itunes unavailable")}
	mb := &resolutionCatalog{releases: []store.Release{{
		MBID: "mb-release", Title: "MusicBrainz fallback", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
	}}}
	runner := New(database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithITunes(itunes))
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	if itunes.calls.Load() != 1 || mb.releaseCalls.Load() != 1 {
		t.Fatalf("provider fallback iTunes=%d MusicBrainz=%d", itunes.calls.Load(), mb.releaseCalls.Load())
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].Source != "musicbrainz" {
		t.Fatalf("MusicBrainz fallback releases=%#v err=%v", releases, err)
	}
}

func TestSyncAppliesMusicBrainzWhenSpotifyIsRateLimited(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	oldSpotify := store.Release{
		MBID: "spotify:old", SpotifyID: "old", Title: "Stored Spotify Album", PrimaryType: "Album",
		FirstReleaseDate: "2025-01-01", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/old", Source: "spotify",
	}
	if err := database.ApplyReleaseBatches(ctx, artist, []store.ReleaseBatch{{
		Provider: "spotify", Releases: []store.Release{oldSpotify},
	}}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rateLimit := &catalog.SpotifyRateLimitError{
		Operation: "Spotify artist albums", Status: 429, Reason: "rate_limited", RetryAfter: 2 * time.Minute,
	}
	runner := New(
		database,
		&resolutionCatalog{releases: []store.Release{{
			MBID: "musicbrainz-new", Title: "Canonical Album", PrimaryType: "Album",
			FirstReleaseDate: "2026-07-30", DatePrecision: 3,
		}}},
		catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithSpotify(&spotifyReleaseCatalog{err: rateLimit}),
	)
	before := time.Now().UTC()
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 2 {
		t.Fatalf("preserved releases=%#v err=%v", releases, err)
	}
	var nextCheck string
	if err := database.DB.QueryRow(`SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&nextCheck); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, nextCheck)
	if err != nil || next.Before(before.Add(119*time.Second)) || next.After(before.Add(3*time.Minute)) {
		t.Fatalf("next Spotify retry=%q parsed=%v err=%v", nextCheck, next, err)
	}
}

func TestQuotaCooldownDefersNextArtistCheckUntilProviderRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	providerRetry := 8 * time.Hour
	runner := New(
		database,
		&resolutionCatalog{releases: []store.Release{{
			MBID: "musicbrainz-release", Title: "Canonical Album", PrimaryType: "Album",
			FirstReleaseDate: "2026-07-30", DatePrecision: 3,
		}}},
		catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithSpotify(&spotifyReleaseCatalog{err: &catalog.SpotifyRateLimitError{
			Operation: "Spotify artist albums", Status: 429, Reason: "QUOTA_EXCEEDED",
			RetryAfter: providerRetry, QuotaExceeded: true,
		}}), WithSpotifyInterval(time.Hour),
	)
	before := time.Now().UTC()
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	var nextCheck string
	if err := database.DB.QueryRow(`SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&nextCheck); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, nextCheck)
	if err != nil || next.Before(before.Add(providerRetry-time.Second)) || next.After(before.Add(providerRetry+time.Second)) {
		t.Fatalf("quota retry=%q parsed=%v err=%v", nextCheck, next, err)
	}
}

func TestPersistedSpotifyCooldownSkipsCallsAfterRestart(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{
		MBID: "artist-mbid", Name: "Example", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(2 * time.Hour)
	if err := database.UpsertProviderHealth(ctx, "spotify", false, &future, true, true, "quota limited"); err != nil {
		t.Fatal(err)
	}
	spotify := &spotifyReleaseCatalog{releases: []store.Release{{
		MBID: "spotify:should-not-fetch", SpotifyID: "should-not-fetch", Title: "Should not fetch", PrimaryType: "Album",
		FirstReleaseDate: "2026-08-01", DatePrecision: 3, Source: "spotify",
	}}}
	mb := &resolutionCatalog{releases: []store.Release{{
		MBID: "musicbrainz-fallback", Title: "Fallback Album", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-30", DatePrecision: 3,
	}}}
	runner := New(database, mb, catalog.AlbumEPNormalizer{}, nil, nil, 6*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithSpotify(spotify), WithSpotifyInterval(time.Hour))
	if err := runner.SyncArtistNow(ctx, artist); err != nil {
		t.Fatal(err)
	}
	if spotify.calls.Load() != 0 || mb.releaseCalls.Load() != 1 {
		t.Fatalf("Spotify calls=%d MusicBrainz calls=%d", spotify.calls.Load(), mb.releaseCalls.Load())
	}
	releases, err := database.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].Title != "Fallback Album" {
		t.Fatalf("fallback releases=%#v err=%v", releases, err)
	}
	var nextCheck string
	if err := database.DB.QueryRow(`SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&nextCheck); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, nextCheck)
	if err != nil || next.Before(future.Add(-time.Second)) {
		t.Fatalf("persisted cooldown retry=%q parsed=%v err=%v", nextCheck, next, err)
	}
}

func TestTotalProviderFailureSchedulesBoundedRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := database.UpsertArtist(ctx, store.Artist{MBID: "artist-mbid", Name: "Example"})
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	err := testRunner(database, &resolutionCatalog{releaseErr: io.ErrUnexpectedEOF}).SyncArtistNow(ctx, artist)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("sync error=%v", err)
	}
	var nextCheck string
	if err := database.DB.QueryRow(`SELECT next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&nextCheck); err != nil {
		t.Fatal(err)
	}
	next, parseErr := time.Parse(time.RFC3339Nano, nextCheck)
	if parseErr != nil || next.Before(before.Add(14*time.Minute+59*time.Second)) ||
		next.After(before.Add(16*time.Minute)) {
		t.Fatalf("bounded provider retry=%q parsed=%v err=%v", nextCheck, next, parseErr)
	}
	if due, err := database.ArtistsDue(ctx, time.Now(), 10); err != nil || len(due) != 0 {
		t.Fatalf("provider failure retried immediately: due=%#v err=%v", due, err)
	}
}

func TestSpotifyRetrySchedulingBounds(t *testing.T) {
	interval := 6 * time.Hour
	if got := syncRetryDelay(&catalog.SpotifyRateLimitError{RetryAfter: 10 * time.Second}, interval); got != time.Minute {
		t.Fatalf("short retry delay=%s", got)
	}
	if got := syncRetryDelay(&catalog.SpotifyRateLimitError{RetryAfter: 24 * time.Hour}, interval); got != interval {
		t.Fatalf("long retry delay=%s", got)
	}
	if got := syncRetryDelay(&catalog.SpotifyRateLimitError{QuotaExceeded: true, RetryAfter: time.Minute}, interval); got != interval {
		t.Fatalf("quota retry delay=%s", got)
	}
	if got := syncRetryDelay(&catalog.SpotifyRateLimitError{QuotaExceeded: true, RetryAfter: 8 * time.Hour}, interval); got != 8*time.Hour {
		t.Fatalf("quota retry delay ignored provider cooldown: %s", got)
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
