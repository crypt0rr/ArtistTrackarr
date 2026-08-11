package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreUsesWriterAndReadOnlyPool(t *testing.T) {
	s := testStore(t)
	if s.Reader == nil {
		t.Fatal("reader pool is nil")
	}
	if got := s.DB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer max connections=%d, want 1", got)
	}
	if got := s.Reader.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("reader max connections=%d, want 4", got)
	}
	if _, err := s.Reader.Exec(`CREATE TABLE should_not_write (id INTEGER)`); err == nil {
		t.Fatal("read-only pool accepted a write")
	}
}

func TestStoreConcurrentReadsDuringWrites(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "concurrent@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "concurrent-artist", Name: "Concurrent Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	readErrs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 10; j++ {
				if _, err := s.FollowedArtists(ctx, userID); err != nil {
					readErrs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		if err := s.ScheduleArtistCheck(ctx, artist.ID, time.Now().UTC().Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
	close(readErrs)
	for err := range readErrs {
		t.Fatalf("concurrent read failed: %v", err)
	}
}

func TestSQLiteBusyClassification(t *testing.T) {
	if !sqliteBusy(errors.New("database is locked")) || !sqliteBusy(errors.New("database table is locked")) {
		t.Fatal("lock errors were not classified as retryable")
	}
	if sqliteBusy(errors.New("constraint failed")) {
		t.Fatal("non-lock error was classified as retryable")
	}
}

func TestITunesMigrationPreservesExistingProviderData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
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
	if _, err := db.Exec(`INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`,
		"2026-08-04T10:00:00+02:00", "INFO", "legacy timestamp", "[]"); err != nil {
		t.Fatal(err)
	}
	artistResult, err := db.Exec(`INSERT INTO artists(mbid,name,created_at,updated_at) VALUES('artist-mbid','Legacy Artist',?,?)`, nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	artistID, _ := artistResult.LastInsertId()
	if _, err := db.Exec(`INSERT INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`, userID, artistID, nowText()); err != nil {
		t.Fatal(err)
	}
	releaseResult, err := db.Exec(`INSERT INTO release_groups(mbid,artist_id,title,primary_type,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,spotify_id,source) VALUES('spotify:legacy',?,'Legacy','Album','',0,'',?,?,?,'spotify')`, artistID, nowText(), nowText(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := releaseResult.LastInsertId()
	if _, err := db.Exec(`INSERT INTO provider_observations(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('spotify','legacy',?,'hash',?)`, releaseID, nowText()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Store{DB: db}).CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Legacy", "https://open.spotify.com/artist/spotify-id", ""); err != nil {
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=9`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("artwork migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=11`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("username migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=12`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("timestamp migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=13`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release trust migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=14`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release inbox migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=15`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release evidence migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=16`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release truth decisions migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=17`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("notification holds migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=18`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release calendar migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=19`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("Spotify appearance migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=20`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("delivery assurance migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=21`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("follow notification rules migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=22`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("release credits migration marker=%d err=%v", migrationsApplied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=23`).Scan(&migrationsApplied); err != nil || migrationsApplied != 1 {
		t.Fatalf("review hot indexes migration marker=%d err=%v", migrationsApplied, err)
	}
	for _, indexName := range []string{"idx_provider_observations_release_observed", "idx_follows_artist_user", "idx_import_rows_job_id"} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, indexName).Scan(&found); err != nil {
			t.Fatalf("migration index %q missing: %v", indexName, err)
		}
	}
	var digestTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='release_digest_runs'`).Scan(&digestTable); err != nil {
		t.Fatalf("release digest table missing: %v", err)
	}
	var assuranceTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='delivery_attempts'`).Scan(&assuranceTable); err != nil {
		t.Fatalf("delivery attempts table missing: %v", err)
	}
	var rulesTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='follow_notification_rules'`).Scan(&rulesTable); err != nil {
		t.Fatalf("follow notification rules table missing: %v", err)
	}
	var migratedRules int
	if err := db.QueryRow(`SELECT COUNT(*) FROM follow_notification_rules`).Scan(&migratedRules); err != nil {
		t.Fatalf("follow notification rules backfill failed: %v", err)
	}
	if migratedRules == 0 {
		t.Fatal("follow notification rules migration did not backfill existing follows")
	}
	var digestEnabled int
	if err := db.QueryRow(`SELECT release_digest_enabled FROM notification_preferences WHERE user_id=?`, userID).Scan(&digestEnabled); err != nil || digestEnabled != 0 {
		t.Fatalf("legacy digest default=%d err=%v", digestEnabled, err)
	}
	var evidenceTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='release_provider_evidence'`).Scan(&evidenceTable); err != nil {
		t.Fatalf("release evidence table missing: %v", err)
	}
	var inboxTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='user_release_states'`).Scan(&inboxTable); err != nil {
		t.Fatalf("release inbox table missing: %v", err)
	}
	var truthTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='release_truth_decisions'`).Scan(&truthTable); err != nil {
		t.Fatalf("release truth decisions table missing: %v", err)
	}
	var holdsTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='notification_holds'`).Scan(&holdsTable); err != nil {
		t.Fatalf("notification holds table missing: %v", err)
	}
	var creditsTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='release_credits'`).Scan(&creditsTable); err != nil {
		t.Fatalf("release credits table missing: %v", err)
	}
	var creditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM release_credits WHERE release_group_id=?`, releaseID).Scan(&creditCount); err != nil || creditCount != 1 {
		t.Fatalf("legacy release credit count=%d err=%v", creditCount, err)
	}
	var normalizedLogTime string
	if err := db.QueryRow(`SELECT created_at FROM application_logs WHERE message=?`, "legacy timestamp").Scan(&normalizedLogTime); err != nil {
		t.Fatal(err)
	}
	if normalizedLogTime != "2026-08-04T08:00:00Z" {
		t.Fatalf("normalized log timestamp=%q", normalizedLogTime)
	}
	var legacyUsername string
	if err := db.QueryRow(`SELECT username FROM users WHERE id=?`, userID).Scan(&legacyUsername); err != nil || legacyUsername != "legacy" {
		t.Fatalf("legacy username=%q err=%v", legacyUsername, err)
	}
	var artworkURL string
	if err := db.QueryRow(`SELECT itunes_artwork_url FROM release_groups WHERE id=?`, releaseID).Scan(&artworkURL); err != nil || artworkURL != "" {
		t.Fatalf("legacy artwork URL=%q err=%v", artworkURL, err)
	}
	var source, itunesID string
	if err := db.QueryRow(`SELECT source,COALESCE(itunes_id,'') FROM release_groups WHERE id=?`, releaseID).Scan(&source, &itunesID); err != nil || source != "spotify" || itunesID != "" {
		t.Fatalf("legacy release source=%q itunes=%q err=%v", source, itunesID, err)
	}
	if _, err := db.Exec(`INSERT INTO release_groups(mbid,artist_id,title,primary_type,musicbrainz_url,first_observed_at,updated_at,itunes_id,source) VALUES('itunes:new',?,'New','EP','',?,?,?,'itunes')`, artistID, nowText(), nowText(), "123"); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRecordsMatchRequiresCompatiblePrecision(t *testing.T) {
	base := Release{Title: "Same Release", PrimaryType: "Album", FirstReleaseDate: "2024", DatePrecision: 1}
	if !releaseRecordsMatch(base, Release{Title: "Same Release", PrimaryType: "Album", FirstReleaseDate: "2024", DatePrecision: 1}) {
		t.Fatal("equal year-precision releases did not match")
	}
	if releaseRecordsMatch(base, Release{Title: "Same Release", PrimaryType: "Album", FirstReleaseDate: "2024-06-01", DatePrecision: 3}) {
		t.Fatal("year and day precision releases were merged")
	}
	if releaseRecordsMatch(Release{Title: "Same Release", PrimaryType: "Album", FirstReleaseDate: "2024-06", DatePrecision: 2},
		Release{Title: "Same Release", PrimaryType: "Album", FirstReleaseDate: "2024-06-15", DatePrecision: 3}) {
		t.Fatal("month and day precision releases were merged")
	}
}

func TestImportRowsAreOwnerScopedAndScheduleNewFollows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "importer@example.com", "hash", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "other-importer@example.com", "hash", "member", "UTC", "")
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
	userID, err := s.CreateUser(ctx, "maintenance@example.com", "hash", "member", "UTC", "")
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
	userID, err := s.CreateUser(ctx, "itunes@example.com", "hash", "member", "UTC", "")
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
	itunesRelease := Release{MBID: "itunes:123", Title: "A Release", PrimaryType: "EP", FirstReleaseDate: "2026-08-01", DatePrecision: 3, ITunesID: "123", ITunesURL: "https://music.apple.com/us/album/a-release", ITunesArtworkURL: "https://is1.mzstatic.com/image/250x250bb.jpg"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{itunesRelease}}}, observed); err != nil {
		t.Fatal(err)
	}
	musicBrainzRelease := Release{MBID: "mb-release", Title: "A Release", PrimaryType: "EP", FirstReleaseDate: "2026-08-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/mb-release"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "musicbrainz", Releases: []Release{musicBrainzRelease}}}, observed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	releases, err := s.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].MBID != "mb-release" || releases[0].Source != "both" || releases[0].ITunesID != "123" || releases[0].ITunesArtworkURL == "" || releases[0].SourceCount != 2 || releases[0].Confidence != "confirmed" {
		t.Fatalf("merged releases=%#v err=%v", releases, err)
	}
	if len(releases[0].Sources) != 2 || releases[0].Sources[0] != "itunes" || releases[0].Sources[1] != "musicbrainz" {
		t.Fatalf("merged release sources=%#v", releases[0].Sources)
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

func TestArtistCoverageRecordsProviderOutcomes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "coverage@example.com", "hash", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "coverage-artist", Name: "Coverage Artist", Country: "NL"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	for _, status := range []ArtistProviderStatus{
		{ArtistID: artist.ID, Provider: "spotify", Status: "failed", LastAttemptAt: &now, LastFailureAt: &now, NextCheckAt: storeTestTimePtr(now.Add(time.Hour)), LastError: "rate limited", UpdatedAt: now},
		{ArtistID: artist.ID, Provider: "itunes", Status: "healthy", LastAttemptAt: &now, LastSuccessAt: &now, NextCheckAt: storeTestTimePtr(now.Add(time.Minute)), ReleaseCount: 1, UpdatedAt: now},
		{ArtistID: artist.ID, Provider: "musicbrainz", Status: "healthy", LastAttemptAt: &now, LastSuccessAt: &now, NextCheckAt: storeTestTimePtr(now.Add(time.Minute)), ReleaseCount: 1, UpdatedAt: now},
	} {
		if err := s.RecordArtistProviderStatus(ctx, status); err != nil {
			t.Fatal(err)
		}
	}
	release := Release{MBID: "coverage-release", Title: "Coverage Release", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3, MusicBrainzURL: "https://musicbrainz.org/release-group/coverage-release"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "musicbrainz", Releases: []Release{release}}}, now); err != nil {
		t.Fatal(err)
	}
	coverage, err := s.FollowedArtistCoveragePage(ctx, userID, 50, 0)
	if err != nil || len(coverage) != 1 {
		t.Fatalf("coverage=%#v err=%v", coverage, err)
	}
	if coverage[0].OverallStatus != "attention" || coverage[0].ReleaseCount != 1 || coverage[0].SingleSourceReleases != 1 || coverage[0].FallbackReleases != 0 {
		t.Fatalf("coverage item=%#v", coverage[0])
	}
	if len(coverage[0].ProviderStatuses) != 3 || coverage[0].ProviderStatuses[0].Provider != "spotify" || coverage[0].ProviderStatuses[1].Provider != "itunes" {
		t.Fatalf("provider statuses=%#v", coverage[0].ProviderStatuses)
	}
	summary, err := s.CoverageSummary(ctx, userID)
	if err != nil || summary.Artists != 1 || summary.AttentionArtists != 1 || summary.FallbackReleases != 0 {
		t.Fatalf("coverage summary=%#v err=%v", summary, err)
	}
}

func storeTestTimePtr(value time.Time) *time.Time { return &value }

func TestITunesArtworkBackfillDoesNotCreateNotifications(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "artwork@example.com", "hash", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artwork-artist", Name: "Artwork Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{{
		MBID: "itunes:456", Title: "No Art Yet", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3,
		ITunesID: "456", ITunesURL: "https://music.apple.com/us/album/no-art",
	}}}}, now); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE release_groups SET itunes_artwork_next_check_at=? WHERE itunes_id=?`, timeText(now.Add(-time.Minute)), "456"); err != nil {
		t.Fatal(err)
	}
	artistDue, ok, err := s.DueITunesArtworkArtist(ctx, now)
	if err != nil || !ok || artistDue.ID != artist.ID {
		t.Fatalf("due artist=%#v ok=%v err=%v", artistDue, ok, err)
	}
	checked, updated, err := s.ApplyITunesArtworkBackfill(ctx, artist.ID, []Release{{
		ITunesID: "456", ITunesArtworkURL: "https://is2.mzstatic.com/image/250x250bb.jpg",
	}}, now.Add(time.Minute))
	if err != nil || checked != 1 || updated != 1 {
		t.Fatalf("backfill checked=%d updated=%d err=%v", checked, updated, err)
	}
	releases, err := s.RecentReleases(ctx, userID, 10)
	if err != nil || len(releases) != 1 || releases[0].ITunesArtworkURL == "" {
		t.Fatalf("backfilled releases=%#v err=%v", releases, err)
	}
	var after int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("backfill notification count changed from %d to %d", before, after)
	}
}

func TestReleaseBaselineAndExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{MBID: "undated", Title: "Mystery", PrimaryType: "Album"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
}

func TestInitialMultiSourceSyncCanChooseSpotifyRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{
		MBID: "artist-id", Name: "Pjotr", SortName: "Pjotr", SpotifyID: "spotify-artist",
	})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{
		MBID: "artist-id", Name: "Example", SortName: "Example", SpotifyID: "spotify-artist",
	})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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

func TestSpotifyAppearanceBaselineSuppressesHistoricalFeaturedReleases(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Fridayy", SpotifyID: "spotify-artist"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseSync(ctx, artist, nil, now); err != nil {
		t.Fatal(err)
	}
	oldFeatured := Release{
		MBID: "spotify:old-featured", SpotifyID: "old-featured", Title: "Old Guest Album", PrimaryType: "Album",
		FirstReleaseDate: "2020-01-01", DatePrecision: 3, ArtistCreditRole: "featured",
		SpotifyURL: "https://open.spotify.com/album/old-featured", Source: "spotify",
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{oldFeatured}}}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
	var appearanceBaseline sql.NullString
	if err := s.DB.QueryRow(`SELECT spotify_appears_on_baseline_synced_at FROM follows
		WHERE user_id=? AND artist_id=?`, userID, artist.ID).Scan(&appearanceBaseline); err != nil || !appearanceBaseline.Valid {
		t.Fatalf("appearance baseline=%#v err=%v", appearanceBaseline, err)
	}
	newFeatured := Release{
		MBID: "spotify:new-featured", SpotifyID: "new-featured", Title: "New Guest Single", PrimaryType: "Single",
		FirstReleaseDate: "2026-08-02", DatePrecision: 3, ArtistCreditRole: "featured",
		SpotifyURL: "https://open.spotify.com/album/new-featured", Source: "spotify",
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{oldFeatured, newFeatured}}}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)
	var title, body, role string
	if err := s.DB.QueryRow(`SELECT e.title,e.body,rg.artist_credit_role
		FROM notification_events e JOIN release_groups rg ON rg.id=e.release_group_id WHERE e.user_id=?`, userID).
		Scan(&title, &body, &role); err != nil {
		t.Fatal(err)
	}
	if title != "New featured appearance from Fridayy" || !strings.Contains(body, "appears on") || role != "featured" {
		t.Fatalf("unexpected featured notification title=%q body=%q role=%q", title, body, role)
	}
	var evidenceRole string
	if err := s.DB.QueryRow(`SELECT artist_credit_role FROM release_provider_evidence
		WHERE provider='spotify' AND provider_id=?`, newFeatured.SpotifyID).Scan(&evidenceRole); err != nil || evidenceRole != "featured" {
		t.Fatalf("featured evidence role=%q err=%v", evidenceRole, err)
	}
}

func TestGuestCreditBaselineAndReleaseDetail(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "guest@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "guest-artist-id", Name: "Fridayy"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseSync(ctx, artist, nil, now); err != nil {
		t.Fatal(err)
	}
	historical := Release{
		MBID: "itunes:historical-guest", ITunesID: "historical-guest", ITunesURL: "https://music.apple.com/us/album/historical",
		Title: "Historical Album", PrimaryType: "Album", FirstReleaseDate: "2020-01-01", DatePrecision: 3,
		ArtistCreditRole: "featured", Source: "itunes",
		Credits: []ReleaseCredit{{Provider: "itunes", ProviderID: "track-old", Role: "guest", TrackTitle: "Old collaboration", CreditName: "Fridayy & Other", Confidence: "probable"}},
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{historical}}}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)
	var baseline string
	if err := s.DB.QueryRow(`SELECT baseline_synced_at FROM follow_credit_baselines WHERE user_id=? AND artist_id=? AND provider='itunes' AND role='guest'`, userID, artist.ID).Scan(&baseline); err != nil || baseline == "" {
		t.Fatalf("guest baseline=%q err=%v", baseline, err)
	}
	future := historical
	future.MBID, future.ITunesID, future.ITunesURL = "itunes:future-guest", "future-guest", "https://music.apple.com/us/album/future"
	future.Title, future.FirstReleaseDate = "Future collaboration", "2026-08-05"
	future.Credits = []ReleaseCredit{{Provider: "itunes", ProviderID: "track-new", Role: "guest", TrackTitle: "New collaboration", CreditName: "Fridayy & Other", Confidence: "probable"}}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{historical, future}}}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)
	var title, body string
	if err := s.DB.QueryRow(`SELECT title,body FROM notification_events WHERE user_id=?`, userID).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != "New guest appearance from Fridayy" || !strings.Contains(body, "New collaboration") {
		t.Fatalf("unexpected guest notification title=%q body=%q", title, body)
	}
	var releaseID int64
	if err := s.DB.QueryRow(`SELECT id FROM release_groups WHERE itunes_id='future-guest'`).Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	detail, err := s.ReleaseDetail(ctx, userID, releaseID)
	if err != nil || len(detail.Credits) != 1 || detail.Credits[0].Role != "guest" {
		t.Fatalf("guest detail=%#v err=%v", detail.Credits, err)
	}
}

func TestSpotifyReleaseIsPromotedToMusicBrainzWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SpotifyID: "spotify-artist"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SpotifyID: "spotify-artist"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
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
	userID, err := s.CreateUser(ctx, "listener@example.com", "hash", "member", "UTC", "")
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "Europe/Amsterdam", "")
	artist, _ := s.UpsertArtist(ctx, Artist{MBID: "artist-id", Name: "Example", SortName: "Example"})
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Phone", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 7, 1, 0, 0, time.UTC) // 09:01 in Amsterdam.
	releases := []Release{{
		MBID: "today", Title: "Today", PrimaryType: "Album",
		FirstReleaseDate: "2026-07-30", DatePrecision: 3,
	}, {
		MBID: "today-ep", Title: "Today EP", PrimaryType: "EP",
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
	assertEventCount(t, s, userID, "release_day", 2)
	var deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 {
		t.Fatalf("release-day deliveries=%d, want two unique fan-outs", deliveries)
	}
}

func TestRenameDestinationIsOwnerScopedAndPreservesCredentials(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ownerID, _ := s.CreateUser(ctx, "owner@example.com", "unused", "member", "UTC", "")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC", "")
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
	ownerID, _ := s.CreateUser(ctx, "owner@example.com", "unused", "member", "UTC", "")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC", "")
	resolution, created, err := s.CreateArtistResolution(ctx, ownerID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "https://i.scdn.co/example")
	if err != nil || !created {
		t.Fatalf("create resolution = %#v, %v, created=%v", resolution, err, created)
	}
	duplicate, created, err := s.CreateArtistResolution(ctx, ownerID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "")
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	resolution, _, _ := s.CreateArtistResolution(ctx, userID, "spotify", "spotify-id", "Example", "https://open.spotify.com/artist/spotify-id", "")
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
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
	otherID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC", "")
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
			if _, err := s.Follow(ctx, otherID, artist.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := s.FollowedArtistCount(ctx, userID)
	if err != nil || count != 2 {
		t.Fatalf("followed artist count=%d err=%v", count, err)
	}
}

func TestFollowedArtistsFilteredPageAndCount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "paged@example.com", "unused", "member", "UTC", "")
	for i := range 55 {
		genres := []string{"Pop"}
		if i < 3 {
			genres = []string{"Country"}
		}
		artist, err := s.UpsertArtist(ctx, Artist{
			MBID: fmt.Sprintf("paged-artist-%02d", i), Name: fmt.Sprintf("Artist %02d", i),
			SortName: fmt.Sprintf("Artist %02d", i), Genres: genres,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}

	count, err := s.FollowedArtistsFilteredCount(ctx, userID, "", "", "")
	if err != nil || count != 55 {
		t.Fatalf("all artist count=%d err=%v", count, err)
	}
	first, err := s.FollowedArtistsFilteredPage(ctx, userID, "", "", "", 50, 0)
	if err != nil || len(first) != 50 || first[0].Name != "Artist 00" || first[49].Name != "Artist 49" {
		t.Fatalf("first page len=%d first=%q last=%q err=%v", len(first), firstName(first), lastName(first), err)
	}
	second, err := s.FollowedArtistsFilteredPage(ctx, userID, "", "", "", 50, 50)
	if err != nil || len(second) != 5 || second[0].Name != "Artist 50" || second[4].Name != "Artist 54" {
		t.Fatalf("second page len=%d first=%q last=%q err=%v", len(second), firstName(second), lastName(second), err)
	}

	filteredCount, err := s.FollowedArtistsFilteredCount(ctx, userID, "Country", "", "")
	if err != nil || filteredCount != 3 {
		t.Fatalf("filtered artist count=%d err=%v", filteredCount, err)
	}
	filtered, err := s.FollowedArtistsFilteredPage(ctx, userID, "Country", "", "", 50, 0)
	if err != nil || len(filtered) != 3 {
		t.Fatalf("filtered page len=%d err=%v", len(filtered), err)
	}
}

func firstName(artists []Artist) string {
	if len(artists) == 0 {
		return ""
	}
	return artists[0].Name
}

func lastName(artists []Artist) string {
	if len(artists) == 0 {
		return ""
	}
	return artists[len(artists)-1].Name
}

func TestAdminDeliveryHistoryPaginationAndDetails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _ := s.CreateUser(ctx, "listener@example.com", "unused", "member", "UTC", "")
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

func TestDeliveryErrorsAreRedactedBeforePersistenceAndDisplay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "safe-delivery@example.com", "unused", "member", "UTC", "safe-delivery")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "safe-delivery-artist", Name: "Safe Delivery Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Webhook", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	releaseResult, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "safe-delivery-release", artist.ID, "Safe Release", "Album", "[]", "2026-01-01", 3,
		"https://musicbrainz.org/release-group/safe-delivery-release", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := releaseResult.LastInsertId()
	eventResult, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Safe event", "Safe body", nowText())
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := eventResult.LastInsertId()
	deliveryResult, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries
		(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destinations[0].ID, "pending", 0, nowText(), "")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, err := deliveryResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	message := "send failed to generic+https://user:password@example.test/hook?token=top-secret"
	if err := s.MarkDeliveryFailed(ctx, deliveryID, 1, message, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.DB.QueryRowContext(ctx, `SELECT last_error FROM deliveries WHERE event_id=?`, eventID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "example.test") || strings.Contains(stored, "top-secret") || strings.Contains(stored, "password") {
		t.Fatalf("stored delivery error leaked credentials: %q", stored)
	}
	history, err := s.DeliveryHistory(ctx, userID, 10)
	if err != nil || len(history) != 1 || strings.Contains(history[0].LastError, "example.test") {
		t.Fatalf("history=%#v err=%v", history, err)
	}
}

func TestAdminUsersAndDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, _ := s.CreateUser(ctx, "admin@example.com", "unused", "admin", "Europe/Amsterdam", "")
	memberID, _ := s.CreateUser(ctx, "member@example.com", "unused", "member", "UTC", "")
	otherMemberID, _ := s.CreateUser(ctx, "other@example.com", "unused", "member", "UTC", "")
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

func TestUsernamesAreValidatedUniqueAndEditable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	firstID, err := s.CreateUser(ctx, "first@example.com", "hash", "member", "UTC", "Household.User")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "second@example.com", "hash", "member", "UTC", "household.user"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username error=%v", err)
	}
	if _, err := s.CreateUser(ctx, "invalid@example.com", "hash", "member", "UTC", "no spaces"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("invalid username error=%v", err)
	}
	if err := s.UpdateProfile(ctx, firstID, "Europe/Amsterdam", "08:30", "New.Name"); err != nil {
		t.Fatal(err)
	}
	user, err := s.UserByID(ctx, firstID)
	if err != nil || user.Username != "New.Name" || user.Timezone != "Europe/Amsterdam" || user.ReminderTime != "08:30" {
		t.Fatalf("updated user=%#v err=%v", user, err)
	}
}

func TestInviteUsernameFailureDoesNotConsumeToken(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, err := s.CreateUser(ctx, "admin@example.com", "hash", "admin", "UTC", "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "invite", "member@example.com", nil, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFromInvite(ctx, token, "hash", "bad name", "UTC"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("invalid invite username error=%v", err)
	}
	if err := s.CreateUserFromInvite(ctx, token, "hash", "member", "UTC"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByEmail(ctx, "member@example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestResetPasswordWithTokenIsAtomicAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "reset@example.com", "old-hash", "member", "UTC", "reset-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateSession(ctx, userID, time.Hour); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "reset", "reset@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, token, "new-hash"); err != nil {
		t.Fatal(err)
	}
	user, err := s.UserByID(ctx, userID)
	if err != nil || user.PasswordHash != "new-hash" {
		t.Fatalf("updated user=%#v err=%v", user, err)
	}
	var sessions int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id=?`, userID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	if err := s.ResetPasswordWithToken(ctx, token, "another-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reused reset token error=%v", err)
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
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
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
