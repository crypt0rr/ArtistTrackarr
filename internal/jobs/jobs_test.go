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

type perArtistCatalog struct {
	releases map[string][]store.Release
	errors   map[string]error
}

func (f *perArtistCatalog) SearchArtists(context.Context, string, int) ([]catalog.ArtistResult, error) {
	return nil, nil
}

func (f *perArtistCatalog) ResolveArtist(context.Context, string) (catalog.ArtistResult, error) {
	return catalog.ArtistResult{}, nil
}

func (f *perArtistCatalog) ResolveExternalArtist(context.Context, string) ([]catalog.ArtistResult, error) {
	return nil, nil
}

func (f *perArtistCatalog) ArtistReleases(_ context.Context, mbid string) ([]store.Release, error) {
	if err := f.errors[mbid]; err != nil {
		return nil, err
	}
	return f.releases[mbid], nil
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

type incrementalSpotifyReleaseCatalog struct {
	releases   []store.Release
	err        error
	sinceDate  string
	calls      atomic.Int32
	sinceCalls atomic.Int32
}

func (f *incrementalSpotifyReleaseCatalog) ArtistReleases(context.Context, string) ([]store.Release, error) {
	f.calls.Add(1)
	return f.releases, f.err
}

func (f *incrementalSpotifyReleaseCatalog) ArtistReleasesSince(_ context.Context, _ string, since string) ([]store.Release, error) {
	f.sinceCalls.Add(1)
	f.sinceDate = since
	return f.releases, f.err
}

type itunesReleaseCatalog struct {
	releases []store.Release
	err      error
	calls    atomic.Int32
}

type listenBrainzStatsProvider struct {
	values map[string]catalog.ListenBrainzArtistStats
	err    error
	calls  atomic.Int32
	mbids  [][]string
}

func (p *listenBrainzStatsProvider) Popularity(_ context.Context, mbids []string) (map[string]catalog.ListenBrainzArtistStats, error) {
	p.calls.Add(1)
	p.mbids = append(p.mbids, append([]string(nil), mbids...))
	return p.values, p.err
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

func TestRunnerOptionalProviderOptions(t *testing.T) {
	runner := New(resolutionTestStore(t), &resolutionCatalog{}, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithListenBrainz(nil), WithArtworkCache(nil), WithSpotifyInterval(time.Hour))
	if runner.listenbrainz != nil || runner.artwork != nil || runner.spotifyInterval != time.Hour {
		t.Fatalf("runner options were not applied safely: listenbrainz=%v artwork=%v spotify_interval=%v", runner.listenbrainz, runner.artwork, runner.spotifyInterval)
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

func TestRunnerStopsImmediatelyWhenContextAlreadyCanceled(t *testing.T) {
	runner := testRunner(resolutionTestStore(t), &resolutionCatalog{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go runner.Run(ctx)
	select {
	case <-runner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("canceled runner did not stop")
	}
}

func TestRunnerProcessesWakeBeforeShutdown(t *testing.T) {
	runner := testRunner(resolutionTestStore(t), &resolutionCatalog{})
	ctx, cancel := context.WithCancel(context.Background())
	runner.Wake()
	go runner.Run(ctx)
	// Give the initial cadence and the coalesced wake a chance to launch their
	// tracked tasks, then verify cancellation still drains them cleanly.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-runner.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("woken runner did not stop")
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

func TestDigestDeliveryUsesNotificationWorker(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "digest-delivery@example.com", "unused", "member", "UTC", "digest-delivery")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "digest-delivery-artist", Name: "Digest Delivery Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("digest delivery secret with at least 32 chars")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test://digest-destination")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(ctx, userID, "Digest", "generic", encrypted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateNotificationPreferences(ctx, store.NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true, DigestEnabled: true, DigestFrequency: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if now.Hour() < 10 {
		now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	} else {
		now = now.Truncate(time.Minute)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-delivery-release", artist.ID, "Digest Delivery Release", "Album", "[]",
		now.AddDate(0, 0, 1).Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/digest-delivery-release", "musicbrainz", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if queued, err := database.QueueDueReleaseDigests(ctx, now); err != nil || queued != 1 {
		t.Fatalf("queued digest=%d err=%v", queued, err)
	}
	sender := &parallelTestSender{delay: 5 * time.Millisecond}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, sender, cipher, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	summary, err := runner.deliver(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Attempted != 1 || summary.Sent != 1 || summary.Failed != 0 || sender.calls.Load() != 1 {
		t.Fatalf("digest delivery summary=%#v calls=%d", summary, sender.calls.Load())
	}
	var status string
	if err := database.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("digest delivery status=%q", status)
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

func TestSelectArtistResolutionCreatesFollow(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, _ := database.CreateUser(ctx, "review-selection@example.com", "unused", "member", "UTC", "review-selection")
	resolution, _, err := database.CreateArtistResolution(ctx, userID, "spotify", "spotify-selection", "Selected", "https://open.spotify.com/artist/spotify-selection", "")
	if err != nil {
		t.Fatal(err)
	}
	provider := &resolutionCatalog{releases: []store.Release{{
		MBID: "selection-release", Title: "Selected Release", PrimaryType: "Album",
		FirstReleaseDate: time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02"), DatePrecision: 3,
	}}}
	runner := testRunner(database, provider)
	status, err := runner.SelectArtistResolution(ctx, resolution, store.ResolutionCandidate{
		MBID: "selection-artist", Name: "Selected", SortName: "Selected", Type: "Person",
	})
	if err != nil || status != "followed" {
		t.Fatalf("selected resolution status=%q err=%v", status, err)
	}
	followed, err := database.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 1 || followed[0].MBID != "selection-artist" {
		t.Fatalf("selected follow=%#v err=%v", followed, err)
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

func TestResolutionBranchesExternalAndSearchCandidates(t *testing.T) {
	tests := []struct {
		name       string
		catalog    *resolutionCatalog
		wantStatus string
		wantError  string
	}{
		{
			name: "multiple external candidates require review",
			catalog: &resolutionCatalog{external: []catalog.ArtistResult{
				{MBID: "external-one", Name: "Example"}, {MBID: "external-two", Name: "Example"},
			}}, wantStatus: "review",
		},
		{
			name: "external candidates without identifiers retry",
			catalog: &resolutionCatalog{external: []catalog.ArtistResult{
				{Name: "Example"}, {Name: "Example"},
			}}, wantStatus: "pending", wantError: "No MusicBrainz candidates were found yet.",
		},
		{
			name:       "empty search retries",
			catalog:    &resolutionCatalog{},
			wantStatus: "pending", wantError: "No MusicBrainz candidates were found yet.",
		},
		{
			name:       "search failure retries",
			catalog:    &resolutionCatalog{searchErr: errors.New("search unavailable")},
			wantStatus: "pending", wantError: "MusicBrainz is temporarily unavailable.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database := resolutionTestStore(t)
			userID, err := database.CreateUser(ctx, "resolution-branch@example.com", "unused", "member", "UTC", "resolution-branch")
			if err != nil {
				t.Fatal(err)
			}
			resolution, _, err := database.CreateArtistResolution(ctx, userID, "spotify", "resolution-branch-id", "Example", "https://open.spotify.com/artist/resolution-branch-id", "")
			if err != nil {
				t.Fatal(err)
			}
			status, err := testRunner(database, test.catalog).ResolveArtistResolutionNow(ctx, resolution)
			if err != nil || status != test.wantStatus {
				t.Fatalf("resolution status=%q err=%v, want %q", status, err, test.wantStatus)
			}
			saved, err := database.ArtistResolution(ctx, userID, resolution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantStatus == "review" && len(saved.Candidates) != 2 {
				t.Fatalf("review candidates=%#v", saved.Candidates)
			}
			if test.wantError != "" && saved.LastError != test.wantError {
				t.Fatalf("retry error=%q, want %q", saved.LastError, test.wantError)
			}
		})
	}
}

func TestResolveArtistResolutionsSummarizesStatuses(t *testing.T) {
	tests := []struct {
		name     string
		provider *resolutionCatalog
		want     resolutionStats
	}{
		{
			name:     "followed",
			provider: &resolutionCatalog{external: []catalog.ArtistResult{{MBID: "summary-followed", Name: "Example"}}},
			want:     resolutionStats{Processed: 1, Followed: 1},
		},
		{
			name: "review",
			provider: &resolutionCatalog{search: []catalog.ArtistResult{
				{MBID: "summary-review-one", Name: "Example"}, {MBID: "summary-review-two", Name: "Example"},
			}},
			want: resolutionStats{Processed: 1, Review: 1},
		},
		{
			name:     "pending",
			provider: &resolutionCatalog{externalErr: errors.New("provider unavailable")},
			want:     resolutionStats{Processed: 1, Pending: 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database := resolutionTestStore(t)
			userID, err := database.CreateUser(ctx, "resolution-summary@example.com", "unused", "member", "UTC", "resolution-summary")
			if err != nil {
				t.Fatal(err)
			}
			resolution, _, err := database.CreateArtistResolution(ctx, userID, "spotify", "resolution-summary-id", "Example", "https://open.spotify.com/artist/resolution-summary-id", "")
			if err != nil {
				t.Fatal(err)
			}
			runner := testRunner(database, test.provider)
			got, err := runner.resolveArtistResolutions(ctx, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("resolution summary=%#v, want %#v", got, test.want)
			}
			_ = resolution
		})
	}
}

func TestSyncArtistsSummarizesMixedOutcomes(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "sync-summary@example.com", "unused", "member", "UTC", "sync-summary")
	if err != nil {
		t.Fatal(err)
	}
	okArtist, err := database.UpsertArtist(ctx, store.Artist{MBID: "sync-summary-ok", Name: "Sync Summary OK"})
	if err != nil {
		t.Fatal(err)
	}
	failArtist, err := database.UpsertArtist(ctx, store.Artist{MBID: "sync-summary-fail", Name: "Sync Summary Fail"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artist := range []store.Artist{okArtist, failArtist} {
		if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}
	provider := &perArtistCatalog{
		releases: map[string][]store.Release{
			okArtist.MBID: {{MBID: "sync-summary-release", Title: "Summary Release", PrimaryType: "Album", FirstReleaseDate: "2026-08-06", DatePrecision: 3}},
		},
		errors: map[string]error{failArtist.MBID: errors.New("provider unavailable")},
	}
	runner := testRunner(database, provider)
	summary, err := runner.syncArtists(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Due != 2 || summary.Succeeded != 1 || summary.Failed != 1 {
		t.Fatalf("sync summary=%#v", summary)
	}
	var releases int
	if err := database.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_groups WHERE artist_id=?`, okArtist.ID).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("successful artist release count=%d", releases)
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
	var providerStatus string
	if err := database.DB.QueryRow(`SELECT status FROM artist_provider_status WHERE artist_id=? AND provider='spotify'`, artist.ID).Scan(&providerStatus); err != nil || providerStatus != "healthy" {
		t.Fatalf("Spotify coverage status=%q err=%v", providerStatus, err)
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
	var spotifyStatus, itunesStatus string
	if err := database.DB.QueryRow(`SELECT status FROM artist_provider_status WHERE artist_id=? AND provider='spotify'`, artist.ID).Scan(&spotifyStatus); err != nil || spotifyStatus != "failed" {
		t.Fatalf("Spotify fallback status=%q err=%v", spotifyStatus, err)
	}
	if err := database.DB.QueryRow(`SELECT status FROM artist_provider_status WHERE artist_id=? AND provider='itunes'`, artist.ID).Scan(&itunesStatus); err != nil || itunesStatus != "healthy" {
		t.Fatalf("iTunes fallback status=%q err=%v", itunesStatus, err)
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

func TestProviderCooldownAndReleaseDateHelpers(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"2026-08-07", true},
		{"2026-08-06", false},
		{"2026-09", true},
		{"2026-08", false},
		{"2027", true},
		{"2026", false},
		{"2026-08-06T00:00:00", false},
		{"", false},
	} {
		if got := isFutureRelease(tc.value, now); got != tc.want {
			t.Errorf("isFutureRelease(%q)=%v, want %v", tc.value, got, tc.want)
		}
	}
	if got := sanitizedProviderError(errors.New("request failed at https://secret.example/path?token=hidden")); got != "request failed at [url]secret.example/path?token=hidden" {
		t.Fatalf("sanitized provider error=%q", got)
	}
	rateLimit := &catalog.SpotifyRateLimitError{Operation: "artist albums", Reason: "QUOTA_EXCEEDED", QuotaExceeded: true, RetryAfter: 2 * time.Hour}
	if got := sanitizedProviderError(rateLimit); got != "artist albums returned 429 Too Many Requests (QUOTA_EXCEEDED)" {
		t.Fatalf("sanitized rate-limit error=%q", got)
	}
	if got := syncRetryDelay(rateLimit, time.Hour); got != 2*time.Hour {
		t.Fatalf("quota retry delay=%v", got)
	}
	normalLimit := &catalog.SpotifyRateLimitError{RetryAfter: 10 * time.Second}
	if got := syncRetryDelay(normalLimit, 2*time.Hour); got != time.Minute {
		t.Fatalf("normal retry delay=%v", got)
	}
	if got := providerFailureRetryDelay(nil, time.Hour); got != 15*time.Minute {
		t.Fatalf("provider failure retry delay=%v", got)
	}
}

func TestITunesProviderCooldownState(t *testing.T) {
	database := resolutionTestStore(t)
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	runner.setITunesProviderCooldown(until)
	got, err := runner.itunesProviderCooldown(context.Background(), now)
	if err != nil || !got.Equal(until) {
		t.Fatalf("iTunes cooldown=%v err=%v", got, err)
	}
	runner.clearITunesProviderCooldown()
	got, err = runner.itunesProviderCooldown(context.Background(), now)
	if err != nil || !got.IsZero() {
		t.Fatalf("cleared iTunes cooldown=%v err=%v", got, err)
	}
}

func TestObserveSpotifyUsesIncrementalProviderAndDefersFutureChecks(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	artist, err := database.UpsertArtist(ctx, store.Artist{
		MBID: "incremental-artist", Name: "Incremental Artist", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &incrementalSpotifyReleaseCatalog{releases: []store.Release{{
		MBID: "spotify:incremental", SpotifyID: "incremental-release", Title: "Incremental Release",
		PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3, Source: "spotify",
	}}}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithSpotify(provider))
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	observation, err := runner.observeSpotify(ctx, artist, now, "2026-07-01", true, true)
	if err != nil || !observation.succeeded || provider.sinceCalls.Load() != 1 || provider.calls.Load() != 0 {
		t.Fatalf("incremental observation=%#v err=%v since=%d full=%d", observation, err, provider.sinceCalls.Load(), provider.calls.Load())
	}
	if provider.sinceDate != "2026-07-01" {
		t.Fatalf("incremental since date=%q", provider.sinceDate)
	}

	artist.SpotifyNextCheckAt = timePtr(now.Add(time.Hour))
	observation, err = runner.observeSpotify(ctx, artist, now, "2026-08-01", false, true)
	if err != nil || !observation.deferred || observation.status != "deferred" || provider.sinceCalls.Load() != 1 {
		t.Fatalf("deferred observation=%#v err=%v since=%d", observation, err, provider.sinceCalls.Load())
	}
}

func TestObserveITunesHonorsCooldownAndRecordsRateLimit(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "itunes-observe-artist", Name: "iTunes Observe Artist"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &itunesReleaseCatalog{}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithITunes(provider))
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	runner.setITunesProviderCooldown(now.Add(time.Hour))
	observation, err := runner.observeITunes(ctx, artist, now, true)
	if err != nil || observation.suppressed || observation.status != "cooldown" || provider.calls.Load() != 0 {
		t.Fatalf("cooldown observation=%#v err=%v calls=%d", observation, err, provider.calls.Load())
	}

	runner.clearITunesProviderCooldown()
	provider.err = &catalog.ITunesRateLimitError{
		Operation: "iTunes artist albums", Status: 429, Reason: "RATE_LIMITED", RetryAfter: 2 * time.Minute,
	}
	observation, err = runner.observeITunes(ctx, artist, now, true)
	if err != nil || observation.err == nil || observation.itunesRateLimit == nil || observation.status != "failed" || provider.calls.Load() != 1 {
		t.Fatalf("rate-limited observation=%#v err=%v calls=%d", observation, err, provider.calls.Load())
	}
	if observation.nextCheckAt == nil || observation.nextCheckAt.Before(now.Add(2*time.Minute)) {
		t.Fatalf("rate-limit retry=%v", observation.nextCheckAt)
	}
}

func TestManualSyncRequestsAndQueuedResolution(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "manual-sync@example.com", "unused", "member", "UTC", "manual-sync")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "manual-sync-artist", Name: "Manual Sync Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	provider := &resolutionCatalog{releases: []store.Release{{
		MBID: "manual-release", Title: "Manual Release", PrimaryType: "Album",
		FirstReleaseDate: "2026-08-06", DatePrecision: 3,
	}}}
	runner := testRunner(database, provider)
	artistRequest, err := database.CreateManualSyncRequest(ctx, userID, "artist", &artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processed := runner.processManualSyncRequests(ctx, time.Now().UTC()); processed != 1 {
		t.Fatalf("processed artist request=%d", processed)
	}
	requests, err := database.ManualSyncRequests(ctx, 10)
	if err != nil || len(requests) != 1 || requests[0].ID != artistRequest.ID || requests[0].Status != "completed" {
		t.Fatalf("completed manual request=%#v err=%v", requests, err)
	}
	if provider.releaseCalls.Load() != 1 {
		t.Fatalf("manual provider calls=%d", provider.releaseCalls.Load())
	}

	retryRequest, err := database.CreateManualSyncRequest(ctx, userID, "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if processed := runner.processManualSyncRequests(ctx, time.Now().UTC()); processed != 1 {
		t.Fatalf("processed retry request=%d", processed)
	}
	requests, err = database.ManualSyncRequests(ctx, 10)
	if err != nil || len(requests) != 2 || requests[0].ID != retryRequest.ID || requests[0].Status != "completed" {
		t.Fatalf("completed retry request=%#v err=%v", requests, err)
	}

	resolution, _, err := database.CreateArtistResolution(ctx, userID, "itunes", "12345", "Selected Artist", "https://music.apple.com/us/artist/selected/12345", "")
	if err != nil {
		t.Fatal(err)
	}
	runner.initLifecycle()
	status, err := runner.QueueSelectedArtistResolution(ctx, resolution, store.ResolutionCandidate{
		MBID: "selected-mbid", Name: "Selected Artist", Type: "Person", Country: "NL",
	})
	if err != nil || status != "followed" {
		t.Fatalf("queued resolution status=%q err=%v", status, err)
	}
	select {
	case <-runner.wake:
	default:
		t.Fatal("queued resolution did not wake runner")
	}
	followed, err := database.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 2 {
		t.Fatalf("followed after resolution=%#v err=%v", followed, err)
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

func seedITunesArtworkJob(t *testing.T, database *store.Store, ctx context.Context, now time.Time) store.Artist {
	t.Helper()
	userID, err := database.CreateUser(ctx, "artwork-job@example.com", "unused", "member", "UTC", "artwork-job")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "artwork-job-artist", Name: "Artwork Job Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyReleaseBatches(ctx, artist, []store.ReleaseBatch{{Provider: "itunes", Releases: []store.Release{{
		MBID: "itunes:artwork-job", ITunesID: "artwork-job-release", ITunesURL: "https://music.apple.com/us/album/artwork-job/1",
		Title: "Artwork Job Release", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
	}}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `UPDATE release_groups SET itunes_artwork_next_check_at=? WHERE itunes_id=?`,
		now.Add(-time.Minute).Format(time.RFC3339Nano), "artwork-job-release"); err != nil {
		t.Fatal(err)
	}
	return artist
}

func TestBackfillITunesArtworkSuccess(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	artist := seedITunesArtworkJob(t, database, ctx, now)
	provider := &itunesReleaseCatalog{releases: []store.Release{{
		ITunesID: "artwork-job-release", ITunesArtworkURL: "https://is2.mzstatic.com/image/250x250bb.jpg",
	}}}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithITunes(provider))
	stats, err := runner.backfillITunesArtwork(ctx, now)
	if err != nil || stats == nil || stats.ArtistID != artist.ID || stats.Checked != 1 || stats.Updated != 1 {
		t.Fatalf("backfill stats=%#v err=%v", stats, err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("iTunes calls=%d, want 1", provider.calls.Load())
	}
	var artworkURL string
	if err := database.DB.QueryRowContext(ctx, `SELECT itunes_artwork_url FROM release_groups WHERE itunes_id=?`, "artwork-job-release").Scan(&artworkURL); err != nil {
		t.Fatal(err)
	}
	if artworkURL == "" {
		t.Fatal("successful backfill did not persist artwork")
	}
	health, err := database.ProviderHealthByName(ctx, "itunes")
	if err != nil || health.LastError != "" || health.RateLimited {
		t.Fatalf("healthy iTunes state=%#v err=%v", health, err)
	}
}

func TestBackfillITunesArtworkFailureSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	artist := seedITunesArtworkJob(t, database, ctx, now)
	provider := &itunesReleaseCatalog{err: errors.New("iTunes temporarily unavailable")}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithITunes(provider))
	stats, err := runner.backfillITunesArtwork(ctx, now)
	if err != nil || stats != nil || provider.calls.Load() != 1 {
		t.Fatalf("failed backfill stats=%#v err=%v calls=%d", stats, err, provider.calls.Load())
	}
	var nextText string
	var attempts int
	if err := database.DB.QueryRowContext(ctx, `SELECT itunes_artwork_next_check_at,itunes_artwork_attempts FROM release_groups WHERE itunes_id=?`, "artwork-job-release").Scan(&nextText, &attempts); err != nil {
		t.Fatal(err)
	}
	next, parseErr := time.Parse(time.RFC3339Nano, nextText)
	if parseErr != nil || attempts != 1 || next.Before(now.Add(time.Minute)) {
		t.Fatalf("retry schedule next=%q parsed=%v attempts=%d", nextText, parseErr, attempts)
	}
	health, err := database.ProviderHealthByName(ctx, "itunes")
	if err != nil || health.LastError == "" || health.RateLimited || health.NextCheckAt == nil {
		t.Fatalf("failed iTunes state=%#v err=%v", health, err)
	}
	// The persisted cooldown is also respected by later ticks, avoiding a
	// second request while the provider is unavailable.
	stats, err = runner.backfillITunesArtwork(ctx, now.Add(30*time.Second))
	if err != nil || stats != nil || provider.calls.Load() != 1 {
		t.Fatalf("retry was not suppressed stats=%#v err=%v calls=%d", stats, err, provider.calls.Load())
	}
	_ = artist
}

func TestBackfillITunesArtworkRateLimitUsesCooldown(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	seedITunesArtworkJob(t, database, ctx, now)
	provider := &itunesReleaseCatalog{err: &catalog.ITunesRateLimitError{
		Operation: "iTunes artist albums", Status: 429, Reason: "RATE_LIMITED", RetryAfter: 2 * time.Minute,
	}}
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithITunes(provider))
	stats, err := runner.backfillITunesArtwork(ctx, now)
	if err != nil || stats != nil || provider.calls.Load() != 1 {
		t.Fatalf("rate-limited backfill stats=%#v err=%v calls=%d", stats, err, provider.calls.Load())
	}
	health, err := database.ProviderHealthByName(ctx, "itunes")
	if err != nil || !health.RateLimited || health.NextCheckAt == nil || health.LastError == "" {
		t.Fatalf("rate-limited iTunes state=%#v err=%v", health, err)
	}
	stats, err = runner.backfillITunesArtwork(ctx, now.Add(time.Minute))
	if err != nil || stats != nil || provider.calls.Load() != 1 {
		t.Fatalf("cooldown was not respected stats=%#v err=%v calls=%d", stats, err, provider.calls.Load())
	}
}

func TestRefreshListenBrainzSuccessFiltersUndatedArtists(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "listenbrainz-job@example.com", "unused", "member", "UTC", "listenbrainz-job")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "LB-ONE", Name: "Listen Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	blank, err := database.UpsertArtist(ctx, store.Artist{MBID: "", Name: "No MBID Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, blank.ID); err != nil {
		t.Fatal(err)
	}
	provider := &listenBrainzStatsProvider{values: map[string]catalog.ListenBrainzArtistStats{
		"lb-one": {MBID: "LB-ONE", TotalListenCount: 123456, TotalUserCount: 789},
	}}
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithListenBrainz(provider))
	count, err := runner.refreshListenBrainz(ctx, now)
	if err != nil || count != 1 || provider.calls.Load() != 1 {
		t.Fatalf("ListenBrainz refresh count=%d err=%v calls=%d", count, err, provider.calls.Load())
	}
	if len(provider.mbids) != 1 || len(provider.mbids[0]) != 1 || provider.mbids[0][0] != "LB-ONE" {
		t.Fatalf("ListenBrainz MBIDs=%#v", provider.mbids)
	}
	var listens, users int64
	var nextText string
	if err := database.DB.QueryRowContext(ctx, `SELECT total_listen_count,total_user_count,next_check_at FROM artist_listenbrainz_stats WHERE artist_id=?`, artist.ID).Scan(&listens, &users, &nextText); err != nil {
		t.Fatal(err)
	}
	if listens != 123456 || users != 789 || nextText == "" {
		t.Fatalf("saved ListenBrainz stats listens=%d users=%d next=%q", listens, users, nextText)
	}
	health, err := database.ProviderHealthByName(ctx, "listenbrainz")
	if err != nil || health.LastError != "" {
		t.Fatalf("healthy ListenBrainz state=%#v err=%v", health, err)
	}
}

func TestRefreshListenBrainzFailureSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	database := resolutionTestStore(t)
	userID, err := database.CreateUser(ctx, "listenbrainz-failure@example.com", "unused", "member", "UTC", "listenbrainz-failure")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "lb-failure", Name: "Listen Failure Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	provider := &listenBrainzStatsProvider{err: errors.New("ListenBrainz unavailable")}
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithListenBrainz(provider))
	count, err := runner.refreshListenBrainz(ctx, now)
	if err == nil || count != 0 || provider.calls.Load() != 1 {
		t.Fatalf("failed ListenBrainz refresh count=%d err=%v calls=%d", count, err, provider.calls.Load())
	}
	var lastError, nextText string
	var attempts int
	if err := database.DB.QueryRowContext(ctx, `SELECT last_error,next_check_at,attempts FROM artist_listenbrainz_stats WHERE artist_id=?`, artist.ID).Scan(&lastError, &nextText, &attempts); err != nil {
		t.Fatal(err)
	}
	if lastError == "" || nextText == "" || attempts != 1 {
		t.Fatalf("ListenBrainz retry row error=%q next=%q attempts=%d", lastError, nextText, attempts)
	}
	health, err := database.ProviderHealthByName(ctx, "listenbrainz")
	if err != nil || health.LastError == "" || health.NextCheckAt == nil {
		t.Fatalf("failed ListenBrainz state=%#v err=%v", health, err)
	}
}

func TestRunMaintenanceWithoutArtworkCache(t *testing.T) {
	database := resolutionTestStore(t)
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.runMaintenance(context.Background())
}

func TestRunnerMaintenanceAndDeliveryHandleClosedStore(t *testing.T) {
	database := resolutionTestStore(t)
	runner := New(database, nil, catalog.AlbumEPNormalizer{}, nil, nil, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = database.Close()
	runner.runMaintenance(context.Background())
	runner.runDeliveryCadence(context.Background())
}
