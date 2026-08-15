package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestAdminDiagnosticsJSONAndRetentionExport(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/admin/diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member diagnostics JSON status=%d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if _, err := database.DB.ExecContext(ctx, `UPDATE users SET role='admin' WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	artist, err := database.UpsertArtist(ctx, store.Artist{MBID: "admin-export-artist", Name: "Admin Export Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := database.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "admin-export-release", artist.ID, "=SUM(1,1)", "Album", "[]", "2026-08-15", 3, "https://musicbrainz.org/release-group/admin-export-release", "musicbrainz", now, now)
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, user.ID, releaseID, "announcement", "=SUM(1,1)", "+notification", now); err != nil {
		t.Fatal(err)
	}

	response, err = client.Get(server.URL + "/admin/diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("diagnostics JSON headers/status=%d content-type=%q cache=%q", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"))
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] != "dev" || payload["operational_status"] == nil || payload["database"] == nil || payload["retention"] == nil || payload["runner"] == nil {
		t.Fatalf("unexpected diagnostics payload=%#v", payload)
	}
	jsonBody, _ := json.Marshal(payload)
	if strings.Contains(string(jsonBody), "example.test") || strings.Contains(string(jsonBody), "secret-token") || strings.Contains(string(jsonBody), "=SUM") {
		t.Fatalf("diagnostics JSON leaked sensitive content: %s", jsonBody)
	}

	response, err = client.Get(server.URL + "/admin/retention/export")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(response.Header.Get("Content-Disposition"), "artisttrackarr-delivery-audit.csv") {
		t.Fatalf("retention export headers/status=%d content-type=%q disposition=%q", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Content-Disposition"))
	}
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0] != "delivery_id" || rows[1][2] != "'=SUM(1,1)" || rows[1][3] != "'+notification" {
		t.Fatalf("unexpected retention export rows=%#v", rows)
	}
}
