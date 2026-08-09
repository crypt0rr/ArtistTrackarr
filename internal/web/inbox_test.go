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

	// A different household member must not be able to mutate this release's
	// inbox state or its evidence review, even when they know the numeric IDs.
	otherID, err := database.CreateUser(ctx, "other-inbox@example.com", "hash", "member", "UTC", "other-inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,'open',?,?)`, releaseID, "date_conflict", "warning", "inbox-owner-fingerprint", "Owner-only review",
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var issueID int64
	if err := database.DB.QueryRowContext(ctx, `SELECT id FROM release_evidence_issues WHERE fingerprint=?`, "inbox-owner-fingerprint").Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `UPDATE sessions SET user_id=? WHERE user_id=?`, otherID, user.ID); err != nil {
		t.Fatal(err)
	}
	bad := noRedirectClient(client)
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, bad, server.URL+"/inbox/"+fmt.Sprint(releaseID)+"/read", url.Values{
		"_csrf": {csrf}, "return": {"/inbox"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user inbox action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/coverage/issues")
	response = postForm(t, bad, server.URL+"/coverage/issues/"+fmt.Sprint(issueID)+"/dismiss", url.Values{
		"_csrf": {csrf}, "return": {"/coverage/issues"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user evidence action status=%d", response.StatusCode)
	}
	var reviews int
	if err := database.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_evidence_reviews WHERE user_id=? AND issue_id=?`, otherID, issueID).Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	if reviews != 0 {
		t.Fatalf("cross-user evidence action created %d review rows", reviews)
	}
}

func TestInboxBadgeAppearsAcrossAuthenticatedPages(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "web-nav-badge-artist", Name: "Web Nav Badge Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "web-nav-badge-release", artist.ID, "Web Nav Badge Release", "Single", "[]", "2026-08-09", 3,
		"https://musicbrainz.org/release-group/web-nav-badge-release", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), "itunes")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
		VALUES(?,?,?,?,?,?)`, user.ID, releaseID, "announcement", "Web nav badge announcement", "body", now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/artists", "/calendar", "/coverage", "/settings"} {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		page := string(body)
		if response.StatusCode != http.StatusOK || !strings.Contains(page, `class="nav-count">1</span>`) {
			t.Fatalf("%s status=%d missing unread inbox badge: %q", path, response.StatusCode, page)
		}
	}
}
