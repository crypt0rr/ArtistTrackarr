package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/logging"
)

func TestManualSyncAdmissionIsAtomicAndBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "manual-admission@example.com", "hash", "member", "UTC", "manual-admission")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "manual-admission-artist", Name: "Manual Admission Artist"})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 16
	requests := make(chan ManualSyncRequest, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request, requestErr := s.CreateManualSyncRequest(ctx, userID, "artist", &artist.ID)
			requests <- request
			errs <- requestErr
		}()
	}
	wait.Wait()
	close(requests)
	close(errs)
	var firstID int64
	for request := range requests {
		if firstID == 0 {
			firstID = request.ID
		}
		if request.ID != firstID {
			t.Fatalf("concurrent duplicate request IDs=%d and %d", firstID, request.ID)
		}
	}
	for requestErr := range errs {
		if requestErr != nil {
			t.Fatalf("concurrent request error=%v", requestErr)
		}
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM manual_sync_requests WHERE scope='artist' AND artist_id=? AND status='queued'`, artist.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queued duplicate count=%d, want 1", count)
	}

	for i := 0; i < maxQueuedManualSyncRequests-1; i++ {
		queuedArtist, upsertErr := s.UpsertArtist(ctx, Artist{MBID: "manual-admission-" + strconv.Itoa(i), Name: "Queued Artist"})
		if upsertErr != nil {
			t.Fatal(upsertErr)
		}
		if _, requestErr := s.CreateManualSyncRequest(ctx, userID, "artist", &queuedArtist.ID); requestErr != nil {
			t.Fatalf("fill queue at %d: %v", i, requestErr)
		}
	}

	lastArtist, err := s.UpsertArtist(ctx, Artist{MBID: "manual-admission-last", Name: "Last Queued Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManualSyncRequest(ctx, userID, "artist", &lastArtist.ID); !errors.Is(err, ErrManualSyncQueueFull) {
		t.Fatalf("full queue error=%v, want %v", err, ErrManualSyncQueueFull)
	}
}

func TestOperationalLogsAndManualSyncLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "operations@example.com", "hash", "member", "UTC", "operations")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.July, 1, 10, 0, 0, 0, time.UTC)
	for _, entry := range []logging.Entry{
		{Time: base, Level: "INFO", Message: "sync started", Attributes: []logging.Field{{Key: "count", Value: "2"}}},
		{Time: base.Add(time.Hour), Level: "WARNING", Message: "sync warning"},
		{Time: base.Add(2 * time.Hour), Level: "DEBUG", Message: "not persisted"},
	} {
		if err := s.InsertApplicationLog(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.ApplicationLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Message != "sync warning" || logs[0].Level != "WARN" || logs[1].Message != "sync started" {
		t.Fatalf("unexpected application logs: %#v", logs)
	}
	if len(logs[1].Attributes) != 1 || logs[1].Attributes[0].Key != "count" {
		t.Fatalf("log attributes were not persisted: %#v", logs[1].Attributes)
	}
	if err := s.PruneApplicationLogs(ctx, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	logs, err = s.ApplicationLogs(ctx, 10)
	if err != nil || len(logs) != 1 || logs[0].Message != "sync warning" {
		t.Fatalf("pruned logs = %#v, err=%v", logs, err)
	}

	artist, err := s.UpsertArtist(ctx, Artist{MBID: "operations-artist", Name: "Operations Artist"})
	if err != nil {
		t.Fatal(err)
	}
	artistRequest, err := s.CreateManualSyncRequest(ctx, userID, "artist", &artist.ID)
	if err != nil {
		t.Fatalf("create artist request: request=%#v err=%v", artistRequest, err)
	}
	duplicate, err := s.CreateManualSyncRequest(ctx, userID, "artist", &artist.ID)
	if err != nil || duplicate.ID != artistRequest.ID {
		t.Fatalf("duplicate artist request: request=%#v err=%v", duplicate, err)
	}
	retryRequest, err := s.CreateManualSyncRequest(ctx, userID, "retry", nil)
	if err != nil {
		t.Fatalf("create retry request: request=%#v err=%v", retryRequest, err)
	}
	if _, err := s.CreateManualSyncRequest(ctx, userID, "unknown", nil); err == nil {
		t.Fatal("invalid sync scope was accepted")
	}
	if _, err := s.CreateManualSyncRequest(ctx, userID, "artist", nil); err == nil {
		t.Fatal("artist request without an artist was accepted")
	}

	claimed, err := s.ClaimManualSyncRequests(ctx, 0)
	if err != nil || len(claimed) != 1 || claimed[0].Status != "running" || claimed[0].StartedAt == nil {
		t.Fatalf("claim with minimum limit: %#v, err=%v", claimed, err)
	}
	if err := s.CompleteManualSyncRequest(ctx, claimed[0].ID, errors.New(strings.Repeat("x", 600))); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimManualSyncRequests(ctx, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != retryRequest.ID {
		t.Fatalf("claim remaining request: %#v, err=%v", claimed, err)
	}
	if err := s.CompleteManualSyncRequest(ctx, claimed[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	// A queued row has NULL started/finished timestamps. This exercises the
	// nullable scan path that is also used for rows created by older versions.
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO manual_sync_requests(requested_by,scope,status,created_at) VALUES(?,?,?,?)`, userID, "retry", "queued", nowText()); err != nil {
		t.Fatal(err)
	}
	requests, err := s.ManualSyncRequests(ctx, 20)
	if err != nil || len(requests) != 3 {
		t.Fatalf("manual sync requests: %#v, err=%v", requests, err)
	}
	for _, request := range requests {
		if request.ID == artistRequest.ID && (request.FinishedAt == nil || len(request.LastError) != 500) {
			t.Fatalf("failed request was not completed/truncated: %#v", request)
		}
		if request.ID == retryRequest.ID && request.FinishedAt == nil {
			t.Fatalf("successful request was not completed: %#v", request)
		}
	}
}

func TestOperationalScannersRejectCorruptPersistedState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`,
		"not-a-time", "INFO", "corrupt", "[]"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplicationLogs(ctx, 10); err == nil {
		t.Fatal("ApplicationLogs accepted an invalid persisted timestamp")
	}

	if _, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,updated_at) VALUES(?,?)`, "corrupt", "not-a-time"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProviderHealth(ctx); err == nil {
		t.Fatal("ProviderHealth accepted an invalid persisted timestamp")
	}
}

func TestProviderHealthAndAdminArtistQueries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "health@example.com", "hash", "member", "UTC", "health")
	if err != nil {
		t.Fatal(err)
	}
	followed, err := s.UpsertArtist(ctx, Artist{MBID: "health-followed", Name: "Followed", SortName: "Followed", Type: "Group", Country: "NL"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertArtist(ctx, Artist{MBID: "health-unfollowed", Name: "Unfollowed"}); err != nil {
		t.Fatal(err)
	}
	if added, err := s.Follow(ctx, userID, followed.ID); err != nil || !added {
		t.Fatalf("follow artist: added=%v err=%v", added, err)
	}
	if err := s.MarkAllArtistsDue(ctx); err != nil {
		t.Fatal(err)
	}
	var next string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_check_at FROM artists WHERE id=?`, followed.ID).Scan(&next); err != nil || next == "" {
		t.Fatalf("artist was not marked due: next=%q err=%v", next, err)
	}
	got, err := s.ArtistByID(ctx, followed.ID)
	if err != nil || got.ID != followed.ID || got.Country != "NL" || got.Type != "Group" {
		t.Fatalf("artist by ID: %#v, err=%v", got, err)
	}
	if _, err := s.ArtistByID(ctx, 999999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing artist error = %v, want sql.ErrNoRows", err)
	}
	adminArtists, err := s.AdminArtists(ctx)
	if err != nil || len(adminArtists) != 1 || adminArtists[0].ID != followed.ID {
		t.Fatalf("admin artists: %#v, err=%v", adminArtists, err)
	}

	nextCheck := time.Now().UTC().Add(time.Hour)
	longError := strings.Repeat("e", 700)
	if err := s.UpsertProviderHealth(ctx, "spotify", false, &nextCheck, true, true, longError); err != nil {
		t.Fatal(err)
	}
	health, err := s.ProviderHealthByName(ctx, "spotify")
	if err != nil || !health.RateLimited || !health.QuotaExceeded || health.NextCheckAt == nil || len(health.LastError) != 500 {
		t.Fatalf("provider health: %#v, err=%v", health, err)
	}
	allHealth, err := s.ProviderHealth(ctx)
	if err != nil || len(allHealth) != 1 || allHealth[0].Provider != "spotify" {
		t.Fatalf("provider health list: %#v, err=%v", allHealth, err)
	}
	if err := s.UpsertProviderHealth(ctx, "spotify", true, nil, false, false, "ignored"); err != nil {
		t.Fatal(err)
	}
	health, err = s.ProviderHealthByName(ctx, "spotify")
	if err != nil || health.RateLimited || health.QuotaExceeded || health.LastError != "" || health.NextCheckAt != nil || health.LastSuccessAt == nil {
		t.Fatalf("successful provider health reset: %#v, err=%v", health, err)
	}
}
