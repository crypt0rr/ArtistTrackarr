package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/security"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func postArtistImportCSV(t *testing.T, client *http.Client, target, csrf, content string) *http.Response {
	t.Helper()
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	if err := writer.WriteField("_csrf", csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "artists.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, target, &payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type actionITunes struct {
	artists   map[string]catalog.ITunesArtist
	searchErr error
}

func (f *actionITunes) SearchArtists(context.Context, string) ([]catalog.ITunesArtist, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	result := make([]catalog.ITunesArtist, 0, len(f.artists))
	for _, artist := range f.artists {
		result = append(result, artist)
	}
	return result, nil
}

func (f *actionITunes) Artist(_ context.Context, id string) (catalog.ITunesArtist, error) {
	if artist, ok := f.artists[id]; ok {
		return artist, nil
	}
	return catalog.ITunesArtist{}, errors.New("artist not found")
}

func noRedirectClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func TestArtistActionsReadyAndLogout(t *testing.T) {
	followMBID := "99999999-9999-4999-8999-999999999999"
	database, server, client := authenticatedTestServer(t, &searchCatalog{
		resolved: map[string]catalog.ArtistResult{
			followMBID: {MBID: followMBID, Name: "Followed By Route", SortName: "Followed By Route"},
		},
	}, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := client.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = ready.Body.Close()
	if ready.StatusCode != http.StatusNoContent {
		t.Fatalf("ready status=%d", ready.StatusCode)
	}
	if ready.Header.Get("X-ArtistTrackarr-Database") != "healthy" ||
		ready.Header.Get("X-ArtistTrackarr-Operational") != "" ||
		ready.Header.Get("X-ArtistTrackarr-Operational-Reason") != "" ||
		ready.Header.Get("X-ArtistTrackarr-Runner") != "" {
		t.Fatalf("readiness exposed operational headers: database=%q operational=%q reason=%q runner=%q",
			ready.Header.Get("X-ArtistTrackarr-Database"), ready.Header.Get("X-ArtistTrackarr-Operational"),
			ready.Header.Get("X-ArtistTrackarr-Operational-Reason"), ready.Header.Get("X-ArtistTrackarr-Runner"))
	}

	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, server.URL+"/artists/follow", url.Values{
		"_csrf": {csrf}, "mbid": {followMBID},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Artist added") {
		t.Fatalf("single follow status/body=%d %q", response.StatusCode, body)
	}
	followed, err := database.FollowedArtists(ctx, user.ID)
	if err != nil || len(followed) != 1 {
		t.Fatalf("single follow list=%#v err=%v", followed, err)
	}
	artist := followed[0]

	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/sync", url.Values{
		"_csrf": {csrf},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Synchronization queued") {
		t.Fatalf("sync status/body=%d %q", response.StatusCode, body)
	}
	requests, err := database.ManualSyncRequests(ctx, 10)
	if err != nil || len(requests) != 1 || requests[0].ArtistID == nil || *requests[0].ArtistID != artist.ID {
		t.Fatalf("manual requests=%#v err=%v", requests, err)
	}

	bad := noRedirectClient(client)
	response, err = bad.PostForm(server.URL+"/artists/not-an-id/sync", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid sync status=%d", response.StatusCode)
	}

	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/delete", url.Values{
		"_csrf": {csrf},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Artist removed") {
		t.Fatalf("unfollow status/body=%d %q", response.StatusCode, body)
	}
	if following, err := database.IsFollowing(ctx, user.ID, artist.ID); err != nil || following {
		t.Fatalf("artist still followed=%v err=%v", following, err)
	}
	response, err = bad.PostForm(server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/delete", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("second unfollow status=%d", response.StatusCode)
	}

	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/logout", url.Values{"_csrf": {csrf}})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Sign in") {
		t.Fatalf("logout status/body=%d %q", response.StatusCode, body)
	}
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Sign in") {
		t.Fatalf("post-logout dashboard status/body=%d %q", response.StatusCode, body)
	}
}

func TestArtistNotificationRuleActionsAreOwnerScoped(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "rule-route-artist", Name: "Rule Route Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	otherArtist, err := database.UpsertArtist(ctx, store.Artist{MBID: "rule-route-artist-two", Name: "Rule Route Artist Two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, otherArtist.ID); err != nil {
		t.Fatal(err)
	}

	page, err := client.Get(server.URL + "/artists")
	if err != nil {
		t.Fatal(err)
	}
	pageBytes, readErr := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	body := string(pageBytes)
	if !strings.Contains(body, "Select all visible") || !strings.Contains(body, "Account defaults") {
		t.Fatalf("artists page omitted notification controls: %q", body)
	}
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/notification-rule", url.Values{
		"_csrf": {csrf}, "delivery_mode": {"digest"}, "include_primary": {"on"}, "albums": {"on"},
		"eps": {"on"}, "singles": {"on"}, "compilations": {"on"}, "announcements": {"on"}, "release_day": {"on"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rule update status=%d", response.StatusCode)
	}
	rule, err := database.FollowNotificationRule(ctx, user.ID, artist.ID)
	if err != nil || rule.DeliveryMode != store.FollowDeliveryDigest || rule.IncludeFeatured {
		t.Fatalf("updated rule=%#v err=%v", rule, err)
	}
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/notification-rules/batch", url.Values{
		"_csrf": {csrf}, "artist_ids": {strconv.FormatInt(artist.ID, 10), strconv.FormatInt(otherArtist.ID, 10)}, "delivery_mode": {"immediate"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("batch rule update status=%d", response.StatusCode)
	}
	for _, artistID := range []int64{artist.ID, otherArtist.ID} {
		rule, err = database.FollowNotificationRule(ctx, user.ID, artistID)
		if err != nil || rule.DeliveryMode != store.FollowDeliveryImmediate {
			t.Fatalf("batch rule artist=%d rule=%#v err=%v", artistID, rule, err)
		}
	}
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/notification-rule/pause", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pause status=%d", response.StatusCode)
	}
	rule, err = database.FollowNotificationRule(ctx, user.ID, artist.ID)
	if err != nil || rule.PausedUntil == nil {
		t.Fatalf("paused rule=%#v err=%v", rule, err)
	}
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/notification-rule/resume", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resume status=%d", response.StatusCode)
	}
	rule, err = database.FollowNotificationRule(ctx, user.ID, artist.ID)
	if err != nil || rule.PausedUntil != nil {
		t.Fatalf("resumed rule=%#v err=%v", rule, err)
	}
	bad := noRedirectClient(client)
	csrf = getCSRF(t, client, server.URL+"/artists")
	response, err = bad.PostForm(server.URL+"/artists/999999/notification-rule", url.Values{"_csrf": {csrf}, "delivery_mode": {"off"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign/missing rule status=%d", response.StatusCode)
	}
}

func TestITunesFollowBatchAndCancellation(t *testing.T) {
	const firstID = "101"
	const secondID = "202"
	itunes := &actionITunes{artists: map[string]catalog.ITunesArtist{
		firstID:  {ID: firstID, Name: "First iTunes", URL: "https://music.apple.com/us/artist/first/101"},
		secondID: {ID: secondID, Name: "Second iTunes", URL: "https://music.apple.com/us/artist/second/202"},
	}}
	database, server, client := authenticatedTestServerWithITunes(t, &searchCatalog{}, nil, itunes, nil)
	csrf := getCSRF(t, client, server.URL+"/artists/search")
	response := postForm(t, client, server.URL+"/artists/follow/itunes", url.Values{
		"_csrf": {csrf}, "itunes_id": {firstID},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("single iTunes follow status=%d", response.StatusCode)
	}

	response = postForm(t, client, server.URL+"/artists/follow/itunes/batch", url.Values{
		"_csrf": {csrf}, "itunes_ids": {firstID, firstID, secondID, "bad", "not-a-number"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "1 queued") ||
		!strings.Contains(string(body), "1 already queued") || !strings.Contains(string(body), "2 failed") {
		t.Fatalf("iTunes batch status/body=%d %q", response.StatusCode, body)
	}
	user, _ := database.UserByEmail(context.Background(), "member@example.com")
	resolutions, err := database.ArtistResolutions(context.Background(), user.ID)
	if err != nil || len(resolutions) != 2 {
		t.Fatalf("iTunes resolutions=%#v err=%v", resolutions, err)
	}

	resolutionID := resolutions[0].ID
	csrf = getCSRF(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolutionID, 10))
	response = postForm(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolutionID, 10)+"/cancel", url.Values{
		"_csrf": {csrf},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cancel iTunes resolution status=%d", response.StatusCode)
	}
	resolutions, err = database.ArtistResolutions(context.Background(), user.ID)
	if err != nil || len(resolutions) != 1 {
		t.Fatalf("cancelled iTunes resolutions=%#v err=%v", resolutions, err)
	}

	bad := noRedirectClient(client)
	response, err = bad.PostForm(server.URL+"/artists/follow/itunes", url.Values{"_csrf": {csrf}, "itunes_id": {"bad"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid iTunes follow status=%d", response.StatusCode)
	}
}

func TestAdminInvitationAndResetRoutes(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	admin, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin.ID); err != nil {
		t.Fatal(err)
	}

	csrf := getCSRF(t, client, server.URL+"/admin")
	response := postForm(t, client, server.URL+"/admin/invite", url.Values{
		"_csrf": {csrf}, "email": {"not-an-email"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid invite status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/admin/invite", url.Values{
		"_csrf": {csrf}, "email": {"member@example.com"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate invite status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/admin/reset", url.Values{
		"_csrf": {csrf}, "email": {"missing-user@example.com"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing reset user status=%d", response.StatusCode)
	}

	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "admin-queue-artist", Name: "Admin Queue Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, admin.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	response = postForm(t, client, server.URL+"/admin/sync/retry", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin retry queue status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/admin/retention/cleanup", url.Values{"_csrf": {csrf}})
	cleanupBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(cleanupBody), "Cleanup was not confirmed") {
		t.Fatalf("unconfirmed retention cleanup status/body=%d %q", response.StatusCode, cleanupBody)
	}
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/sync/artists/"+strconv.FormatInt(artist.ID, 10), url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin artist queue status=%d", response.StatusCode)
	}
	bad := noRedirectClient(client)
	response, err = bad.PostForm(server.URL+"/admin/sync/artists/not-an-id", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid admin artist queue status=%d", response.StatusCode)
	}
	response, err = bad.PostForm(server.URL+"/admin/sync/artists/999999", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing admin artist queue status=%d", response.StatusCode)
	}
	requests, err := database.ManualSyncRequests(ctx, 10)
	if err != nil || len(requests) != 2 {
		t.Fatalf("admin queued requests=%#v err=%v", requests, err)
	}

	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/invite", url.Values{
		"_csrf": {csrf}, "email": {"invited@example.com"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Invitation for invited@example.com") {
		t.Fatalf("invite status/body=%d %q", response.StatusCode, body)
	}
	tokenPattern := regexp.MustCompile(`/invite/([A-Za-z0-9_-]+)`).FindStringSubmatch(string(body))
	if len(tokenPattern) != 2 {
		t.Fatalf("invitation token missing: %q", body)
	}
	token := tokenPattern[1]
	csrf = getCSRF(t, client, server.URL+"/invite/"+token)
	response = postForm(t, client, server.URL+"/invite/"+token, url.Values{
		"_csrf": {csrf}, "username": {"invited-user"}, "password": {"a sufficiently long password"}, "timezone": {"UTC"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("accept invite status=%d", response.StatusCode)
	}
	invited, err := database.UserByEmail(ctx, "invited@example.com")
	if err != nil || invited.Username != "invited-user" {
		t.Fatalf("invited user=%#v err=%v", invited, err)
	}

	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/reset", url.Values{
		"_csrf": {csrf}, "email": {"invited@example.com"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Password reset for invited@example.com") {
		t.Fatalf("reset status/body=%d %q", response.StatusCode, body)
	}
	resetMatch := regexp.MustCompile(`/reset/([A-Za-z0-9_-]+)`).FindStringSubmatch(string(body))
	if len(resetMatch) != 2 {
		t.Fatalf("reset token missing: %q", body)
	}
	resetToken := resetMatch[1]
	csrf = getCSRF(t, client, server.URL+"/reset/"+resetToken)
	response = postForm(t, client, server.URL+"/reset/"+resetToken, url.Values{
		"_csrf": {csrf}, "password": {"a new sufficiently long password"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("accept reset status=%d", response.StatusCode)
	}
	updated, err := database.UserByEmail(ctx, "invited@example.com")
	if err != nil || !security.CheckPassword(updated.PasswordHash, "a new sufficiently long password") {
		t.Fatalf("reset password did not apply user=%#v err=%v", updated, err)
	}

	// Invalid usernames must leave an invitation usable. This guards the
	// transaction boundary around token consumption, not just the happy path.
	csrf = getCSRF(t, client, server.URL+"/admin")
	response = postForm(t, client, server.URL+"/admin/invite", url.Values{
		"_csrf": {csrf}, "email": {"retry-invite@example.com"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	inviteMatch := regexp.MustCompile(`/invite/([A-Za-z0-9_-]+)`).FindStringSubmatch(string(body))
	if len(inviteMatch) != 2 {
		t.Fatalf("retry invitation token missing: %q", body)
	}
	csrf = getCSRF(t, client, server.URL+"/invite/"+inviteMatch[1])
	response = postForm(t, client, server.URL+"/invite/"+inviteMatch[1], url.Values{
		"_csrf": {csrf}, "username": {"invited-user"}, "password": {"another sufficiently long password"}, "timezone": {"UTC"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Invitation is invalid") {
		t.Fatalf("duplicate username invite status/body=%d %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/invite/"+inviteMatch[1])
	response = postForm(t, client, server.URL+"/invite/"+inviteMatch[1], url.Values{
		"_csrf": {csrf}, "username": {"retry-user"}, "password": {"another sufficiently long password"}, "timezone": {"UTC"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retry invite status=%d", response.StatusCode)
	}
}

func TestAdminQueueActionsHandleCanceledStoreContext(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "admin-actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	userID, err := database.CreateUser(context.Background(), "admin-actions@example.com", "hash", "admin", "UTC", "admin-actions")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(context.Background(), store.Artist{MBID: "admin-actions-artist", Name: "Admin Actions Artist"})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	session := store.Session{User: store.User{ID: userID, Role: "admin"}}
	request := httptest.NewRequest(http.MethodPost, "/admin/sync/retry", nil)
	request = request.WithContext(context.WithValue(context.WithValue(canceled, sessionKey, session), csrfKey, "csrf"))
	response := httptest.NewRecorder()
	app.queueRetrySync(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "context+canceled") {
		t.Fatalf("canceled retry queue status=%d location=%q", response.Code, response.Header().Get("Location"))
	}

	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", strconv.FormatInt(artist.ID, 10))
	request = httptest.NewRequest(http.MethodPost, "/admin/sync/artists/"+strconv.FormatInt(artist.ID, 10), nil)
	request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeContext), csrfKey, "csrf"))
	response = httptest.NewRecorder()
	app.queueArtistSync(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled artist queue status=%d body=%q", response.Code, response.Body.String())
	}

	routeContext = chi.NewRouteContext()
	routeContext.URLParams.Add("id", "999")
	request = httptest.NewRequest(http.MethodPost, "/admin/users/999/delete", nil)
	request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeContext), csrfKey, "csrf"))
	response = httptest.NewRecorder()
	app.deleteUser(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled user delete status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestOwnerActionsHandleCanceledStoreContext(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "owner-actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	userID, err := database.CreateUser(context.Background(), "owner-actions@example.com", "hash", "member", "UTC", "owner-actions")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	session := store.Session{User: store.User{ID: userID, Role: "member", Username: "owner-actions"}}
	invoke := func(method, path, id string, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("id", id)
		request := httptest.NewRequest(method, path, nil)
		request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeContext), csrfKey, "csrf"))
		response := httptest.NewRecorder()
		handler(response, request)
		return response
	}
	checks := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{name: "unfollow", path: "/artists/1/delete", handler: app.unfollow},
		{name: "cancel resolution", path: "/artist-resolutions/1/cancel", handler: app.cancelArtistResolution},
		{name: "delete destination", path: "/destinations/1/delete", handler: app.deleteDestination},
		{name: "coverage sync", path: "/coverage/artists/1/sync", handler: app.queueCoverageSync},
		{name: "select resolution", path: "/artist-resolutions/1", handler: app.selectArtistResolution},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			response := invoke(http.MethodPost, check.path, "1", check.handler)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("canceled action status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
	// The release truth and notification hold actions should also turn a
	// canceled store operation into a generic server error, never a success or
	// a leaked database error.
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", "1")
	request := httptest.NewRequest(http.MethodPost, "/releases/1/truth?provider=spotify", nil)
	request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeCtx), csrfKey, "csrf"))
	response := httptest.NewRecorder()
	app.releaseTruthAction(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled release truth status=%d body=%q", response.Code, response.Body.String())
	}

	routeCtx = chi.NewRouteContext()
	routeCtx.URLParams.Add("id", "1")
	routeCtx.URLParams.Add("action", "notify")
	request = httptest.NewRequest(http.MethodPost, "/notifications/holds/1/notify", nil)
	request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeCtx), csrfKey, "csrf"))
	response = httptest.NewRecorder()
	app.notificationHoldAction(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled notification hold status=%d body=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/admin/profile?timezone=UTC&reminder_time=09:00", nil)
	request = request.WithContext(context.WithValue(context.WithValue(canceled, sessionKey, session), csrfKey, "csrf"))
	response = httptest.NewRecorder()
	app.profile(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "context+canceled") {
		t.Fatalf("canceled legacy profile status=%d location=%q", response.Code, response.Header().Get("Location"))
	}

	routeCtx = chi.NewRouteContext()
	routeCtx.URLParams.Add("id", "1")
	routeCtx.URLParams.Add("action", "read")
	request = httptest.NewRequest(http.MethodPost, "/inbox/1/read", nil)
	request = request.WithContext(context.WithValue(context.WithValue(context.WithValue(canceled, sessionKey, session), chi.RouteCtxKey, routeCtx), csrfKey, "csrf"))
	response = httptest.NewRecorder()
	app.inboxStateAction(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled inbox action status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestSetupPageRendersGenericErrorWhenStoreIsUnavailable(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "couldn&#39;t load this page") ||
		strings.Contains(string(body), "database closed") {
		t.Fatalf("setup store failure status/body=%d %q", response.StatusCode, body)
	}
}

func TestDestinationActionsAndPreferences(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("the encryption key has more than 32 bytes")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("ntfy://ntfy.sh/artisttrackarr")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(ctx, user.ID, "Test destination", "ntfy", encrypted); err != nil {
		t.Fatal(err)
	}

	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, server.URL+"/destinations/1/rename", url.Values{
		"_csrf": {csrf}, "name": {"Living room"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("destination rename status=%d", response.StatusCode)
	}
	destination, err := database.Destination(ctx, user.ID, 1)
	if err != nil || destination.Name != "Living room" {
		t.Fatalf("renamed destination=%#v err=%v", destination, err)
	}
	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/destinations/1/rename", url.Values{
		"_csrf": {csrf}, "name": {"   "},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid destination rename status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/destinations/1/test", url.Values{"_csrf": {csrf}})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Test sent") {
		t.Fatalf("destination test status/body=%d %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/destinations", url.Values{
		"_csrf": {csrf}, "name": {"Kitchen"}, "service": {"ntfy"}, "topic": {"kitchen"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Destination added") {
		t.Fatalf("destination add status/body=%d %q", response.StatusCode, body)
	}
	destinations, err := database.Destinations(ctx, user.ID)
	foundKitchen := false
	for _, destination := range destinations {
		if destination.Name == "Kitchen" {
			foundKitchen = true
			break
		}
	}
	if err != nil || len(destinations) != 2 || !foundKitchen {
		t.Fatalf("added destinations=%#v err=%v", destinations, err)
	}
	if err := database.AddDestination(ctx, user.ID, "Broken", "ntfy", []byte("not encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err = database.Destinations(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var brokenID int64
	for _, destination := range destinations {
		if destination.Name == "Broken" {
			brokenID = destination.ID
			break
		}
	}
	if brokenID == 0 {
		t.Fatal("broken destination was not added")
	}
	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, fmt.Sprintf("%s/destinations/%d/test", server.URL, brokenID), url.Values{"_csrf": {csrf}})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Test failed") {
		t.Fatalf("broken destination test status/body=%d %q", response.StatusCode, body)
	}
	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/destinations", url.Values{
		"_csrf": {csrf}, "name": {"Invalid"}, "service": {"unsupported"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unsupported destination service") {
		t.Fatalf("invalid destination add status/body=%d %q", response.StatusCode, body)
	}
	response = postForm(t, client, server.URL+"/preferences", url.Values{
		"_csrf": {csrf}, "albums": {"on"}, "eps": {"on"}, "singles": {"on"}, "announcements": {"on"}, "release_day": {"on"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("legacy preferences status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/destinations/1/delete", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("destination delete status=%d", response.StatusCode)
	}
	if _, err := database.Destination(ctx, user.ID, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted destination lookup err=%v", err)
	}
	bad := noRedirectClient(client)
	response, err = bad.PostForm(server.URL+"/destinations/1/delete", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("second destination delete status=%d", response.StatusCode)
	}

}

func TestArtistImportWithoutFileRendersArtistsPage(t *testing.T) {
	_, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, server.URL+"/artists/import", url.Values{"_csrf": {csrf}})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Select an ArtistTrackarr CSV file") ||
		!strings.Contains(string(body), "<h1>Artists</h1>") || !strings.Contains(string(body), "Export CSV") {
		t.Fatalf("import error status/body=%d %q", response.StatusCode, body)
	}
}

func TestArtistImportValidationAndSizeErrorsRenderArtistsPage(t *testing.T) {
	_, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postArtistImportCSV(t, client, server.URL+"/artists/import", csrf, "artist,display_name\ninvalid,row\n")
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "CSV is missing required column") ||
		!strings.Contains(string(body), "<h1>Artists</h1>") || !strings.Contains(string(body), "Export CSV") {
		t.Fatalf("invalid CSV status/body=%d %q", response.StatusCode, body)
	}

	oversized := strings.Repeat("x", maxArtistImportBytes+1)
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postArtistImportCSV(t, client, server.URL+"/artists/import", csrf, oversized)
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "1 MiB or smaller") ||
		!strings.Contains(string(body), "<h1>Artists</h1>") {
		t.Fatalf("oversized CSV status/body=%d %q", response.StatusCode, body)
	}
}

func TestWebValidationAndOwnerScopedNotFoundPaths(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()

	// Settings validation should preserve the page and return a useful client
	// error instead of redirecting a malformed profile update.
	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, server.URL+"/settings/profile", url.Values{
		"_csrf": {csrf}, "username": {"x"}, "timezone": {"UTC"}, "reminder_time": {"09:00"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "3–32") {
		t.Fatalf("invalid settings username status/body=%d %q", response.StatusCode, body)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/profile", url.Values{
		"_csrf": {csrf}, "username": {"valid-user"}, "timezone": {"Not/AZone"}, "reminder_time": {"09:00"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid IANA timezone") {
		t.Fatalf("invalid settings timezone status/body=%d %q", response.StatusCode, body)
	}

	// Reset links must fail safely for unknown tokens and retain the token form
	// rather than producing an empty response or a successful redirect.
	csrf = getCSRF(t, client, server.URL+"/reset/unknown-token")
	noRedirect := noRedirectClient(client)
	response, err := noRedirect.PostForm(server.URL+"/reset/unknown-token", url.Values{
		"_csrf": {csrf}, "password": {"a sufficiently long password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Reset link is invalid") {
		t.Fatalf("invalid reset status/body=%d %q", response.StatusCode, body)
	}

	// Import results are owner scoped and reject malformed or foreign IDs.
	for _, id := range []string{"not-an-id", "999999"} {
		response, err = noRedirect.Get(server.URL + "/artists/imports/" + id)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("invalid import id %q status=%d", id, response.StatusCode)
		}
	}
	other, err := database.CreateUser(ctx, "import-owner@example.com", "hash", "member", "UTC", "import-owner")
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.CreateImportJob(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	response, err = noRedirect.Get(server.URL + "/artists/imports/" + strconv.FormatInt(job.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign import status=%d", response.StatusCode)
	}

	// A pending resolution cannot be completed by submitting an MBID that was
	// not part of its review snapshot.
	member, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	resolution, created, err := database.CreateArtistResolution(ctx, member.ID, "spotify", "resolution-web", "Resolution Web", "https://open.spotify.com/artist/resolution-web", "")
	if err != nil || !created {
		t.Fatalf("create resolution=%#v created=%v err=%v", resolution, created, err)
	}
	csrf = getCSRF(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10))
	response = postForm(t, client, server.URL+"/artist-resolutions/"+strconv.FormatInt(resolution.ID, 10), url.Values{
		"_csrf": {csrf}, "mbid": {"not-in-review"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "select one of the reviewed") {
		t.Fatalf("invalid resolution selection status/body=%d %q", response.StatusCode, body)
	}

	// Syncing an artist that is not followed is not allowed even when the
	// canonical artist record still exists.
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "unfollowed-sync", Name: "Unfollowed Sync"})
	if err != nil {
		t.Fatal(err)
	}
	csrf = getCSRF(t, client, server.URL+"/artists")
	response, err = noRedirect.PostForm(server.URL+"/artists/"+strconv.FormatInt(artist.ID, 10)+"/sync", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unfollowed sync status=%d", response.StatusCode)
	}
}

func TestProviderActionFailuresAndReadinessErrors(t *testing.T) {
	// A canonical lookup failure is surfaced as a client error and does not
	// create a partial artist or follow.
	_, server, client := authenticatedTestServer(t, &searchCatalog{resolveError: map[string]error{
		"failed-mbid": errors.New("MusicBrainz unavailable"),
	}}, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/artists")
	response := postForm(t, client, server.URL+"/artists/follow", url.Values{
		"_csrf": {csrf}, "mbid": {"failed-mbid"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("failed MusicBrainz follow status=%d", response.StatusCode)
	}

	// Provider-specific routes reject unavailable providers and failed
	// verification before anything is queued.
	csrf = getCSRF(t, client, server.URL+"/artists")
	response = postForm(t, client, server.URL+"/artists/follow/spotify", url.Values{
		"_csrf": {csrf}, "spotify_id": {"4NHQUGzhtTLFvgF5SZesLK"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfigured Spotify follow status=%d", response.StatusCode)
	}

	itunes := &actionITunes{artists: map[string]catalog.ITunesArtist{}}
	_, itunesServer, itunesClient := authenticatedTestServerWithITunes(t, &searchCatalog{}, nil, itunes, nil)
	csrf = getCSRF(t, itunesClient, itunesServer.URL+"/artists")
	response = postForm(t, itunesClient, itunesServer.URL+"/artists/follow/itunes", url.Values{
		"_csrf": {csrf}, "itunes_id": {"999"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed iTunes verification status=%d", response.StatusCode)
	}

	// Closing both handles exercises the unauthenticated readiness and
	// provider-health failure responses without exposing database details.
	database, healthServer, healthClient := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	healthUser, err := database.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, healthUser.ID); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	response, err = healthClient.Get(healthServer.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy readiness status=%d", response.StatusCode)
	}
	response, err = healthClient.Get(healthServer.URL + "/admin/provider-health")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unhealthy provider health status=%d", response.StatusCode)
	}
}

func TestNotificationHoldActionsAreOwnerScoped(t *testing.T) {
	database, server, client := authenticatedTestServer(t, &searchCatalog{}, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "web-hold-artist", Name: "Web Hold Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	releaseResult, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "web-hold-release", artist.ID, "Web Held Release", "Album", "[]", "2026-09-01", 3,
		"https://musicbrainz.org/release-group/web-hold-release", "both", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := releaseResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,'open',?,?)`, releaseID, "date_conflict", "warning", "web-hold-fingerprint", "Review this conflict",
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	prefs, err := database.NotificationPreferences(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	prefs.HoldConflictingNotifications = true
	if err := database.UpdateNotificationPreferences(ctx, prefs); err != nil {
		t.Fatal(err)
	}
	if err := database.EnqueueEvent(ctx, user.ID, releaseID, "announcement", "Held release", "Review this release", now); err != nil {
		t.Fatal(err)
	}
	holds, err := database.NotificationHolds(ctx, user.ID, 10)
	if err != nil || len(holds) != 1 {
		t.Fatalf("web holds=%#v err=%v", holds, err)
	}
	holdID := holds[0].ID
	csrf := getCSRF(t, client, server.URL+"/inbox")
	bad := noRedirectClient(client)
	response := postForm(t, bad, server.URL+"/notifications/holds/not-an-id/notify", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid hold ID status=%d", response.StatusCode)
	}
	response = postForm(t, client, server.URL+"/notifications/holds/"+strconv.FormatInt(holdID, 10)+"/invalid", url.Values{"_csrf": {csrf}})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid hold action status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/notifications/holds/"+strconv.FormatInt(holdID, 10)+"/notify", url.Values{
		"_csrf": {csrf}, "return": {"/inbox"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Inbox") {
		t.Fatalf("notify hold status/body=%d %q", response.StatusCode, body)
	}
	var status string
	if err := database.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, holdID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "released" {
		t.Fatalf("hold status=%q", status)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,issue_fingerprint,planned_at,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, user.ID, releaseID, "release_day", "Discarded hold", "body", "review", "discard-fingerprint",
		now.Format(time.RFC3339Nano), "held", now.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var discardID int64
	if err := database.DB.QueryRowContext(ctx, `SELECT id FROM notification_holds WHERE issue_fingerprint=?`, "discard-fingerprint").Scan(&discardID); err != nil {
		t.Fatal(err)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/notifications/holds/"+strconv.FormatInt(discardID, 10)+"/discard", url.Values{
		"_csrf": {csrf}, "return": {"/inbox?state=held"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("discard hold status=%d", response.StatusCode)
	}
	if err := database.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, discardID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "discarded" {
		t.Fatalf("discarded hold status=%q", status)
	}
	csrf = getCSRF(t, client, server.URL+"/inbox")
	response = postForm(t, client, server.URL+"/notifications/holds/"+strconv.FormatInt(discardID, 10)+"/restore", url.Values{
		"_csrf": {csrf}, "return": {"/inbox?state=held"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restore hold status=%d", response.StatusCode)
	}
	if err := database.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, discardID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "held" {
		t.Fatalf("restored hold status=%q, want held while evidence remains open", status)
	}
}
