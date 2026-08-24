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

func TestReleaseTruthDecisionPageAndAction(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "web-truth-artist", Name: "Web Truth Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_id,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "web-truth-release", artist.ID, "Web Truth Release", "Album", "[]",
		"2026-08-08", 3, "https://musicbrainz.org/release-group/web-truth-release", "web-truth-spotify",
		"https://open.spotify.com/album/web-truth-release", "spotify", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('spotify',? ,?,'hash',?)`,
		"web-truth-spotify", releaseID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/releases/" + fmt.Sprint(releaseID))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(page, "Release truth") ||
		!strings.Contains(page, "Confirm Spotify") {
		t.Fatalf("release page status/body=%d %q", response.StatusCode, page)
	}

	csrf := getCSRF(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID))
	response = postForm(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"confirm"}, "provider": {"spotify"}, "reason": {"Spotify has the current listing"},
	})
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("truth action status=%d body=%q", response.StatusCode, body)
	}
	detail, err := database.ReleaseDetail(ctx, user.ID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TruthState != "confirmed" || detail.TruthProvider != "spotify" || detail.TruthReason != "Spotify has the current listing" {
		t.Fatalf("truth decision=%+v", detail.Release)
	}

	// A valid provider without a persisted observation is rejected without
	// changing the existing decision.
	csrf = getCSRF(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID))
	response = postForm(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"confirm"}, "provider": {"itunes"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "provider is not available") {
		t.Fatalf("unavailable truth provider status/body=%d %q", response.StatusCode, body)
	}

	csrf = getCSRF(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID))
	response = postForm(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"clear"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear truth status=%d", response.StatusCode)
	}
	csrf = getCSRF(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID))
	response = postForm(t, client, server.URL+"/releases/"+fmt.Sprint(releaseID)+"/truth", url.Values{
		"_csrf": {csrf}, "action": {"clear"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("clearing absent truth decision status=%d", response.StatusCode)
	}
}

// TestTruthConfirmFormOffersAReasonField pins the reason down to what a member
// can actually submit. The store validates a reason, the handler reads one and
// the release page renders it back - but for several releases no template
// carried an input for it, so every stored reason was empty and the "Their
// reason:" line never had anything to show. The existing action test missed
// this because it posts the field directly, synthesising a request the UI had
// no way to produce. This one asserts the rendered form.
func TestTruthConfirmFormOffersAReasonField(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "reason-form-artist", Name: "Reason Form Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, user.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_id,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "reason-form-release", artist.ID, "Reason Form Release", "Album", "[]",
		"2026-08-08", 3, "https://musicbrainz.org/release-group/reason-form-release", "reason-form-spotify",
		"https://open.spotify.com/album/reason-form-release", "spotify", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('spotify',? ,?,'hash',?)`,
		"reason-form-spotify", releaseID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/releases/" + fmt.Sprint(releaseID))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(raw)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("release page status=%d body=%q", response.StatusCode, page)
	}

	form, ok := truthConfirmForm(page)
	if !ok {
		t.Fatalf("no truth confirm form on the release page: %q", page)
	}
	if !strings.Contains(form, `name="reason"`) {
		t.Fatalf("truth confirm form carries no reason input, so no member can ever set one: %q", form)
	}
	// The reason has to travel with the provider. A reason input in a form that
	// does not also submit the provider would post a confirm with no target.
	if !strings.Contains(form, `name="provider"`) {
		t.Fatalf("reason input is not in the same form as the provider choice: %q", form)
	}
}

// truthConfirmForm returns the release page's truth form that submits a
// provider choice, so assertions can be scoped to it rather than to the whole
// page (the "Clear decision" form also posts to the same endpoint).
func truthConfirmForm(page string) (string, bool) {
	rest := page
	for {
		start := strings.Index(rest, "<form")
		if start < 0 {
			return "", false
		}
		rest = rest[start:]
		end := strings.Index(rest, "</form>")
		if end < 0 {
			return "", false
		}
		form := rest[:end]
		if strings.Contains(form, "/truth") && strings.Contains(form, `value="confirm"`) {
			return form, true
		}
		rest = rest[end+len("</form>"):]
	}
}

// TestAdminCanOverrideAnotherMembersTruthDecision pins the confirm form to the
// store's own authorisation rule. releaseTruthDecisionWritableTx explicitly
// permits an administrator to change a decision another member made, and the
// "Clear decision" form has always carried that exception - but the confirm form
// introduced with the reason field guarded on TruthDecidedByAnotherMember alone,
// which is role-independent. The page told members "Ask them or an administrator
// to change it" while giving that administrator no control to change it with.
func TestAdminCanOverrideAnotherMembersTruthDecision(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	admin, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Promote the signed-in member, the pattern the other admin tests use.
	if _, err := database.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin.ID); err != nil {
		t.Fatal(err)
	}
	otherID, err := database.CreateUser(ctx, "other-decider@example.com", "hash", "member", "UTC", "other-decider")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "admin-truth-artist", Name: "Admin Truth Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Follow(ctx, admin.ID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_id,spotify_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "admin-truth-release", artist.ID, "Admin Truth Release", "Album", "[]",
		"2026-08-08", 3, "https://musicbrainz.org/release-group/admin-truth-release", "admin-truth-spotify",
		"https://open.spotify.com/album/admin-truth-release", "spotify",
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('spotify',?,?,'hash',?)`,
		"admin-truth-spotify", releaseID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// The other member must own the release too for their decision to be valid.
	if _, err := database.Follow(ctx, otherID, artist.ID); err != nil {
		t.Fatal(err)
	}
	// Another member decides first.
	if err := database.SetReleaseTruthDecision(ctx, otherID, releaseID, "spotify", "their reason"); err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/releases/" + fmt.Sprint(releaseID))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	page := string(raw)

	form, ok := truthConfirmForm(page)
	if !ok {
		t.Fatalf("an administrator has no confirm control over another member's decision: %s", page)
	}
	if !strings.Contains(form, `name="reason"`) {
		t.Fatalf("the administrator's confirm form carries no reason input: %q", form)
	}
	// The other member's words must not be pre-filled into the admin's box.
	if strings.Contains(form, "their reason") {
		t.Fatalf("another member's reason was prefilled for the administrator: %q", form)
	}
}
