package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreOpenRejectsMissingParentAndCloseHandlesNilHandles(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "artisttrackarr.db")); err == nil {
		t.Fatal("Open accepted a database path whose parent does not exist")
	}
	if err := (&Store{}).Close(); err != nil {
		t.Fatalf("nil store close error=%v", err)
	}
}

func TestStoreOpenEscapesSQLiteURIPathCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artist ? # ü.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open path with URI-significant characters: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at the requested path: %v", err)
	}

	dsn := sqliteDSN("relative/artist ? # ü.db", false)
	for _, escaped := range []string{"%3F", "%23", "%20", "%C3%BC"} {
		if !strings.Contains(dsn, escaped) {
			t.Fatalf("sqlite DSN %q does not escape %s", dsn, escaped)
		}
	}
	if strings.Contains(dsn, "? #") || strings.Contains(dsn, "# ü") {
		t.Fatalf("sqlite DSN left path delimiters unescaped: %q", dsn)
	}
}

func TestStoreCloseIsIdempotentAcrossConcurrentCallers(t *testing.T) {
	s := testStore(t)
	const callers = 8
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- s.Close()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent close error=%v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close error=%v", err)
	}
}

func TestArtistProviderStatusValidationAndRetention(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "provider-status-edge", Name: "Provider Status Edge"})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []ArtistProviderStatus{
		{ArtistID: 0, Provider: "spotify"},
		{ArtistID: artist.ID, Provider: "unknown"},
	} {
		if err := s.RecordArtistProviderStatus(ctx, invalid); err == nil {
			t.Fatalf("invalid provider status accepted: %#v", invalid)
		}
	}
	longError := strings.Repeat("x", 600)
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "SPOTIFY", ReleaseCount: 4, LastError: longError,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "spotify", ReleaseCount: -1, LastError: "updated",
	}); err != nil {
		t.Fatal(err)
	}
	var provider, status, lastError string
	var releaseCount int
	if err := s.DB.QueryRowContext(ctx, `SELECT provider,status,release_count,last_error FROM artist_provider_status WHERE artist_id=?`, artist.ID).
		Scan(&provider, &status, &releaseCount, &lastError); err != nil {
		t.Fatal(err)
	}
	if provider != "spotify" || status != "pending" || releaseCount != 4 || lastError != "updated" {
		t.Fatalf("retained provider status provider=%q status=%q release_count=%d error=%q", provider, status, releaseCount, lastError)
	}
	if len(lastError) > 500 {
		t.Fatalf("provider error was not bounded: %d", len(lastError))
	}
	if coverage, err := s.FollowedArtistCoveragePage(ctx, 1, 1000, -10); err != nil || len(coverage) != 0 {
		t.Fatalf("empty bounded coverage=%#v err=%v", coverage, err)
	}
}

func TestStandbyProviderStatusPreservesFailureEvidence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "provider-standby", Name: "Provider Standby"})
	if err != nil {
		t.Fatal(err)
	}
	failureAt := time.Date(2026, time.August, 18, 10, 0, 0, 0, time.UTC)
	nextCheck := failureAt.Add(time.Hour)
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "failed",
		LastAttemptAt: &failureAt, LastFailureAt: &failureAt, NextCheckAt: &nextCheck,
		ReleaseCount: 7, LastError: "provider unavailable", UpdatedAt: failureAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "standby", UpdatedAt: failureAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var status, lastFailure, lastError, storedNext string
	var releaseCount int
	if err := s.DB.QueryRowContext(ctx, `SELECT status,last_failure_at,last_error,next_check_at
		FROM artist_provider_status WHERE artist_id=? AND provider='musicbrainz'`, artist.ID).
		Scan(&status, &lastFailure, &lastError, &storedNext); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT release_count FROM artist_provider_status
		WHERE artist_id=? AND provider='musicbrainz'`, artist.ID).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if status != "standby" || lastFailure == "" || lastError != "provider unavailable" || storedNext == "" || releaseCount != 7 {
		t.Fatalf("standby state lost failure evidence status=%q failure=%q error=%q next=%q releases=%d", status, lastFailure, lastError, storedNext, releaseCount)
	}
}

func TestStoreHealthArtworkRetryAndScopedEmptyQueries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "edge-paths@example.com", "hash", "member", "UTC", "edge-paths")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "edge-paths-artist", Name: "Edge Paths Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "itunes",
		Releases: []Release{{
			MBID: "itunes:edge-paths-release", ITunesID: "edge-paths-release", Title: "Edge Release",
			PrimaryType: "Album", FirstReleaseDate: "2026-08-06", DatePrecision: 3,
		}},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var releaseID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE itunes_id=?`, "edge-paths-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(15 * time.Minute)
	if err := s.ScheduleITunesArtworkRetry(ctx, artist.ID, next); err != nil {
		t.Fatal(err)
	}
	var nextText string
	var attempts int
	if err := s.DB.QueryRowContext(ctx, `SELECT itunes_artwork_next_check_at,itunes_artwork_attempts FROM release_groups WHERE id=?`, releaseID).Scan(&nextText, &attempts); err != nil {
		t.Fatal(err)
	}
	if nextText == "" || attempts != 1 {
		t.Fatalf("artwork retry next=%q attempts=%d", nextText, attempts)
	}
	if err := s.Healthy(ctx); err != nil {
		t.Fatalf("store health=%v", err)
	}
	if issues, err := s.EvidenceIssuesForRelease(ctx, userID, releaseID, time.Now().UTC()); err != nil || len(issues) != 0 {
		t.Fatalf("empty evidence issues=%#v err=%v", issues, err)
	}
	if holds, err := s.NotificationHoldsForRelease(ctx, userID, releaseID); err != nil || len(holds) != 0 {
		t.Fatalf("empty notification holds=%#v err=%v", holds, err)
	}
	if _, err := s.EvidenceIssuesForRelease(ctx, userID+1, releaseID, time.Now().UTC()); err != nil {
		t.Fatalf("owner-scoped empty evidence query=%v", err)
	}
}

func TestDigestDeliveryFailureUpdatesRunAndRedacts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "digest-edge@example.com", "hash", "member", "UTC", "digest-edge")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Digest destination", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	now := time.Now().UTC()
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs
		(user_id,frequency,period_start,title,body,release_count,status,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, "daily", "2026-08-06", "Daily digest", "Digest body", 1, "pending", nowText())
	if err != nil {
		t.Fatal(err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries
		(run_id,destination_id,status,attempts,next_attempt_at,last_error)
		VALUES(?,?,?,?,?,?)`, runID, destinations[0].ID, "pending", 0, nowText(), "")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDigestDeliveryFailed(ctx, deliveryID, 5, "POST https://example.test/hook?token=secret failed", now); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus, lastError, runStatus string
	if err := s.DB.QueryRowContext(ctx, `SELECT status,last_error FROM release_digest_deliveries WHERE id=?`, deliveryID).Scan(&deliveryStatus, &lastError); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "failed" || runStatus != "failed" || strings.Contains(lastError, "example.test") || strings.Contains(lastError, "secret") {
		t.Fatalf("digest failure delivery=%q run=%q error=%q", deliveryStatus, runStatus, lastError)
	}
	if err := s.MarkDigestDeliveryFailed(ctx, deliveryID, 1, "temporary failure", now); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE id=?`, deliveryID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "pending" {
		t.Fatalf("retry digest delivery status=%q", deliveryStatus)
	}
	if _, err := s.AdminDeliveryDetail(ctx, deliveryID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("digest delivery appeared in notification detail=%v", err)
	}
}

func TestCoverageStatusMatrix(t *testing.T) {
	tests := []struct {
		name string
		item ArtistCoverage
		want string
	}{
		{name: "pending without providers", item: ArtistCoverage{}, want: "pending"},
		{name: "fresh release without providers", item: ArtistCoverage{ReleaseCount: 1}, want: "fresh"},
		{name: "fallback without providers", item: ArtistCoverage{FallbackReleases: 1}, want: "fallback"},
		{name: "confirmed without providers", item: ArtistCoverage{ConfirmedReleases: 1}, want: "confirmed"},
		{name: "provider failure", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "spotify", Status: "failed"}}}, want: "attention"},
		{name: "provider cooldown", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "itunes", Status: "cooldown"}}}, want: "attention"},
		{name: "confirmed provider", item: ArtistCoverage{ConfirmedReleases: 1, ProviderStatuses: []ArtistProviderStatus{{Provider: "musicbrainz", Status: "healthy"}}}, want: "confirmed"},
		{name: "fallback provider", item: ArtistCoverage{FallbackReleases: 1, ProviderStatuses: []ArtistProviderStatus{{Provider: "itunes", Status: "healthy"}}}, want: "fallback"},
		{name: "healthy provider", item: ArtistCoverage{ProviderStatuses: []ArtistProviderStatus{{Provider: "spotify", Status: "healthy"}}}, want: "fresh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := coverageStatus(test.item); got != test.want {
				t.Fatalf("coverageStatus(%#v)=%q, want %q", test.item, got, test.want)
			}
		})
	}
}

func TestEvidencePrecisionAndDateValidation(t *testing.T) {
	for _, test := range []struct {
		item ReleaseEvidence
		want int
	}{
		{item: ReleaseEvidence{DatePrecision: 1, FirstReleaseDate: "2026-01-01"}, want: 1},
		{item: ReleaseEvidence{DatePrecision: 0, FirstReleaseDate: "2026"}, want: 1},
		{item: ReleaseEvidence{DatePrecision: 0, FirstReleaseDate: "2026-01"}, want: 2},
		{item: ReleaseEvidence{DatePrecision: 0, FirstReleaseDate: "2026-01-02"}, want: 3},
		{item: ReleaseEvidence{DatePrecision: 4, FirstReleaseDate: "2026-01-02"}, want: 3},
		{item: ReleaseEvidence{DatePrecision: 0, FirstReleaseDate: "unknown"}, want: 2},
	} {
		if got := evidencePrecision(test.item); got != test.want {
			t.Fatalf("evidencePrecision(%#v)=%d, want %d", test.item, got, test.want)
		}
	}
	for _, test := range []struct {
		value     string
		precision int
		want      bool
	}{
		{"2026", 1, true}, {"2026-02", 2, true}, {"2026-02-03", 3, true},
		{"2026-2", 2, false}, {"2026-02-30", 3, false}, {"2026", 3, false}, {"", 0, false},
	} {
		if got := validEvidenceDate(test.value, test.precision); got != test.want {
			t.Fatalf("validEvidenceDate(%q,%d)=%v, want %v", test.value, test.precision, got, test.want)
		}
	}
	compatible := ReleaseEvidence{FirstReleaseDate: "2026", DatePrecision: 1}
	if !evidenceDatesCompatible(compatible, ReleaseEvidence{FirstReleaseDate: "2026-09-01", DatePrecision: 3}) {
		t.Fatal("year and day evidence should share a compatible year")
	}
	if evidenceDatesCompatible(ReleaseEvidence{FirstReleaseDate: "2026-08", DatePrecision: 2}, ReleaseEvidence{FirstReleaseDate: "2026-09", DatePrecision: 2}) {
		t.Fatal("different month evidence should not be compatible")
	}
}

func TestResolveNotificationHoldDiscardsOwnerScopedHold(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "hold-discard@example.com", "hash", "member", "UTC", "hold-discard")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "hold-discard-other@example.com", "hash", "member", "UTC", "hold-discard-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "hold-discard-artist", Name: "Hold Discard Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "hold-discard-release", artist.ID, "Hold Discard Release", "Album", "[]", "2026-08-10", 3,
		"https://musicbrainz.org/release-group/hold-discard-release", "itunes", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,planned_at,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, userID, releaseID, "announcement", "Hold title", "Hold body", "manual review", nowText(), "held", nowText())
	if err != nil {
		t.Fatal(err)
	}
	holdID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if holds, err := s.NotificationHoldsForRelease(ctx, userID, releaseID); err != nil || len(holds) != 1 || holds[0].ID != holdID {
		t.Fatalf("owner hold lookup=%#v err=%v", holds, err)
	}
	if holds, err := s.NotificationHoldsForRelease(ctx, otherID, releaseID); err != nil || len(holds) != 0 {
		t.Fatalf("cross-user hold lookup=%#v err=%v", holds, err)
	}
	if err := s.ResolveNotificationHold(ctx, userID, holdID, "invalid"); !errors.Is(err, ErrInvalidNotificationHoldAction) {
		t.Fatalf("invalid hold action error=%v", err)
	}
	if err := s.ResolveNotificationHold(ctx, otherID, holdID, "discard"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user discard error=%v", err)
	}
	if err := s.ResolveNotificationHold(ctx, userID, holdID, "discard"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, holdID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "discarded" {
		t.Fatalf("hold status=%q, want discarded", status)
	}
	var events int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events WHERE user_id=? AND release_group_id=?`, userID, releaseID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("discarded hold created %d events", events)
	}
	if holds, err := s.NotificationHolds(ctx, userID, 20); err != nil || len(holds) != 0 {
		t.Fatalf("remaining holds=%#v err=%v", holds, err)
	}
	if err := s.ResolveNotificationHold(ctx, userID, holdID, "discard"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second discard error=%v", err)
	}
}

func TestDiscardedNotificationHoldIsTerminalUntilRestored(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	userID, err := s.CreateUser(ctx, "hold-terminal@example.com", "hash", "member", "UTC", "hold-terminal")
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := s.NotificationPreferences(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	preferences.HoldConflictingNotifications = true
	if err := s.UpdateNotificationPreferences(ctx, preferences); err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "hold-terminal-artist", Name: "Hold Terminal Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "hold-terminal-release", artist.ID, "Hold Terminal Release", "Album", "[]",
		now.Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/hold-terminal-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,evidence_json,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, releaseID, "title_conflict", "warning", "hold-terminal-fingerprint", "title conflict", "[]", "open", nowText(), nowText()); err != nil {
		t.Fatal(err)
	}

	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	holds, err := s.NotificationHoldsForRelease(ctx, userID, releaseID)
	if err != nil || len(holds) != 1 {
		t.Fatalf("initial holds=%#v err=%v", holds, err)
	}
	if err := s.ResolveNotificationHold(ctx, userID, holds[0].ID, "discard"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if holds, err := s.NotificationHolds(ctx, userID, 20); err != nil || len(holds) != 0 {
		t.Fatalf("discarded hold was recreated: holds=%#v err=%v", holds, err)
	}
	allHolds, err := s.NotificationHoldsForReleaseIncludingDiscarded(ctx, userID, releaseID)
	if err != nil || len(allHolds) != 1 || allHolds[0].Status != "discarded" {
		t.Fatalf("discarded hold projection=%#v err=%v", allHolds, err)
	}

	if _, err := s.DB.ExecContext(ctx, `UPDATE release_evidence_issues SET status='resolved',resolved_at=? WHERE release_group_id=?`, nowText(), releaseID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveNotificationHold(ctx, userID, allHolds[0].ID, "restore"); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "release_day", 1)
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notification_holds WHERE id=?`, allHolds[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "released" {
		t.Fatalf("restored hold status=%q, want released", status)
	}
}

func TestWriteTransactionCancellationAndClosedDatabase(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.beginWriteTx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled beginWriteTx error=%v", err)
	}
	tx, err := s.beginWriteTx(context.Background())
	if err != nil {
		// The first transaction should be usable; this also makes the test
		// explicit about the fallback Store shape used by package tests.
		t.Fatal(err)
	}
	_ = tx.Rollback()
	_ = s.DB.Close()
	if _, err := s.beginWriteTx(context.Background()); err == nil {
		t.Fatal("closed database unexpectedly opened a transaction")
	}
}

func TestWriteHelpersHonorCancellation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "cancelled-writes", Name: "Cancelled Writes"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.MarkArtistChecked(cancelled, artist.ID, time.Now().UTC(), time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("MarkArtistChecked cancellation=%v", err)
	}
	if err := s.ScheduleSpotifyCheck(cancelled, artist.ID, time.Now().UTC()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ScheduleSpotifyCheck cancellation=%v", err)
	}
	if err := s.EnqueueEvent(cancelled, 1, 1, "announcement", "title", "body", time.Now().UTC()); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnqueueEvent cancellation=%v", err)
	}
}

func TestStoreFilterAndClassificationHelpers(t *testing.T) {
	preferences := NotificationPreferences{Albums: true, EPs: false, Singles: true}
	for _, test := range []struct {
		name    string
		primary string
		want    bool
	}{
		{name: "album enabled", primary: " Album ", want: true},
		{name: "ep disabled", primary: "ep", want: false},
		{name: "single enabled", primary: "SINGLE", want: true},
		{name: "unknown enabled", primary: "audiobook", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := releaseTypeEnabled(preferences, test.primary); got != test.want {
				t.Fatalf("releaseTypeEnabled(%q)=%v, want %v", test.primary, got, test.want)
			}
		})
	}

	release := Release{Confidence: "confirmed"}
	if got := calendarConfidenceLabel(release, true); got != "held for review" {
		t.Fatalf("held confidence=%q", got)
	}
	release.TruthIssueCount = 1
	if got := calendarConfidenceLabel(release, false); got != "review required" {
		t.Fatalf("issue confidence=%q", got)
	}
	release.TruthIssueCount = 0
	release.Confidence = "confirmed"
	if got := calendarConfidenceLabel(release, false); got != "confirmed" {
		t.Fatalf("confirmed confidence=%q", got)
	}
	release.Confidence = "single source"
	release.SourceCount = 2
	if got := calendarConfidenceLabel(release, false); got != "confirmed" {
		t.Fatalf("multi-source confidence=%q", got)
	}
	release.SourceCount = 1
	if got := calendarConfidenceLabel(release, false); got != "single source" {
		t.Fatalf("single-source confidence=%q", got)
	}

	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	where, args := releaseInboxFilters("unread", "Spotify", "EP", now)
	if !strings.Contains(where, "s.state IS NULL") || !strings.Contains(where, "rg.source=?") ||
		!strings.Contains(where, "lower(rg.primary_type)=?") || len(args) != 3 {
		t.Fatalf("inbox filters where=%q args=%#v", where, args)
	}
	where, args = releaseInboxFilters("dismissed", "", "", now)
	if !strings.Contains(where, "s.state='dismissed'") || len(args) != 0 {
		t.Fatalf("dismissed filters where=%q args=%#v", where, args)
	}

	where, args, err := evidenceIssueFilters("open", "snoozed", "date_conflict", "warning", now)
	if err != nil || !strings.Contains(where, "i.issue_type=?") || !strings.Contains(where, "i.severity=?") || len(args) != 4 {
		t.Fatalf("evidence filters where=%q args=%#v err=%v", where, args, err)
	}
	if _, _, err := evidenceIssueFilters("invalid", "", "", "", now); err == nil {
		t.Fatal("invalid evidence status was accepted")
	}
	if got := normalizeDigestFrequency(" DAILY "); got != "daily" {
		t.Fatalf("daily digest frequency=%q", got)
	}
	if got := normalizeDigestFrequency("monthly"); got != "weekly" {
		t.Fatalf("fallback digest frequency=%q", got)
	}
	if got := firstNonEmpty("", "", "fallback", "later"); got != "fallback" {
		t.Fatalf("first non-empty=%q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("empty first non-empty=%q", got)
	}
}

func TestHealthyRequiresMigratedSchema(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.Healthy(ctx); err != nil {
		t.Fatalf("healthy migrated store=%v", err)
	}
	// A read-only connection can still read the migrated schema, but the
	// rollback-only write probe must classify it as a write failure.
	readOnly := &Store{DB: s.Reader}
	err := readOnly.Healthy(ctx)
	var healthErr *DatabaseHealthError
	if !errors.As(err, &healthErr) || healthErr.State != DatabaseWriteFailed {
		t.Fatalf("read-only health error=%v, want write_failed", err)
	}
	if _, err := s.DB.ExecContext(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := s.Healthy(ctx); err == nil {
		t.Fatal("healthy reported a database without the application schema")
	}
}
