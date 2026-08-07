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
