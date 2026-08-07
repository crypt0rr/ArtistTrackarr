package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestReleaseInboxPageAndStateAction(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "web-inbox-artist", Name: "Web Inbox Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "web-inbox-release", artist.ID, "Web Inbox Release", "Single", "[]", "2026-08-05", 3,
		"https://musicbrainz.org/release-group/web-inbox-release", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), "itunes")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
		VALUES(?,?,?,?,?,?)`, user.ID, releaseID, "announcement", "Web inbox announcement", "body", now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/inbox")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(page, "Keep every release in view") ||
		!strings.Contains(page, "Web Inbox Release") ||
		!strings.Contains(page, "iTunes only") ||
		!strings.Contains(page, `action="/inbox/`+fmt.Sprint(releaseID)+`/read"`) {
		t.Fatalf("inbox status/body=%d %q", response.StatusCode, page)
	}
	csrf := getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/inbox/"+fmt.Sprint(releaseID)+"/read", url.Values{
		"_csrf": {csrf}, "return": {"/inbox"},
	})
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("read action status=%d body=%q", response.StatusCode, body)
	}
	if count, err := database.ReleaseInboxUnreadCount(ctx, user.ID, now); err != nil || count != 0 {
		t.Fatalf("unread count after read=%d err=%v", count, err)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/inbox/"+fmt.Sprint(releaseID)+"/snooze", url.Values{
		"_csrf": {csrf}, "duration": {"1h"}, "return": {"/inbox"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snooze action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/inbox/"+fmt.Sprint(releaseID)+"/snooze", url.Values{
		"_csrf": {csrf}, "duration": {"invalid"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid snooze status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/inbox/"+fmt.Sprint(releaseID)+"/dismiss", url.Values{
		"_csrf": {csrf}, "return": {"/inbox"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dismiss action status=%d", response.StatusCode)
	}
}
