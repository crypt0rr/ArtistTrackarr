package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestArtistMetadataFiltersStatsAndScheduling(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "metadata@example.com", "hash", "member", "UTC", "metadata")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.UpsertArtist(ctx, Artist{
		MBID: "metadata-first", Name: "First Artist", SortName: "First Artist", Type: "Person", Country: "NL",
		Genres: []string{" Country ", "Pop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpsertArtist(ctx, Artist{MBID: "metadata-second", Name: "Second Artist", SortName: "Second Artist", Type: "Group"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artist := range []Artist{first, second} {
		if added, err := s.Follow(ctx, userID, artist.ID); err != nil || !added {
			t.Fatalf("follow %d: added=%v err=%v", artist.ID, added, err)
		}
	}
	if err := s.SaveArtistGenres(ctx, first.ID, []string{"Country", "Pop", "country", "  ", "Rock"}); err != nil {
		t.Fatal(err)
	}
	genres, err := s.ArtistGenres(ctx, first.ID)
	if err != nil || len(genres) != 3 {
		t.Fatalf("artist genres=%#v err=%v", genres, err)
	}
	if err := s.SaveArtistGenres(ctx, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if following, err := s.IsFollowing(ctx, userID, first.ID); err != nil || !following {
		t.Fatalf("IsFollowing existing = %v, %v", following, err)
	}
	if following, err := s.IsFollowing(ctx, userID, 999999); err != nil || following {
		t.Fatalf("IsFollowing missing = %v, %v", following, err)
	}
	if count, err := s.FollowedArtistCount(ctx, userID); err != nil || count != 2 {
		t.Fatalf("follow count=%d err=%v", count, err)
	}

	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	if err := s.SaveListenBrainzStats(ctx, map[int64]ListenBrainzStats{
		first.ID: {ArtistID: first.ID, TotalListenCount: 100, TotalUserCount: 10},
	}, now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	followed, err := s.FollowedArtists(ctx, userID)
	if err != nil || len(followed) != 2 {
		t.Fatalf("followed artists=%#v err=%v", followed, err)
	}
	if followed[0].Genres == nil || followed[0].ListenCount != 100 {
		t.Fatalf("metadata was not enriched: %#v", followed)
	}
	filtered, err := s.FollowedArtistsFiltered(ctx, userID, "country", "NL", "person")
	if err != nil || len(filtered) != 1 || filtered[0].ID != first.ID {
		t.Fatalf("combined filter=%#v err=%v", filtered, err)
	}
	filtered, err = s.FollowedArtistsFilteredPage(ctx, userID, "unknown", "unknown", "group", 1, -1)
	if err != nil || len(filtered) != 1 || filtered[0].ID != second.ID {
		t.Fatalf("unknown filter/page=%#v err=%v", filtered, err)
	}
	if count, err := s.FollowedArtistsFilteredCount(ctx, userID, "country", "", ""); err != nil || count != 1 {
		t.Fatalf("genre count=%d err=%v", count, err)
	}
	for _, dimension := range []string{"genre", "country", "type"} {
		breakdown, err := s.FollowedBreakdown(ctx, userID, dimension)
		if err != nil || len(breakdown) == 0 {
			t.Fatalf("%s breakdown=%#v err=%v", dimension, breakdown, err)
		}
	}
	if _, err := s.FollowedBreakdown(ctx, userID, "unsupported"); err == nil {
		t.Fatal("unsupported breakdown was accepted")
	}
	top, err := s.TopListenBrainzArtists(ctx, userID, 0)
	if err != nil || len(top) != 1 || top[0].ID != first.ID {
		t.Fatalf("top ListenBrainz artists=%#v err=%v", top, err)
	}
	due, err := s.DueListenBrainzArtists(ctx, now.Add(time.Hour), 0)
	if err != nil || len(due) != 1 || due[0].ID != second.ID {
		t.Fatalf("due ListenBrainz artists=%#v err=%v", due, err)
	}
	if err := s.ScheduleListenBrainzRetry(ctx, []int64{second.ID}, now.Add(-time.Hour), "temporary failure"); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueListenBrainzArtists(ctx, now, 10)
	if err != nil || len(due) != 1 || due[0].ID != second.ID {
		t.Fatalf("retried ListenBrainz artist=%#v err=%v", due, err)
	}

	if err := s.MarkArtistChecked(ctx, first.ID, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleArtistCheck(ctx, first.ID, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSpotifyChecked(ctx, first.ID, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleSpotifyCheck(ctx, first.ID, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	state, err := s.SpotifyPollingState(ctx, first.ID)
	if err != nil || state.UnchangedChecks != 0 {
		t.Fatalf("Spotify polling state=%#v err=%v", state, err)
	}
	if err := s.MarkSpotifyCheckedAdaptive(ctx, first.ID, now, time.Hour, false, false); err != nil {
		t.Fatal(err)
	}
	state, err = s.SpotifyPollingState(ctx, first.ID)
	if err != nil || state.UnchangedChecks != 1 || state.LastChangeAt == nil {
		t.Fatalf("adaptive Spotify state=%#v err=%v", state, err)
	}
	if err := s.Unfollow(ctx, userID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Unfollow(ctx, userID, second.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second unfollow error=%v, want sql.ErrNoRows", err)
	}
}

func TestFollowBringsSpotifyScheduleForwardForNewFollower(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	firstUser, err := s.CreateUser(ctx, "first-follower@example.com", "hash", "member", "UTC", "first-follower")
	if err != nil {
		t.Fatal(err)
	}
	secondUser, err := s.CreateUser(ctx, "second-follower@example.com", "hash", "member", "UTC", "second-follower")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "new-follower-schedule", Name: "Scheduled Artist", SpotifyID: "0OdUWJ0sBjDrqHygGUXeCF"})
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, firstUser, artist.ID); err != nil || !added {
		t.Fatalf("first follow added=%v err=%v", added, err)
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	if _, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=? WHERE id=?`, timeText(future), timeText(future), artist.ID); err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, secondUser, artist.ID); err != nil || !added {
		t.Fatalf("second follow added=%v err=%v", added, err)
	}
	var next, spotifyNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at,spotify_next_check_at FROM artists WHERE id=?`, artist.ID).Scan(&next, &spotifyNext); err != nil {
		t.Fatal(err)
	}
	nextAt, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		t.Fatal(err)
	}
	spotifyAt, err := time.Parse(time.RFC3339Nano, spotifyNext)
	if err != nil {
		t.Fatal(err)
	}
	if nextAt.After(time.Now().UTC().Add(time.Second)) || spotifyAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("new follower did not bring schedules forward: next=%v spotify_next=%v", nextAt, spotifyAt)
	}
}

func TestLatestSpotifyReleaseDateAndArtistResolutionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "resolution@example.com", "hash", "member", "UTC", "resolution")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "resolution-artist", Name: "Resolution Artist"})
	if err != nil {
		t.Fatal(err)
	}
	resolution, created, err := s.CreateArtistResolution(ctx, userID, "spotify", "spotify-resolution", "Resolution Artist", "https://open.spotify.com/artist/spotify-resolution", "https://image.example/resolution")
	if err != nil || !created {
		t.Fatal(err)
	}
	resolutions, err := s.ArtistResolutions(ctx, userID)
	if err != nil || len(resolutions) != 1 || resolutions[0].ID != resolution.ID {
		t.Fatalf("artist resolutions=%#v err=%v", resolutions, err)
	}
	if got, err := s.ArtistResolution(ctx, userID, resolution.ID); err != nil || got.ProviderID != "spotify-resolution" {
		t.Fatalf("artist resolution=%#v err=%v", got, err)
	}
	if err := s.MarkArtistResolutionReview(ctx, userID, resolution.ID, []ResolutionCandidate{{MBID: "candidate", Name: "Candidate"}}); err != nil {
		t.Fatal(err)
	}
	reviewed, err := s.ArtistResolution(ctx, userID, resolution.ID)
	if err != nil || reviewed.Status != "review" || len(reviewed.Candidates) != 1 {
		t.Fatalf("reviewed resolution=%#v err=%v", reviewed, err)
	}
	if err := s.RetryArtistResolution(ctx, userID, resolution.ID, 2, time.Now().UTC().Add(time.Minute), "retry later"); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueArtistResolutions(ctx, time.Now().UTC().Add(2*time.Minute), 10)
	if err != nil || len(due) != 1 || due[0].Attempts != 2 {
		t.Fatalf("due resolution=%#v err=%v", due, err)
	}
	if err := s.CancelArtistResolution(ctx, userID, resolution.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelArtistResolution(ctx, userID, resolution.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second resolution cancel error=%v, want sql.ErrNoRows", err)
	}

	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,spotify_id,spotify_url,first_observed_at,updated_at,source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"spotify-release-date", artist.ID, "Latest", "Album", "[]", "2026-08-05", 3, "", "spotify-release-date", "https://open.spotify.com/album/release-date", nowText(), nowText(), "spotify"); err != nil {
		t.Fatal(err)
	}
	date, err := s.LatestSpotifyReleaseDate(ctx, artist.ID)
	if err != nil || date != "2026-08-05" {
		t.Fatalf("latest Spotify release date=%q err=%v", date, err)
	}
	completed, added, err := s.CompleteArtistResolution(ctx, ArtistResolution{ID: 0, UserID: userID}, Artist{})
	if !errors.Is(err, sql.ErrNoRows) || completed.ID != 0 || added {
		t.Fatalf("missing resolution completion=%#v added=%v err=%v", completed, added, err)
	}
}

func TestDueArtistResolutionsAreFairAcrossUsers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	first, err := s.CreateInitialAdmin(ctx, "resolution-fair-first@example.com", "hash", "UTC", "resolution-fair-first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateUser(ctx, "resolution-fair-second@example.com", "hash", "member", "UTC", "resolution-fair-second")
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{first, second} {
		for i := 0; i < maxArtistResolutionBatchPerUser+2; i++ {
			providerID := fmt.Sprintf("fair-%d-%d", userID, i)
			if _, created, err := s.CreateArtistResolution(ctx, userID, "spotify", providerID,
				"Fair Resolution", "https://open.spotify.com/artist/"+providerID, ""); err != nil || !created {
				t.Fatalf("user %d resolution %d created=%v err=%v", userID, i, created, err)
			}
		}
	}

	due, err := s.DueArtistResolutions(ctx, time.Now().UTC().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int64]int{}
	for _, resolution := range due {
		counts[resolution.UserID]++
	}
	if len(due) != 8 || counts[first] != maxArtistResolutionBatchPerUser || counts[second] != maxArtistResolutionBatchPerUser {
		t.Fatalf("due resolutions=%d per user=%v, want eight total and %d each", len(due), counts, maxArtistResolutionBatchPerUser)
	}
	if due, err := s.DueArtistResolutions(ctx, time.Now().UTC(), 0); err != nil || len(due) != 0 {
		t.Fatalf("zero-limit due resolutions=%#v err=%v, want empty result", due, err)
	}
}

func TestMemberOwnedDestinationsAndResolutionsAreBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "bounded-work@example.com", "hash", "member", "UTC", "bounded-work")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxDestinationsPerUser; i++ {
		if err := s.AddDestination(ctx, userID, fmt.Sprintf("Destination %d", i), "ntfy", []byte("encrypted")); err != nil {
			t.Fatalf("destination %d: %v", i, err)
		}
	}
	if err := s.AddDestination(ctx, userID, "one too many", "ntfy", []byte("encrypted")); !errors.Is(err, ErrDestinationLimit) {
		t.Fatalf("destination limit error=%v, want ErrDestinationLimit", err)
	}

	for i := 0; i < maxArtistResolutionsPerUser; i++ {
		if _, created, err := s.CreateArtistResolution(ctx, userID, "spotify", fmt.Sprintf("bounded-%d", i), "Bounded Artist", "https://open.spotify.com/artist/bounded", ""); err != nil || !created {
			t.Fatalf("resolution %d created=%v err=%v", i, created, err)
		}
	}
	duplicate, created, err := s.CreateArtistResolution(ctx, userID, "spotify", "bounded-0", "Bounded Artist", "https://open.spotify.com/artist/bounded", "")
	if err != nil || created || duplicate.ID == 0 {
		t.Fatalf("duplicate resolution=%#v created=%v err=%v", duplicate, created, err)
	}
	if _, created, err := s.CreateArtistResolution(ctx, userID, "spotify", "bounded-extra", "Bounded Artist", "https://open.spotify.com/artist/bounded", ""); !errors.Is(err, ErrArtistResolutionLimit) || created {
		t.Fatalf("resolution limit created=%v err=%v, want ErrArtistResolutionLimit", created, err)
	}
}

func TestCompleteArtistResolutionDoesNotDuplicateExistingFollow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "existing-resolution@example.com", "hash", "member", "UTC", "existing-resolution")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "existing-resolution-artist", Name: "Existing Resolution Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, userID, artist.ID); err != nil || !added {
		t.Fatalf("initial follow added=%v err=%v", added, err)
	}
	resolution, created, err := s.CreateArtistResolution(ctx, userID, "itunes", "existing-resolution-itunes", artist.Name, "https://music.apple.com/us/artist/existing-resolution", "")
	if err != nil || !created {
		t.Fatalf("resolution=%#v created=%v err=%v", resolution, created, err)
	}
	completed, added, err := s.CompleteArtistResolution(ctx, resolution, artist)
	if err != nil || added || completed.ID != artist.ID {
		t.Fatalf("completed=%#v added=%v err=%v", completed, added, err)
	}
	if _, err := s.ArtistResolution(ctx, userID, resolution.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed resolution still present: %v", err)
	}
}
