package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestCalendarICSUsesCorrectedDateForUnclaimedMergedProviderRow(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{
		MBID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "Merged Provider Artist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(24 * time.Hour).Add(8 * time.Hour)
	originalDate := observed.AddDate(0, 0, 5).Format("2006-01-02")
	correctedDate := observed.AddDate(0, 0, 12).Format("2006-01-02")
	spotify := store.Release{
		SpotifyID: "calendar-merged-spotify", SpotifyURL: "https://open.spotify.com/album/calendar-merged-spotify",
		Title: "Calendar Merged Release", PrimaryType: "Album", FirstReleaseDate: originalDate, DatePrecision: 3,
	}
	itunes := store.Release{
		ITunesID: "calendar-merged-itunes", ITunesURL: "https://music.apple.com/us/album/calendar-merged-itunes",
		Title: spotify.Title, PrimaryType: spotify.PrimaryType, FirstReleaseDate: originalDate, DatePrecision: 3,
	}
	for _, batch := range []store.ReleaseBatch{
		{Provider: "spotify", Releases: []store.Release{spotify}},
		{Provider: "itunes", Releases: []store.Release{itunes}},
	} {
		if err := database.ApplyReleaseBatches(ctx, artist, []store.ReleaseBatch{batch}, observed); err != nil {
			t.Fatalf("apply %s batch: %v", batch.Provider, err)
		}
	}
	correctedSpotify := spotify
	correctedSpotify.FirstReleaseDate = correctedDate
	if err := database.ApplyReleaseBatches(ctx, artist, []store.ReleaseBatch{{Provider: "spotify", Releases: []store.Release{correctedSpotify}}}, observed.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/calendar.ics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	ics := strings.ReplaceAll(string(body), "\r\n ", "")
	if response.StatusCode != http.StatusOK || !strings.Contains(ics, "SUMMARY:Calendar Merged Release — Merged Provider Artist") ||
		!strings.Contains(ics, "DTSTART;VALUE=DATE:"+strings.ReplaceAll(correctedDate, "-", "")) ||
		strings.Contains(ics, "DTSTART;VALUE=DATE:"+strings.ReplaceAll(originalDate, "-", "")) {
		t.Fatalf("calendar ICS status/body=%d %q, want only corrected release date %s", response.StatusCode, body, correctedDate)
	}
}

func TestCalendarFeedRouteGeneratesRotatesAndRevokesToken(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"generate"},
	})
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Copy this URL now") {
		t.Fatalf("generate status/body=%d %q", response.StatusCode, body)
	}
	tokenPattern := regexp.MustCompile(`/calendar/feed/([A-Za-z0-9_-]+)`)
	match := tokenPattern.FindStringSubmatch(string(body))
	if len(match) != 2 {
		t.Fatalf("generated feed URL missing from settings: %q", body)
	}
	first := match[1]
	publicClient := &http.Client{}
	feed, err := publicClient.Get(server.URL + "/calendar/feed/" + first)
	if err != nil {
		t.Fatal(err)
	}
	feedBody, _ := io.ReadAll(feed.Body)
	_ = feed.Body.Close()
	if feed.StatusCode != http.StatusOK || !strings.Contains(feed.Header.Get("Content-Type"), "text/calendar") || !strings.Contains(string(feedBody), "BEGIN:VCALENDAR") {
		t.Fatalf("feed status/content=%d %q", feed.StatusCode, feedBody)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"rotate"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	match = tokenPattern.FindStringSubmatch(string(body))
	if response.StatusCode != http.StatusOK || len(match) != 2 || match[1] == first {
		t.Fatalf("rotate status/body=%d %q", response.StatusCode, body)
	}
	second := match[1]
	oldFeed, err := publicClient.Get(server.URL + "/calendar/feed/" + first)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldFeed.Body.Close()
	if oldFeed.StatusCode != http.StatusNotFound {
		t.Fatalf("rotated old token status=%d, want 404", oldFeed.StatusCode)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"revoke"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", response.StatusCode)
	}
	revoked, err := publicClient.Get(server.URL + "/calendar/feed/" + second)
	if err != nil {
		t.Fatal(err)
	}
	_ = revoked.Body.Close()
	if revoked.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked token status=%d, want 404", revoked.StatusCode)
	}
}
