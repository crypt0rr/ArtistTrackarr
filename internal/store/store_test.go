package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestITunesMigrationPreservesExistingProviderData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		t.Fatal(err)
	}
	// Build a v7 database using the embedded migration files in order. The
	// migration runner is then exercised against a real legacy schema.
	for _, name := range []string{
		"001_initial.sql", "002_artist_resolutions.sql", "003_spotify_releases.sql",
		"004_provider_scheduling.sql", "005_reliability.sql", "006_notification_preferences.sql",
		"007_adaptive_spotify_polling.sql",
	} {
		body, readErr := migrations.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		var version int
		_, _ = fmt.Sscanf(name, "%03d_", &version)
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, nowText()); err != nil {
			t.Fatal(err)
		}
	}
	userResult, err := db.Exec(`INSERT INTO users(email,password_hash,role,created_at) VALUES('legacy@example.com','hash','member',?)`, nowText())
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := userResult.LastInsertId()
	artistResult, err := db.Exec(`INSERT INTO artists(mbid,name,created_at,updated_at) VALUES('artist-mbid','Legacy Artist',?,?)`, nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	artistID, _ := artistResult.LastInsertId()
	releaseResult, err := db.Exec(`INSERT INTO release_groups(mbid,artist_id,title,primary_type,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,spotify_id,source) VALUES('spotify:legacy',?,'Legacy','Album','',0,'',?,?,?,'spotify')`, artistID, nowText(), nowText(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := releaseResult.LastInsertId()
	if _, err := db.Exec(`INSERT INTO provider_observations(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('spotify','legacy',?,'hash',?)`, releaseID, nowText()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Store{DB: db}).CreateArtistResolution(ctx, userID, "spotify-id", "Legacy", "https://open.spotify.com/artist/spotify-id", ""); err != nil {
		t.Fatal(err)
	}
	s := &Store{DB: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var migrationsApplied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=8`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("migration marker=%d err=%v", migrationsApplied, err)
	}
	var source, itunesID string
	if err := db.QueryRow(`SELECT source,COALESCE(itunes_id,'') FROM release_groups WHERE id=?`, releaseID).Scan(&source, &itunesID); err != nil || source != "spotify" || itunesID != "" {
		t.Fatalf("legacy release source=%q itunes=%q err=%v", source, itunesID, err)
	}
	if _, err := db.Exec(`INSERT INTO release_groups(mbid,artist_id,title,primary_type,musicbrainz_url,first_observed_at,updated_at,itunes_id,source) VALUES('itunes:new',?,'New','EP','',?,?,?,'itunes')`, artistID, nowText(), nowText(), "123"); err != nil {
		t.Fatal(err)
	}
}

func TestImportRowsAreOwnerScopedAndScheduleNewFollows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "importer@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "other-importer@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateImportJob(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	mbid := "11111111-1111-4111-8111-111111111111"
	row, err := s.SaveImportRow(ctx, userID, job.ID, ImportInput{
		SourceValue: "https://musicbrainz.org/artist/" + mbid,
		DisplayName: "Imported Artist", MBID: mbid,
		MBURL:     "https://musicbrainz.org/artist/" + mbid,
		SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF", SpotifyURL: "https://open.spotify.com/artist/0OdUWJ0sBjDrqHygGUXeCF",
	})
	if err != nil || row.Status != "added" || row.ArtistID == nil {
		t.Fatalf("added row=%#v err=%v", row, err)
	}
	duplicate, err := s.SaveImportRow(ctx, userID, job.ID, ImportInput{
		SourceValue: "https://musicbrainz.org/artist/" + mbid, DisplayName: "Imported Artist", MBID: mbid,
		MBURL: "https://musicbrainz.org/artist/" + mbid,
	})
	if err != nil || duplicate.Status != "already_followed" {
		t.Fatalf("duplicate row=%#v err=%v", duplicate, err)
	}
	invalid, err := s.SaveImportRow(ctx, userID, job.ID, ImportInput{SourceValue: "bad", DisplayName: "Bad", Reason: "invalid MusicBrainz ID"})
	if err != nil || invalid.Status != "invalid" {
		t.Fatalf("invalid row=%#v err=%v", invalid, err)
	}
	loaded, err := s.ImportJob(ctx, userID, job.ID)
	if err != nil || loaded.Added != 1 || loaded.AlreadyFollowed != 1 || loaded.Invalid != 1 || len(loaded.Rows) != 3 {
		t.Fatalf("loaded job=%#v err=%v", loaded, err)
	}
	if _, err := s.ImportJob(ctx, otherID, job.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user import lookup err=%v", err)
	}
	var next sql.NullString
	if err := s.DB.QueryRow(`SELECT next_check_at FROM artists WHERE mbid=?`, mbid).Scan(&next); err != nil || !next.Valid {
		t.Fatalf("imported artist was not scheduled: %q err=%v", next.String, err)
	}
}

func TestPruneExpiredStateKeepsActiveAndQueuedState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "maintenance@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)
	activeSession, _, err := s.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateSession(ctx, userID, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthToken(ctx, "invite", "old@example.com", nil, userID, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthToken(ctx, "invite", "active@example.com", nil, userID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLoginFailure(ctx, "old-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE login_attempts SET first_at=?`, timeText(now.Add(-25*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO manual_sync_requests(requested_by,scope,status,created_at,finished_at) VALUES(?,?,?,?,?)`, userID, "retry", "completed", timeText(old), timeText(old)); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateImportJob(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE import_jobs SET created_at=? WHERE id=?`, timeText(old), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveImportRow(ctx, userID, job.ID, ImportInput{DisplayName: "bad", SourceValue: "bad", Reason: "invalid"}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.PruneExpiredState(ctx, now)
	if err != nil || stats.Sessions != 1 || stats.AuthTokens != 1 || stats.LoginAttempts != 1 || stats.ManualSyncs != 1 || stats.ImportJobs != 1 {
		t.Fatalf("maintenance stats=%#v err=%v", stats, err)
	}
	if _, err := s.Session(ctx, activeSession); err != nil {
		t.Fatalf("active session removed: %v", err)
	}
	var rows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM import_rows WHERE job_id=?`, job.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("import rows after cascade=%d err=%v", rows, err)
	}
}

func TestITunesAndMusicBrainzReleaseObservationsMerge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "itunes@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artist-mbid", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	itunesRelease := Release{MBID: "itunes:123", Title: "A Release", PrimaryType: "EP", FirstReleaseDate: "2026-08-01", DatePrecision: 3, ITunesID: "123", ITunesURL: "https://music.apple.com/us/album/a-release"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{itunesRelease}}}, observed); err != nil {
		t.Fatal(err)
	}
	musicBrainzRelease := Release{MBID: "mb-release", Title: "A Release", PrimaryType: "EP", FirstReleaseDate: "2026-08-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/mb-release"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "musicbrainz", Releases: []Release{musicBrainzRelease}}}, observed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	releases, err := s.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].MBID != "mb-release" || releases[0].Source != "both" || releases[0].ITunesID != "123" {
		t.Fatalf("merged releases=%#v err=%v", releases, err)
	}
	var observations, events int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM provider_observations WHERE release_group_id=?`, releases[0].ID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if observations != 2 || events != 1 {
		t.Fatalf("observations=%d events=%d", observations, events)
	}
}

func TestReleaseBaselineAndExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	initial := []Release{
		{MBID: "old", Title: "Back Catalogue", PrimaryType: "Album", FirstReleaseDate: "2001-01-01", DatePrecision: 3},
		{MBID: "future", Title: "Tomorrow", PrimaryType: "EP", FirstReleaseDate: "2026-08-30", DatePrecision: 3},
	}
	if err := s.ApplyReleaseSync(ctx, artist, initial, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	if err := s.ApplyReleaseSync(ctx, artist, initial, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	newRelease := append(initial, Release{
		MBID: "new", Title: "Just Released", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-29", DatePrecision: 3,
	})
	if err := s.ApplyReleaseSync(ctx, artist, newRelease, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
}

func TestSpotifyAdaptivePollingPersistsAndBacksOff(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "adaptive-artist", Name: "Adaptive"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	baseInterval := 24 * time.Hour
	if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, baseInterval, true, false); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 4; want++ {
		if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, baseInterval, false, false); err != nil {
			t.Fatal(err)
		}
		state, err := s.SpotifyPollingState(ctx, artist.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.UnchangedChecks != want {
			t.Fatalf("unchanged streak=%d, want %d", state.UnchangedChecks, want)
		}
	}
	state, err := s.SpotifyPollingState(ctx, artist.ID)
	if err != nil || state.LastChangeAt == nil || !state.LastChangeAt.Equal(now) {
		t.Fatalf("adaptive state=%#v err=%v", state, err)
	}
	var next string
	if err := s.DB.QueryRow(`SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	when, err := time.Parse(time.RFC3339Nano, next)
	if err != nil || when.Before(now.Add(7*24*time.Hour)) || when.After(now.Add(8*24*time.Hour)) {
		t.Fatalf("adaptive next check=%q parsed=%v", next, when)
	}
	if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, baseInterval, true, false); err != nil {
		t.Fatal(err)
	}
	state, err = s.SpotifyPollingState(ctx, artist.ID)
	if err != nil || state.UnchangedChecks != 0 {
		t.Fatalf("change did not reset state=%#v err=%v", state, err)
	}
}

func TestSpotifyBatchChangedDetectsNewAndUpdatedReleases(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "spotify-change-artist", Name: "Changes"})
	if err != nil {
		t.Fatal(err)
	}
	release := Release{MBID: "spotify:album-1", SpotifyID: "album-1", Title: "First", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3, SpotifyURL: "https://open.spotify.com/album/album-1"}
	changed, err := s.SpotifyBatchChanged(ctx, []Release{release})
	if err != nil || !changed {
		t.Fatalf("new release changed=%v err=%v", changed, err)
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{release}}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	changed, err = s.SpotifyBatchChanged(ctx, []Release{release})
	if err != nil || changed {
		t.Fatalf("unchanged release changed=%v err=%v", changed, err)
	}
	release.Title = "First (Deluxe)"
	changed, err = s.SpotifyBatchChanged(ctx, []Release{release})
	if err != nil || !changed {
		t.Fatalf("updated release changed=%v err=%v", changed, err)
	}
}

func TestInitialSyncChoosesNearestUpcomingRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	if err := s.AddDestination(ctx, userID, "Phone", "ntfy", []byte("encrypted-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Living room", "gotify", []byte("encrypted-two")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	releases := []Release{
		{MBID: "old", Title: "Last Year", PrimaryType: "Album", FirstReleaseDate: "2025-01-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.test/old"},
		{MBID: "far", Title: "Far Future", PrimaryType: "Album", FirstReleaseDate: "2027", DatePrecision: 1, MusicBrainzURL: "https://musicbrainz.test/far"},
		{MBID: "near", Title: "Next Month", PrimaryType: "EP", FirstReleaseDate: "2026-08", DatePrecision: 2, MusicBrainzURL: "https://musicbrainz.test/near"},
	}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	var title, body, releaseMBID string
	if err := s.DB.QueryRow(`SELECT e.title,e.body,rg.mbid FROM notification_events e
		JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&title, &body, &releaseMBID); err != nil {
		t.Fatal(err)
	}
	if title != "Upcoming release from Example" || releaseMBID != "near" ||
		!strings.Contains(body, "2026-08") || !strings.Contains(body, "https://musicbrainz.test/near") {
		t.Fatalf("unexpected initial notification: title=%q body=%q release=%q", title, body, releaseMBID)
	}
	var deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want 2", deliveries)
	}
}

func TestInitialSyncChoosesLatestPastAndSkipsUndated(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	releases := []Release{
		{MBID: "undated", Title: "Mystery", PrimaryType: "Album"},
		{MBID: "older", Title: "Older", PrimaryType: "Album", FirstReleaseDate: "2025", DatePrecision: 1},
		{MBID: "latest", Title: "Latest", PrimaryType: "EP", FirstReleaseDate: "2026-06", DatePrecision: 2},
	}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	var releaseMBID, title string
	if err := s.DB.QueryRow(`SELECT rg.mbid,e.title FROM notification_events e
		JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&releaseMBID, &title); err != nil {
		t.Fatal(err)
	}
	if releaseMBID != "latest" || title != "Latest release from Example" {
		t.Fatalf("selected %q with title %q", releaseMBID, title)
	}
}

func TestInitialSyncWithOnlyUndatedReleasesCreatesNoEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{MBID: "undated", Title: "Mystery", PrimaryType: "Album"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
}

func TestInitialMultiSourceSyncCanChooseSpotifyRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{
		MBID: "artist-id", Name: "Pjotr", SortName: "Pjotr", SpotifyID: "spotify-artist",
	})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{
		{Provider: "musicbrainz", Releases: []Release{{
			MBID: "old-mbid", Title: "Old Album", PrimaryType: "Album",
			FirstReleaseDate: "2017-01-01", DatePrecision: 3,
			MusicBrainzURL: "https://musicbrainz.org/release-group/old-mbid",
		}}},
		{Provider: "spotify", Releases: []Release{{
			MBID: "spotify:new-album", SpotifyID: "new-album", Title: "1. KRUIS", PrimaryType: "EP",
			FirstReleaseDate: "2026-08-01", DatePrecision: 3,
			SpotifyURL: "https://open.spotify.com/album/new-album", Source: "spotify",
		}}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	var title, body, source string
	if err := s.DB.QueryRow(`SELECT e.title,e.body,rg.source FROM notification_events e
		JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&title, &body, &source); err != nil {
		t.Fatal(err)
	}
	if title != "Upcoming release from Pjotr" || source != "spotify" ||
		!strings.Contains(body, "1. KRUIS") ||
		!strings.Contains(body, "https://open.spotify.com/album/new-album") {
		t.Fatalf("unexpected Spotify onboarding event: title=%q body=%q source=%q", title, body, source)
	}
}

func TestSpotifyUpgradeBaselineSuppressesBackCatalogueAndAlertsNewRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{
		MBID: "artist-id", Name: "Example", SortName: "Example", SpotifyID: "spotify-artist",
	})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseSync(ctx, artist, nil, now); err != nil {
		t.Fatal(err)
	}
	oldSpotify := Release{
		MBID: "spotify:old", SpotifyID: "old", Title: "Back Catalogue", PrimaryType: "Album",
		FirstReleaseDate: "2020-01-01", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/old", Source: "spotify",
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "spotify", Releases: []Release{oldSpotify},
	}}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
	var spotifyBaseline sql.NullString
	if err := s.DB.QueryRow(`SELECT spotify_baseline_synced_at FROM follows
		WHERE user_id=? AND artist_id=?`, userID, artist.ID).Scan(&spotifyBaseline); err != nil || !spotifyBaseline.Valid {
		t.Fatalf("Spotify baseline=%#v err=%v", spotifyBaseline, err)
	}
	newSpotify := Release{
		MBID: "spotify:new", SpotifyID: "new", Title: "New EP", PrimaryType: "EP",
		FirstReleaseDate: "2026-08-02", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/new", Source: "spotify",
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "spotify", Releases: []Release{oldSpotify, newSpotify},
	}}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)
}

func TestSpotifyReleaseIsPromotedToMusicBrainzWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SpotifyID: "spotify-artist"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	spotifyRelease := Release{
		MBID: "spotify:spotify-release", SpotifyID: "spotify-release", Title: "Shared Album",
		PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
		SpotifyURL: "https://open.spotify.com/album/spotify-release", Source: "spotify",
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "spotify", Releases: []Release{spotifyRelease},
	}}, now); err != nil {
		t.Fatal(err)
	}
	musicBrainzRelease := Release{
		MBID: "release-mbid", Title: "Shared Album", PrimaryType: "Album",
		FirstReleaseDate: "2026-08-01", DatePrecision: 3,
		MusicBrainzURL: "https://musicbrainz.org/release-group/release-mbid",
	}
	if err := s.ApplyReleaseSync(ctx, artist, []Release{musicBrainzRelease}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count, observations int
	var mbid, source, spotifyID string
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_groups WHERE artist_id=?`, artist.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT mbid,source,spotify_id FROM release_groups WHERE artist_id=?`, artist.ID).
		Scan(&mbid, &source, &spotifyID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM provider_observations`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if count != 1 || observations != 2 || mbid != "release-mbid" || source != "both" ||
		spotifyID != "spotify-release" {
		t.Fatalf("release count=%d observations=%d mbid=%q source=%q spotifyID=%q",
			count, observations, mbid, source, spotifyID)
	}
	assertEventCount(t, s, userID, "announcement", 1)
}

func TestSpotifyEditionsCollapseIntoOneRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SpotifyID: "spotify-artist"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "spotify",
		Releases: []Release{
			{
				MBID: "spotify:standard", SpotifyID: "standard", Title: "Shared Album",
				PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
				SpotifyURL: "https://open.spotify.com/album/standard", Source: "spotify",
			},
			{
				MBID: "spotify:deluxe", SpotifyID: "deluxe", Title: "Shared Album (Deluxe Edition)",
				PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
				SpotifyURL: "https://open.spotify.com/album/deluxe", Source: "spotify",
			},
		},
	}}, now); err != nil {
		t.Fatal(err)
	}
	var releases, observations int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_groups WHERE artist_id=?`, artist.ID).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM provider_observations WHERE provider='spotify'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if releases != 1 || observations != 2 {
		t.Fatalf("release groups=%d Spotify observations=%d", releases, observations)
	}
	assertEventCount(t, s, userID, "announcement", 1)
}

func TestDashboardReleasesSeparatesDefinitelyFutureDates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example"})
	s.Follow(ctx, userID, artist.ID)
	releases := []Release{
		{MBID: "future-day", Title: "Future day", PrimaryType: "Album", FirstReleaseDate: "2026-08-15", DatePrecision: 3},
		{MBID: "future-month", Title: "Future month", PrimaryType: "Album", FirstReleaseDate: "2026-09", DatePrecision: 2},
		{MBID: "future-year", Title: "Future year", PrimaryType: "Album", FirstReleaseDate: "2027", DatePrecision: 1},
		{MBID: "today", Title: "Today", PrimaryType: "Album", FirstReleaseDate: "2026-07-30", DatePrecision: 3},
		{MBID: "past", Title: "Past", PrimaryType: "Album", FirstReleaseDate: "2026-07-29", DatePrecision: 3},
		{MBID: "current-month", Title: "Current month", PrimaryType: "Album", FirstReleaseDate: "2026-07", DatePrecision: 2},
		{MBID: "current-year", Title: "Current year", PrimaryType: "Album", FirstReleaseDate: "2026", DatePrecision: 1},
		{MBID: "invalid-date", Title: "Invalid date", PrimaryType: "Album", FirstReleaseDate: "not-a-date", DatePrecision: 3},
		{MBID: "wrong-precision", Title: "Wrong precision", PrimaryType: "Album", FirstReleaseDate: "2028", DatePrecision: 3},
		{MBID: "undated", Title: "Undated", PrimaryType: "Album"},
	}
	if err := s.ApplyReleaseSync(ctx, artist, releases, time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	upcoming, recent, err := s.DashboardReleases(ctx, userID, "2026-07-30", 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReleaseMBIDs(t, upcoming, []string{"future-day", "future-month", "future-year"})
	assertReleaseMBIDs(t, recent, []string{
		"invalid-date", "wrong-precision", "today", "past", "current-month", "current-year", "undated",
	})
}

func TestScheduleArtistCheckDefersDueArtist(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	if err := s.ScheduleArtistCheck(ctx, artist.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due, err := s.ArtistsDue(ctx, now, 10); err != nil || len(due) != 0 {
		t.Fatalf("artists due immediately=%#v err=%v", due, err)
	}
	if due, err := s.ArtistsDue(ctx, now.Add(time.Hour), 10); err != nil || len(due) != 1 || due[0].ID != artist.ID {
		t.Fatalf("artists due after cooldown=%#v err=%v", due, err)
	}
}

func assertReleaseMBIDs(t *testing.T, releases []Release, want []string) {
	t.Helper()
	got := make([]string, len(releases))
	for i := range releases {
		got[i] = releases[i].MBID
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("release order=%v want=%v", got, want)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "listener@example.com", "hash", "member", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	raw, csrf, err := s.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Session(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if session.User.ID != userID || session.CSRFToken != csrf {
		t.Fatalf("unexpected session: %#v", session)
	}
}

func TestReleaseDayUsesUserTimezoneAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "Europe/Amsterdam")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	s.Follow(ctx, userID, artist.ID)
	now := time.Date(2026, 7, 30, 7, 1, 0, 0, time.UTC) // 09:01 in Amsterdam.
	releases := []Release{{
		MBID: "today", Title: "Today", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-30", DatePrecision: 3,
	}}
	if err := s.ApplyReleaseSync(ctx, artist, releases, now); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "release_day", 1)
}

func TestRenameDestinationIsOwnerScopedAndPreservesCredentials(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ownerID, _ := s.CreateUser(ctx, "owner@example.com", "unused", "member", "UTC")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
	encrypted := []byte("encrypted destination URL")
	if err := s.AddDestination(ctx, ownerID, "  Phone  ", "ntfy", encrypted); err != nil {
		t.Fatal(err)
	}
	destinations, _ := s.Destinations(ctx, ownerID)
	if len(destinations) != 1 || destinations[0].Name != "Phone" {
		t.Fatalf("unexpected destination: %#v", destinations)
	}
	id := destinations[0].ID
	if err := s.RenameDestination(ctx, otherID, id, "Stolen"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user rename error = %v", err)
	}
	if err := s.RenameDestination(ctx, ownerID, id, "  My phone  "); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.Destination(ctx, ownerID, id)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "My phone" || !bytes.Equal(renamed.EncryptedURL, encrypted) || renamed.Service != "ntfy" {
		t.Fatalf("rename changed protected fields: %#v", renamed)
	}
	for _, name := range []string{"   ", strings.Repeat("é", 81)} {
		if err := s.RenameDestination(ctx, ownerID, id, name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
}

func TestArtistResolutionLifecycleIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ownerID, _ := s.CreateUser(ctx, "owner@example.com", "unused", "member", "UTC")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
	resolution, created, err := s.CreateArtistResolution(
		ctx, ownerID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example",
	)
	if err != nil || !created {
		t.Fatalf("create resolution = %#v, %v, created=%v", resolution, err, created)
	}
	duplicate, created, err := s.CreateArtistResolution(
		ctx, ownerID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "",
	)
	if err != nil || created || duplicate.ID != resolution.ID {
		t.Fatalf("duplicate resolution = %#v, %v, created=%v", duplicate, err, created)
	}
	if _, err := s.ArtistResolution(ctx, otherID, resolution.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user lookup error = %v", err)
	}
	if err := s.CancelArtistResolution(ctx, otherID, resolution.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user cancel error = %v", err)
	}
	candidates := []ResolutionCandidate{{
		MBID: "artist-mbid", Name: "Example", SortName: "Example", Type: "Person", Country: "NL",
	}}
	if err := s.MarkArtistResolutionReview(ctx, ownerID, resolution.ID, candidates); err != nil {
		t.Fatal(err)
	}
	review, err := s.ArtistResolution(ctx, ownerID, resolution.ID)
	if err != nil || review.Status != "review" || len(review.Candidates) != 1 {
		t.Fatalf("review resolution = %#v, %v", review, err)
	}
	artist := candidates[0].Artist()
	artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL =
		resolution.ProviderID, resolution.ProviderURL, resolution.ImageURL
	artist, added, err := s.CompleteArtistResolution(ctx, review, artist)
	if err != nil || !added || artist.SpotifyID != "spotify-id" {
		t.Fatalf("complete resolution artist=%#v added=%v err=%v", artist, added, err)
	}
	if _, err := s.ArtistResolution(ctx, ownerID, resolution.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed resolution still exists: %v", err)
	}
	followed, err := s.FollowedArtists(ctx, ownerID)
	if err != nil || len(followed) != 1 || followed[0].MBID != "artist-mbid" {
		t.Fatalf("followed artists = %#v, %v", followed, err)
	}
}

func TestFollowedArtistsSortsByDisplayName(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artists := []Artist{
		{MBID: "artist-z", Name: "zeta", SortName: "Zeta"},
		{MBID: "artist-a", Name: "Alpha", SortName: "Alpha"},
		{MBID: "artist-b", Name: "beta", SortName: "Beta"},
	}
	for _, artist := range artists {
		saved, err := s.UpsertArtist(ctx, artist)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Follow(ctx, userID, saved.ID); err != nil {
			t.Fatal(err)
		}
	}
	followed, err := s.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 3 {
		t.Fatalf("followed artists=%#v err=%v", followed, err)
	}
	for i, want := range []string{"Alpha", "beta", "zeta"} {
		if followed[i].Name != want {
			t.Fatalf("artist order=%#v, want %v first", followed, want)
		}
	}
}

func TestArtistResolutionRetryScheduling(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	resolution, _, _ := s.CreateArtistResolution(
		ctx, userID, "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "",
	)
	now := time.Now().UTC()
	if err := s.RetryArtistResolution(ctx, userID, resolution.ID, 2, now.Add(time.Hour), "try later"); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueArtistResolutions(ctx, now, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("early due resolutions = %#v, %v", due, err)
	}
	due, err = s.DueArtistResolutions(ctx, now.Add(2*time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].Attempts != 2 || due[0].LastError != "try later" {
		t.Fatalf("due resolutions = %#v, %v", due, err)
	}
}

func TestFollowedArtistCount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
	for i := range 2 {
		artist, err := s.UpsertArtist(ctx, Artist{
			MBID: fmt.Sprintf("artist-%d", i), Name: fmt.Sprintf("Artist %d", i), SortName: fmt.Sprintf("Artist %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			s.Follow(ctx, otherID, artist.ID)
		}
	}
	count, err := s.FollowedArtistCount(ctx, userID)
	if err != nil || count != 2 {
		t.Fatalf("followed artist count=%d err=%v", count, err)
	}
}

func TestAdminDeliveryHistoryPaginationAndDetails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "audit-artist", Name: "Audit Artist", SortName: "Audit Artist"})
	if err := s.AddDestination(ctx, userID, "Phone", "ntfy", []byte("encrypted-secret")); err != nil {
		t.Fatal(err)
	}
	destination, _ := s.Destinations(ctx, userID)
	base := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	for i := range 55 {
		releaseResult, err := s.DB.Exec(`INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("release-%02d", i), artist.ID, fmt.Sprintf("Release %02d", i), "Album", "[]",
			"2026-07-30", 3, "https://musicbrainz.test/release", timeText(base), timeText(base))
		if err != nil {
			t.Fatal(err)
		}
		releaseID, _ := releaseResult.LastInsertId()
		eventResult, err := s.DB.Exec(`INSERT INTO notification_events
			(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
			userID, releaseID, "announcement", fmt.Sprintf("Event %02d", i),
			fmt.Sprintf("Detailed body %02d", i), timeText(base.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		eventID, _ := eventResult.LastInsertId()
		if _, err := s.DB.Exec(`INSERT INTO deliveries
			(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
			eventID, destination[0].ID, "failed", 5, timeText(base.Add(time.Hour)), "provider rejected request"); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.AdminDeliveryHistoryCount(ctx)
	if err != nil || count != 55 {
		t.Fatalf("admin delivery count=%d err=%v", count, err)
	}
	first, err := s.AdminDeliveryHistory(ctx, 50, 0)
	if err != nil || len(first) != 50 || first[0].Title != "Event 54" {
		t.Fatalf("first admin page len=%d first=%#v err=%v", len(first), first[0], err)
	}
	if first[0].UserEmail != "listener@example.com" || first[0].Body != "Detailed body 54" ||
		first[0].Destination != "Phone" || first[0].Service != "ntfy" ||
		first[0].Status != "failed" || first[0].Attempts != 5 ||
		first[0].LastError != "provider rejected request" || first[0].NextAttempt == nil {
		t.Fatalf("admin delivery details=%#v", first[0])
	}
	second, err := s.AdminDeliveryHistory(ctx, 50, 50)
	if err != nil || len(second) != 5 || second[0].Title != "Event 04" {
		t.Fatalf("second admin page=%#v err=%v", second, err)
	}
}

func TestAdminUsersAndDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, _ := s.CreateUser(ctx, "admin@example.com", "unused", "admin", "Europe/Amsterdam")
	memberID, _ := s.CreateUser(ctx, "member@example.com", "unused", "member", "UTC")
	otherMemberID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC")
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "admin-user-artist", Name: "Example", SortName: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, memberID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, memberID, "Phone", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateSession(ctx, memberID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthToken(ctx, "invite", "member@example.com", nil, adminID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthToken(ctx, "invite", "new@example.com", nil, memberID, time.Hour); err != nil {
		t.Fatal(err)
	}

	users, err := s.AdminUsers(ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("admin users=%#v err=%v", users, err)
	}
	if users[0].ID != adminID || users[1].Email != "member@example.com" ||
		users[1].FollowCount != 1 || users[1].DestinationCount != 1 {
		t.Fatalf("unexpected admin users=%#v", users)
	}
	if err := s.DeleteUser(ctx, otherMemberID, memberID); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("member delete error=%v", err)
	}
	if err := s.DeleteUser(ctx, adminID, adminID); !errors.Is(err, ErrCannotDeleteSelf) {
		t.Fatalf("self delete error=%v", err)
	}
	if err := s.DeleteUser(ctx, adminID, memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByID(ctx, memberID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted user lookup error=%v", err)
	}
	for name, query := range map[string]string{
		"sessions":     `SELECT COUNT(*) FROM sessions WHERE user_id=?`,
		"follows":      `SELECT COUNT(*) FROM follows WHERE user_id=?`,
		"destinations": `SELECT COUNT(*) FROM destinations WHERE user_id=?`,
	} {
		var count int
		if err := s.DB.QueryRowContext(ctx, query, memberID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", name, count, err)
		}
	}
	var tokens int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_tokens WHERE email='member@example.com' OR created_by=?`, memberID,
	).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("auth tokens count=%d err=%v", tokens, err)
	}
	var artists int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id=?`, artist.ID).Scan(&artists); err != nil || artists != 1 {
		t.Fatalf("shared artist count=%d err=%v", artists, err)
	}
	if err := s.DeleteUser(ctx, adminID, 99999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing user delete error=%v", err)
	}
}

func TestSpotifyPollDelayIsStableAndSpreadAcrossInterval(t *testing.T) {
	interval := 24 * time.Hour
	first := spotifyPollDelay(1, interval)
	if first != spotifyPollDelay(1, interval) {
		t.Fatal("Spotify poll delay is not stable")
	}
	if first < interval/2 || first >= interval+interval/2 {
		t.Fatalf("Spotify poll delay=%s outside expected range", first)
	}
	if first == spotifyPollDelay(2, interval) {
		t.Fatal("different artists unexpectedly received the same deterministic delay")
	}
}

func TestMigrationsUpgradeVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	versionOne, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(versionOne)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var applied int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=2`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration 2 applied=%d err=%v", applied, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=3`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration 3 applied=%d err=%v", applied, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=4`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration 4 applied=%d err=%v", applied, err)
	}
	var table string
	if err := s.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='artist_resolutions'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	var spotifyBaseline, source, spotifyImage string
	if err := s.DB.QueryRow(`SELECT spotify_baseline_synced_at FROM follows LIMIT 1`).Scan(&spotifyBaseline); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected follows column check error: %v", err)
	}
	if err := s.DB.QueryRow(`SELECT source,spotify_image_url FROM release_groups LIMIT 1`).
		Scan(&source, &spotifyImage); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected release columns check error: %v", err)
	}
	var spotifyNext string
	if err := s.DB.QueryRow(`SELECT spotify_next_check_at FROM artists LIMIT 1`).Scan(&spotifyNext); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected artist scheduling check error: %v", err)
	}
	var unchanged int
	if err := s.DB.QueryRow(`SELECT spotify_unchanged_checks FROM artists LIMIT 1`).Scan(&unchanged); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected adaptive polling column check error: %v", err)
	}
}

func assertEventCount(t *testing.T, s *Store, userID int64, eventType string, want int) {
	t.Helper()
	var got int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=? AND event_type=?`,
		userID, eventType).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s events = %d, want %d", eventType, got, want)
	}
}
