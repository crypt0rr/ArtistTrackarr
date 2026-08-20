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
