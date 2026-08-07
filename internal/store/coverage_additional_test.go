package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/security"
)

func TestFollowMovesInitialSyncForwardOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "follow-coverage@example.com", "hash", "member", "UTC", "follow-coverage")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "follow-coverage-artist", Name: "Follow Coverage"})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, future, artist.ID); err != nil {
		t.Fatal(err)
	}
	added, err := s.Follow(ctx, userID, artist.ID)
	if err != nil || !added {
		t.Fatalf("first follow added=%v err=%v", added, err)
	}
	var first string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	firstTime, err := time.Parse(time.RFC3339Nano, first)
	if err != nil || !firstTime.Before(time.Now().UTC().Add(time.Hour)) {
		t.Fatalf("initial follow did not move sync forward: %q (%v)", first, err)
	}

	added, err = s.Follow(ctx, userID, artist.ID)
	if err != nil || added {
		t.Fatalf("duplicate follow added=%v err=%v", added, err)
	}
	var second string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("duplicate follow moved next check from %q to %q", first, second)
	}
}

func TestArtistByIDParsesNullableSchedulingFields(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artist-by-id-coverage", Name: "Artist By ID"})
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)
	next := checked.Add(3 * time.Hour)
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET last_checked_at=?,spotify_next_check_at=? WHERE id=?`,
		timeText(checked), timeText(next), artist.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.ArtistByID(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastCheckedAt == nil || !loaded.LastCheckedAt.Equal(checked) ||
		loaded.SpotifyNextCheckAt == nil || !loaded.SpotifyNextCheckAt.Equal(next) {
		t.Fatalf("artist timestamps were not parsed: %#v", loaded)
	}
	missing, err := s.ArtistByID(ctx, 999999)
	if !errors.Is(err, sql.ErrNoRows) || missing.ID != 0 {
		t.Fatalf("missing artist=%#v err=%v", missing, err)
	}
}

func TestMarkArtistCheckedUpdatesProviderSchedules(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artist-provider-schedule", Name: "Provider Schedule"})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"itunes", "musicbrainz", "spotify"} {
		if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{ArtistID: artist.ID, Provider: provider, Status: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	if err := s.MarkArtistChecked(ctx, artist.ID, now, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	want := timeText(now.Add(2 * time.Hour))
	var artistNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&artistNext); err != nil {
		t.Fatal(err)
	}
	if artistNext != want {
		t.Fatalf("artist next check=%q, want %q", artistNext, want)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT provider,next_check_at FROM artist_provider_status WHERE artist_id=? ORDER BY provider`, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var provider string
		var next sql.NullString
		if err := rows.Scan(&provider, &next); err != nil {
			t.Fatal(err)
		}
		if provider != "spotify" && (!next.Valid || next.String != want) {
			t.Fatalf("provider %s next check=%q, want %q", provider, next.String, want)
		}
		if provider == "spotify" && next.Valid {
			t.Fatalf("Spotify schedule was changed by canonical check: %q", next.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleSpotifyCheckUpdatesStatusAndRollsBackOnStatusFailure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "spotify-schedule-coverage", Name: "Spotify Schedule Coverage"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{ArtistID: artist.ID, Provider: "spotify", Status: "healthy"}); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	if err := s.ScheduleSpotifyCheck(ctx, artist.ID, next); err != nil {
		t.Fatal(err)
	}
	var artistNext, providerNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&artistNext); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at FROM artist_provider_status WHERE artist_id=? AND provider='spotify'`, artist.ID).Scan(&providerNext); err != nil {
		t.Fatal(err)
	}
	if artistNext != timeText(next) || providerNext != timeText(next) {
		t.Fatalf("Spotify schedules artist=%q provider=%q, want %q", artistNext, providerNext, timeText(next))
	}

	if _, err := s.DB.ExecContext(ctx, `DROP TABLE artist_provider_status`); err != nil {
		t.Fatal(err)
	}
	err = s.ScheduleSpotifyCheck(ctx, artist.ID, next.Add(time.Hour))
	if err == nil {
		t.Fatal("schedule succeeded after provider status table was removed")
	}
	var after string
	if err := s.DB.QueryRowContext(ctx, `SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != artistNext {
		t.Fatalf("failed schedule changed artist timestamp from %q to %q", artistNext, after)
	}
	var beforeChecked, afterChecked sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT last_checked_at FROM artists WHERE id=?`, artist.ID).Scan(&beforeChecked); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArtistChecked(ctx, artist.ID, next, time.Hour); err == nil {
		t.Fatal("artist check succeeded after provider status table was removed")
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT last_checked_at FROM artists WHERE id=?`, artist.ID).Scan(&afterChecked); err != nil {
		t.Fatal(err)
	}
	if beforeChecked.Valid != afterChecked.Valid || beforeChecked.String != afterChecked.String {
		t.Fatalf("failed artist check changed last_checked_at from %#v to %#v", beforeChecked, afterChecked)
	}
}

func TestWriteSchedulingMethodsHonorCanceledContext(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()
	if err := s.MarkArtistChecked(ctx, 1, now, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled MarkArtistChecked error=%v", err)
	}
	if err := s.ScheduleSpotifyCheck(ctx, 1, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ScheduleSpotifyCheck error=%v", err)
	}
	if _, _, err := s.CompleteArtistResolution(ctx, ArtistResolution{ID: 1, UserID: 1}, Artist{MBID: "canceled-resolution"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled CompleteArtistResolution error=%v", err)
	}
}

func TestCompleteArtistResolutionPreservesExistingArtistMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "resolution-metadata@example.com", "hash", "member", "UTC", "resolution-metadata")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{
		MBID: "resolution-metadata-artist", Name: "Old Name", SpotifyID: "existing-spotify",
		SpotifyURL: "https://open.spotify.com/artist/existing-spotify",
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Date(2026, time.August, 6, 8, 0, 0, 0, time.UTC)
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET last_checked_at=? WHERE id=?`, timeText(checked), artist.ID); err != nil {
		t.Fatal(err)
	}
	resolution, created, err := s.CreateArtistResolution(ctx, userID, "itunes", "metadata-itunes", "New Name", "https://music.apple.com/us/artist/new/1", "")
	if err != nil || !created {
		t.Fatalf("resolution=%#v created=%v err=%v", resolution, created, err)
	}
	completed, added, err := s.CompleteArtistResolution(ctx, resolution, Artist{MBID: artist.MBID, Name: "New Name"})
	if err != nil || !added {
		t.Fatalf("completed=%#v added=%v err=%v", completed, added, err)
	}
	if completed.Name != "New Name" || completed.SpotifyID != "existing-spotify" ||
		completed.LastCheckedAt == nil || !completed.LastCheckedAt.Equal(checked) {
		t.Fatalf("existing artist metadata was not preserved: %#v", completed)
	}
}

func TestUpdateProfileRejectsUsernameOwnedByAnotherUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	firstID, err := s.CreateUser(ctx, "profile-owner@example.com", "hash", "member", "UTC", "profile-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "profile-other@example.com", "hash", "member", "Europe/Amsterdam", "profile-other"); err != nil {
		t.Fatal(err)
	}

	err = s.UpdateProfile(ctx, firstID, "America/New_York", "08:30", "PROFILE-OTHER")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("cross-user username update error=%v, want ErrUsernameTaken", err)
	}
	user, err := s.UserByID(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "profile-owner" || user.Timezone != "UTC" || user.ReminderTime != "09:00" {
		t.Fatalf("failed profile update changed user=%#v", user)
	}
}

func TestDeleteUserCanRemoveSecondAdministrator(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actingAdmin, err := s.CreateUser(ctx, "acting-admin@example.com", "hash", "admin", "UTC", "acting-admin")
	if err != nil {
		t.Fatal(err)
	}
	otherAdmin, err := s.CreateUser(ctx, "other-admin@example.com", "hash", "admin", "UTC", "other-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, actingAdmin, otherAdmin); err != nil {
		t.Fatalf("delete second administrator: %v", err)
	}
	if _, err := s.UserByID(ctx, otherAdmin); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted administrator lookup error=%v, want sql.ErrNoRows", err)
	}
	remaining, err := s.UserByID(ctx, actingAdmin)
	if err != nil || remaining.Role != "admin" {
		t.Fatalf("acting administrator was not preserved: %#v err=%v", remaining, err)
	}
}

func TestCreateArtistResolutionValidatesProviderIdentity(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "resolution-validation@example.com", "hash", "member", "UTC", "resolution-validation")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		provider string
		id       string
		artist   string
		url      string
		wantErr  string
	}{
		{name: "unsupported provider", provider: "musicbrainz", id: "mbid", artist: "Example", url: "https://example.test"},
		{name: "missing id", provider: "spotify", artist: "Example", url: "https://open.spotify.com/artist/id"},
		{name: "missing name", provider: "itunes", id: "123", url: "https://music.apple.com/artist/example"},
		{name: "missing url", provider: "spotify", id: "id", artist: "Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.CreateArtistResolution(ctx, userID, tc.provider, tc.id, tc.artist, tc.url, " image ")
			if err == nil {
				t.Fatal("incomplete provider identity was accepted")
			}
		})
	}
}

func TestCompleteArtistResolutionPersistsGenresWithoutProviderLeakage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "itunes-resolution@example.com", "hash", "member", "UTC", "itunes-resolution")
	if err != nil {
		t.Fatal(err)
	}
	resolution, created, err := s.CreateArtistResolution(ctx, userID, "itunes", "itunes-artist-42", "Genre Artist", "https://music.apple.com/us/artist/genre-artist/42", "")
	if err != nil || !created {
		t.Fatalf("resolution=%#v created=%v err=%v", resolution, created, err)
	}
	completed, added, err := s.CompleteArtistResolution(ctx, resolution, Artist{
		MBID: "genre-artist-mbid", Name: "Genre Artist", Genres: []string{"Country", "country", " Americana "},
	})
	if err != nil || !added || completed.SpotifyID != "" {
		t.Fatalf("completed=%#v added=%v err=%v", completed, added, err)
	}
	genres, err := s.ArtistGenres(ctx, completed.ID)
	if err != nil || len(genres) != 2 || genres[0] != "Country" || genres[1] != "Americana" {
		t.Fatalf("resolved genres=%#v err=%v", genres, err)
	}
	followed, err := s.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 1 || followed[0].MBID != "genre-artist-mbid" {
		t.Fatalf("resolved follows=%#v err=%v", followed, err)
	}
}

func TestITunesArtworkBackfillNegativeCacheAndURLValidation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "artwork-negative@example.com", "hash", "member", "UTC", "artwork-negative")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artwork-negative-artist", Name: "Artwork Negative"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "itunes", Releases: []Release{
		{MBID: "itunes:artwork-good", ITunesID: "artwork-good", Title: "Good Art", PrimaryType: "Album", FirstReleaseDate: "2026-08-01", DatePrecision: 3},
		{MBID: "itunes:artwork-missing", ITunesID: "artwork-missing", Title: "Missing Art", PrimaryType: "Single", FirstReleaseDate: "2026-08-02", DatePrecision: 3},
	}}}, now); err != nil {
		t.Fatal(err)
	}
	checked, updated, err := s.ApplyITunesArtworkBackfill(ctx, artist.ID, []Release{
		{ITunesID: "artwork-good", ITunesArtworkURL: "https://is1.mzstatic.com/image/250x250bb.jpg"},
		{ITunesID: "artwork-missing", ITunesArtworkURL: "https://example.test/not-allowed.jpg"},
	}, now)
	if err != nil || checked != 2 || updated != 1 {
		t.Fatalf("backfill checked=%d updated=%d err=%v", checked, updated, err)
	}
	var goodURL, missingNext string
	var attempts int
	if err := s.DB.QueryRowContext(ctx, `SELECT itunes_artwork_url FROM release_groups WHERE itunes_id=?`, "artwork-good").Scan(&goodURL); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT itunes_artwork_next_check_at,itunes_artwork_attempts FROM release_groups WHERE itunes_id=?`, "artwork-missing").Scan(&missingNext, &attempts); err != nil {
		t.Fatal(err)
	}
	if goodURL == "" || attempts != 1 {
		t.Fatalf("artwork metadata good=%q attempts=%d", goodURL, attempts)
	}
	next, err := time.Parse(time.RFC3339Nano, missingNext)
	if err != nil || !next.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("negative artwork cache next=%q parsed=%v err=%v", missingNext, next, err)
	}
}

func TestSpotifyAdaptivePollingKeepsUpcomingReleasesOnBaseCadence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "adaptive-upcoming-artist", Name: "Adaptive Upcoming"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, time.Hour, true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, time.Hour, false, true); err != nil {
		t.Fatal(err)
	}
	var upcomingNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&upcomingNext); err != nil {
		t.Fatal(err)
	}
	upcoming, err := time.Parse(time.RFC3339Nano, upcomingNext)
	if err != nil {
		t.Fatal(err)
	}
	baseDelay := spotifyPollDelay(artist.ID, time.Hour)
	if !upcoming.Equal(now.Add(baseDelay)) {
		t.Fatalf("upcoming next=%v, want base cadence %v", upcoming, now.Add(baseDelay))
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET spotify_unchanged_checks=0 WHERE id=?`, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSpotifyCheckedAdaptive(ctx, artist.ID, now, time.Hour, false, false); err != nil {
		t.Fatal(err)
	}
	var backoffNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&backoffNext); err != nil {
		t.Fatal(err)
	}
	backoff, err := time.Parse(time.RFC3339Nano, backoffNext)
	expectedBackoff := spotifyPollDelay(artist.ID, 2*time.Hour)
	if expectedBackoff > 2*time.Hour {
		expectedBackoff = 2 * time.Hour
	}
	if err != nil || !backoff.Equal(now.Add(expectedBackoff)) {
		t.Fatalf("backoff next=%v raw=%q err=%v", backoff, backoffNext, err)
	}
}

func TestFollowedArtistPageDefaultsAndEmptyBounds(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "page-bounds@example.com", "hash", "member", "UTC", "page-bounds")
	if err != nil {
		t.Fatal(err)
	}
	for _, mbid := range []string{"page-bounds-a", "page-bounds-b"} {
		artist, err := s.UpsertArtist(ctx, Artist{MBID: mbid, Name: mbid})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.FollowedArtistsFilteredPage(ctx, userID, "", "", "", 0, -10)
	if err != nil || len(page) != 2 {
		t.Fatalf("default page=%#v err=%v", page, err)
	}
	page, err = s.FollowedArtistsFilteredPage(ctx, userID, "", "", "", 10, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("out-of-range page=%#v err=%v", page, err)
	}
	if count, err := s.FollowedArtistCount(ctx, 999999); err != nil || count != 0 {
		t.Fatalf("empty user count=%d err=%v", count, err)
	}
}

func TestConsumeAuthTokenReturnsLinkedUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "linked-token@example.com", "hash", "member", "UTC", "linked-token")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "invite", "linked-token@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	email, linked, err := s.ConsumeAuthToken(ctx, token, "invite")
	if err != nil || email != "linked-token@example.com" || linked == nil || *linked != userID {
		t.Fatalf("consumed token email=%q linked=%#v err=%v", email, linked, err)
	}
	if _, _, err := s.ConsumeAuthToken(ctx, token, "invite"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("linked token was reusable: %v", err)
	}
}

func TestConsumeAuthTokenIsSingleUseUnderConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, err := s.CreateUser(ctx, "token-admin@example.com", "hash", "admin", "UTC", "token-admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "invite", "replay@example.com", nil, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		email string
		err   error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			email, _, err := s.ConsumeAuthToken(ctx, token, "invite")
			results <- result{email: email, err: err}
		}()
	}
	group.Wait()
	close(results)
	var successes, replays int
	for item := range results {
		if item.err == nil && item.email == "replay@example.com" {
			successes++
		} else if errors.Is(item.err, sql.ErrNoRows) {
			replays++
		} else {
			t.Fatalf("unexpected concurrent token result=%#v", item)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("concurrent token outcomes successes=%d replays=%d", successes, replays)
	}
}

func TestFollowIsIdempotentUnderConcurrentRequests(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "follow-concurrent@example.com", "hash", "member", "UTC", "follow-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "follow-concurrent-artist", Name: "Follow Concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			added, err := s.Follow(ctx, userID, artist.ID)
			results <- added
			errs <- err
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	addedCount := 0
	for added := range results {
		if added {
			addedCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent follow error=%v", err)
		}
	}
	if addedCount != 1 {
		t.Fatalf("concurrent follow added=%d, want 1", addedCount)
	}
	var follows int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=? AND artist_id=?`, userID, artist.ID).Scan(&follows); err != nil || follows != 1 {
		t.Fatalf("follow rows=%d err=%v", follows, err)
	}
}

func TestResetPasswordTokenCannotBeReplayedConcurrently(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "reset-concurrent@example.com", "old-hash", "member", "UTC", "reset-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "reset", "reset-concurrent@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, hash := range []string{"new-hash-one", "new-hash-two"} {
		group.Add(1)
		go func(hash string) {
			defer group.Done()
			results <- s.ResetPasswordWithToken(ctx, token, hash)
		}(hash)
	}
	group.Wait()
	close(results)
	var successes, replays int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, sql.ErrNoRows):
			replays++
		default:
			t.Fatalf("unexpected concurrent reset error=%v", err)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("concurrent reset outcomes successes=%d replays=%d", successes, replays)
	}
	user, err := s.UserByID(ctx, userID)
	if err != nil || (user.PasswordHash != "new-hash-one" && user.PasswordHash != "new-hash-two") {
		t.Fatalf("password after concurrent reset=%q err=%v", user.PasswordHash, err)
	}
}

func TestResetPasswordRejectsTokenWithoutLinkedUserWithoutConsumingIt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, err := s.CreateUser(ctx, "reset-ownerless-admin@example.com", "hash", "admin", "UTC", "reset-ownerless-admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAuthToken(ctx, "reset", "ownerless@example.com", nil, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, token, "should-not-apply"); err == nil || err.Error() != "reset token has no user" {
		t.Fatalf("ownerless reset error=%v", err)
	}
	var used sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT used_at FROM auth_tokens WHERE token_hash=?`, security.Digest(token)).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used.Valid {
		t.Fatal("ownerless reset token was consumed")
	}
}

func TestLoginAllowanceExpiresAfterStoredBlockWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	key := "expired-block@example.com"
	if _, err := s.CreateUser(ctx, key, "hash", "member", "UTC", "expired-block"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO login_attempts(key_hash,failures,first_at,blocked_until) VALUES(?,?,?,?)`,
		security.Digest(key), 5, timeText(time.Now().UTC().Add(-20*time.Minute)), timeText(time.Now().UTC().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	allowed, err := s.LoginAllowed(ctx, key)
	if err != nil || !allowed {
		t.Fatalf("expired block allowance=%v err=%v", allowed, err)
	}
}

func TestDerivedUsernameNormalizesAndSuffixesDeterministically(t *testing.T) {
	taken := map[string]struct{}{"long-name-tag": {}, "long-name-tag-2": {}}
	if got := derivedUsername("Long Name+tag@example.com", 7, taken); got != "long-name-tag-3" {
		t.Fatalf("derived username=%q", got)
	}
	if got := derivedUsername("@@", 42, nil); got != "user-42" {
		t.Fatalf("fallback username=%q", got)
	}
	long := derivedUsername("abcdefghijklmnopqrstuvwxyz0123456789@example.com", 1, nil)
	if len(long) != 32 {
		t.Fatalf("long derived username length=%d value=%q", len(long), long)
	}
}

func TestReleaseProjectionLabelsCoverProviderSources(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: " musicbrainz ", want: "MusicBrainz"},
		{provider: "SPOTIFY", want: "Spotify"},
		{provider: "itunes", want: "iTunes"},
		{provider: "other", want: "other"},
	} {
		if got := providerLabel(test.provider); got != test.want {
			t.Errorf("providerLabel(%q)=%q, want %q", test.provider, got, test.want)
		}
	}
	for _, test := range []struct {
		source string
		count  int
		want   string
	}{
		{source: "both", count: 1, want: "confirmed"},
		{source: "spotify", count: 2, want: "confirmed"},
		{source: "musicbrainz", count: 1, want: "canonical"},
		{source: "spotify", count: 1, want: "spotify"},
		{source: "itunes", count: 1, want: "itunes"},
		{source: "unknown", count: 1, want: "unconfirmed"},
		{source: "unknown", count: 0, want: "unconfirmed"},
	} {
		if got := releaseConfidence(test.source, test.count); got != test.want {
			t.Errorf("releaseConfidence(%q,%d)=%q, want %q", test.source, test.count, got, test.want)
		}
	}
}

func TestApplyReleaseBatchesRejectsDuplicateProviderBeforeCommit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "duplicate-provider-artist", Name: "Duplicate Provider Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{
		{Provider: "musicbrainz", Releases: []Release{{
			MBID: "duplicate-provider-release", Title: "Duplicate Provider", PrimaryType: "Album",
			FirstReleaseDate: "2026-08-07", DatePrecision: 3,
		}}},
		{Provider: " MUSICBRAINZ ", Releases: []Release{{
			MBID: "duplicate-provider-release-2", Title: "Should Roll Back", PrimaryType: "Album",
		}}},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "duplicate release batch for musicbrainz") {
		t.Fatalf("duplicate provider error=%v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_groups WHERE artist_id=?`, artist.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("duplicate provider rollback left %d release groups", count)
	}
}

func TestNotificationHoldScannerParsesReleasedAt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "released-hold@example.com", "hash", "member", "UTC", "released-hold")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "released-hold-artist", Name: "Released Hold Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	releaseResult, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "released-hold-release", artist.ID, "Released Hold Release", "Album", "[]", "2026-08-07", 3,
		"https://musicbrainz.org/release-group/released-hold-release", "itunes", timeText(now), timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := releaseResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	released := now.Add(time.Minute)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at,released_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, userID, releaseID, "announcement", "Held", "Body", "review", "fingerprint",
		timeText(now), "held", timeText(now), timeText(released)); err != nil {
		t.Fatal(err)
	}
	holds, err := s.NotificationHolds(ctx, userID, 10)
	if err != nil || len(holds) != 1 {
		t.Fatalf("holds=%#v err=%v", holds, err)
	}
	if holds[0].ReleasedAt == nil || !holds[0].ReleasedAt.Equal(released) {
		t.Fatalf("released_at=%v, want %v", holds[0].ReleasedAt, released)
	}
}

func TestQueueDueReleaseDaysSkipsInvalidAndFutureSchedules(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	lateUser, err := s.CreateUser(ctx, "release-day-late@example.com", "hash", "member", "UTC", "release-day-late")
	if err != nil {
		t.Fatal(err)
	}
	invalidUser, err := s.CreateUser(ctx, "release-day-invalid@example.com", "hash", "member", "UTC", "release-day-invalid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET timezone='Not/AZone' WHERE id=?`, invalidUser); err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "release-day-coverage-artist", Name: "Release Day Coverage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{lateUser, invalidUser} {
		if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET reminder_time='23:00' WHERE id=?`, lateUser); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "release-day-today", artist.ID, "Release Day Today", "Album", "[]",
		now.Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/release-day-today",
		"https://open.spotify.com/album/release-day-today", "musicbrainz", timeText(now), timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	todayID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, lateUser, "release_day", 0)
	assertEventCount(t, s, invalidUser, "release_day", 0)

	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET reminder_time='00:00' WHERE id=?`, lateUser); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, lateUser, "release_day", 1)
	var body string
	if err := s.DB.QueryRowContext(ctx, `SELECT body FROM notification_events WHERE user_id=? AND release_group_id=?`, lateUser, todayID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "https://open.spotify.com/album/release-day-today") {
		t.Fatalf("release-day body=%q, missing preferred Spotify link", body)
	}

	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "release-day-tomorrow", artist.ID, "Release Day Tomorrow", "EP", "[]",
		now.AddDate(0, 0, 1).Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/release-day-tomorrow",
		"musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, lateUser, "release_day", 1)
}

func TestLegacyUsernameMigrationSanitizesAndSuffixesRows(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		CREATE TABLE users(id INTEGER PRIMARY KEY,email TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"Alice+tag@example.com", "alice tag@example.com", "@@@example.com"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(email) VALUES(?)`, email); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/011_usernames.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Store{DB: db}).migrateUsernames(ctx, body); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT username FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			t.Fatal(err)
		}
		got = append(got, username)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"alice-tag", "alice-tag-2", "user-3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("migrated usernames=%v, want %v", got, want)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(email,username) VALUES('collision@example.com','ALICE-TAG')`); err == nil {
		t.Fatal("case-insensitive username index was not created")
	}
}

func TestLegacyUsernameMigrationRollsBackInvalidDDL(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		CREATE TABLE users(id INTEGER PRIMARY KEY,email TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	body := []byte(`ALTER TABLE users ADD COLUMN username TEXT NOT NULL DEFAULT '' COLLATE NOCASE;
		THIS IS NOT VALID SQL`)
	if err := (&Store{DB: db}).migrateUsernames(ctx, body); err == nil {
		t.Fatal("invalid username migration unexpectedly succeeded")
	}
	var columnCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='username'`).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 0 {
		t.Fatal("failed username migration left a partially added column")
	}
}

func TestITunesFallbackMigrationRollsBackOnLegacySchemaError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.migrateITunesFallback(context.Background()); err == nil {
		t.Fatal("iTunes fallback migration accepted a schema without release_groups")
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign key enforcement=%d, want enabled after failed migration", foreignKeys)
	}
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='release_groups_itunes'`).Scan(&table); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed migration left temporary table %q (err=%v)", table, err)
	}
}

func TestOperationalTimestampMigrationNormalizesSQLiteDatetime(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=12`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`,
		"2026-08-07 14:15:16", "INFO", "space timestamp", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateOperationalTimestamps(ctx); err != nil {
		t.Fatal(err)
	}
	var normalized string
	if err := s.DB.QueryRowContext(ctx, `SELECT created_at FROM application_logs WHERE message=?`, "space timestamp").Scan(&normalized); err != nil {
		t.Fatal(err)
	}
	if normalized != "2026-08-07T14:15:16Z" {
		t.Fatalf("normalized timestamp=%q", normalized)
	}
	var marker int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=12`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("timestamp migration marker=%d err=%v", marker, err)
	}
}

func TestOperationalTimestampMigrationRollsBackInvalidData(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=12`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`,
		"not-a-timestamp", "INFO", "invalid timestamp", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateOperationalTimestamps(ctx); err == nil || !strings.Contains(err.Error(), "invalid timestamp") {
		t.Fatalf("invalid timestamp migration error=%v", err)
	}
	var raw string
	if err := s.DB.QueryRowContext(ctx, `SELECT created_at FROM application_logs WHERE message=?`, "invalid timestamp").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "not-a-timestamp" {
		t.Fatalf("rollback changed invalid timestamp to %q", raw)
	}
	var marker int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=12`).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("failed timestamp migration marker=%d err=%v", marker, err)
	}
}

func TestApplyReleaseBatchesRollsBackWhenLaterProviderIsUnsupported(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "batch-rollback-artist", Name: "Batch Rollback Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{
		{Provider: "musicbrainz", Releases: []Release{{
			MBID: "batch-rollback-release", Title: "Should Roll Back", PrimaryType: "Album",
			FirstReleaseDate: "2026-08-07", DatePrecision: 3,
		}}},
		{Provider: "unsupported", Releases: []Release{{
			MBID: "batch-rollback-unsupported", Title: "Unsupported", PrimaryType: "Album",
		}}},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "unsupported release provider") {
		t.Fatalf("unsupported provider error=%v", err)
	}
	for _, table := range []string{"release_groups", "provider_observations", "release_evidence_issues", "notification_events"} {
		var count int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("rollback left %d rows in %s", count, table)
		}
	}
}

func TestApplyReleaseBatchesRollsBackDuplicateProviderBatches(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "batch-duplicate-artist", Name: "Batch Duplicate Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{
		{Provider: "musicbrainz", Releases: []Release{{
			MBID: "batch-duplicate-release-1", Title: "First Batch", PrimaryType: "Album",
			FirstReleaseDate: "2026-08-07", DatePrecision: 3,
		}}},
		{Provider: "MUSICBRAINZ", Releases: []Release{{
			MBID: "batch-duplicate-release-2", Title: "Second Batch", PrimaryType: "Album",
			FirstReleaseDate: "2026-08-14", DatePrecision: 3,
		}}},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "duplicate release batch for musicbrainz") {
		t.Fatalf("duplicate provider error=%v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_groups WHERE artist_id=?`, artist.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("duplicate provider rollback left %d release rows", count)
	}
}

func TestCompleteArtistResolutionRejectsForeignOwnerWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "resolution-owner@example.com", "hash", "member", "UTC", "resolution-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "resolution-other@example.com", "hash", "member", "UTC", "resolution-other")
	if err != nil {
		t.Fatal(err)
	}
	resolution, created, err := s.CreateArtistResolution(ctx, owner, "spotify", "foreign-resolution", "Foreign Resolution", "https://open.spotify.com/artist/foreign-resolution", "")
	if err != nil || !created {
		t.Fatalf("resolution=%#v created=%v err=%v", resolution, created, err)
	}
	_, added, err := s.CompleteArtistResolution(ctx, ArtistResolution{ID: resolution.ID, UserID: other}, Artist{
		MBID: "foreign-resolution-mbid", Name: "Should Not Follow",
	})
	if !errors.Is(err, sql.ErrNoRows) || added {
		t.Fatalf("foreign completion added=%v err=%v", added, err)
	}
	if _, err := s.ArtistByMBID(ctx, "foreign-resolution-mbid"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign completion created artist: %v", err)
	}
	resolutions, err := s.ArtistResolutions(ctx, owner)
	if err != nil || len(resolutions) != 1 || resolutions[0].ID != resolution.ID {
		t.Fatalf("owner resolution after foreign completion=%#v err=%v", resolutions, err)
	}
}

func TestSaveImportRowRejectsForeignJobOwnerWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "import-job-owner@example.com", "hash", "member", "UTC", "import-job-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "import-job-other@example.com", "hash", "member", "UTC", "import-job-other")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateImportJob(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	input := ImportInput{SourceValue: "import-owner-mbid", DisplayName: "Import Owner Artist", MBID: "import-owner-mbid"}
	if _, err := s.SaveImportRow(ctx, other, job.ID, input); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign import row error=%v", err)
	}
	var rows, artists int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_rows`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE mbid=?`, input.MBID).Scan(&artists); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || artists != 0 {
		t.Fatalf("foreign import changed rows=%d artists=%d", rows, artists)
	}
	row, err := s.SaveImportRow(ctx, owner, job.ID, input)
	if err != nil || row.Status != "added" {
		t.Fatalf("owner import row=%#v err=%v", row, err)
	}
}
