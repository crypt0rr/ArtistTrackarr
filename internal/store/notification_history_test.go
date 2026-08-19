package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNotificationDeliveryQueriesAndHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "delivery-history@example.com", "hash", "member", "UTC", "delivery-history")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "delivery-history-artist", Name: "Delivery History Artist"})
	if err != nil {
		t.Fatal(err)
	}
	releaseResult, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "delivery-history-release", artist.ID, "History Album", "Album", "[]", "2026-08-01", 3, "https://musicbrainz.org/release-group/history", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := releaseResult.LastInsertId()
	eventResult, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "History Album announced", "The body", nowText())
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := eventResult.LastInsertId()
	if err := s.AddDestination(ctx, userID, "Primary", "generic", []byte("encrypted-url")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destinationID := destinations[0].ID
	base := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	deliveryResult, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destinationID, "pending", 0, timeText(base.Add(-time.Minute)), "")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := deliveryResult.LastInsertId()
	if due, err := s.DueDeliveries(ctx, base, 10); err != nil || len(due) != 1 || due[0].ID != deliveryID || due[0].Title != "History Album announced" {
		t.Fatalf("due deliveries=%#v err=%v", due, err)
	}
	if due, err := s.DueDigestDeliveries(ctx, base, 0); err != nil || len(due) != 0 {
		t.Fatalf("zero-limit digest deliveries=%#v err=%v", due, err)
	}
	if err := s.MarkDeliveryFailed(ctx, deliveryID, 1, "send to https://example.test/hook?token=secret", base); err != nil {
		t.Fatal(err)
	}
	var status, lastError string
	if err := s.DB.QueryRowContext(ctx, `SELECT status,last_error FROM deliveries WHERE id=?`, deliveryID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || strings.Contains(lastError, "example.test") || strings.Contains(lastError, "secret") {
		t.Fatalf("failed delivery status=%q error=%q", status, lastError)
	}
	if err := s.MarkDeliveryFailed(ctx, deliveryID, 5, strings.Repeat("x", 600), base); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status,last_error FROM deliveries WHERE id=?`, deliveryID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || len(lastError) != 500 {
		t.Fatalf("permanent delivery status=%q error length=%d", status, len(lastError))
	}
	if err := s.MarkDeliverySent(ctx, deliveryID, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status,attempts,last_error FROM deliveries WHERE id=?`, deliveryID).Scan(&status, new(int), &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || lastError != "" {
		t.Fatalf("sent delivery status=%q error=%q", status, lastError)
	}

	history, err := s.DeliveryHistory(ctx, userID, 10)
	if err != nil || len(history) != 1 || history[0].Destination != "Primary" || history[0].Status != "sent" {
		t.Fatalf("delivery history=%#v err=%v", history, err)
	}
	if count, err := s.AdminDeliveryHistoryCount(ctx); err != nil || count != 1 {
		t.Fatalf("admin delivery count=%d err=%v", count, err)
	}
	adminRows, err := s.AdminDeliveryHistory(ctx, 0, -1)
	if err != nil || len(adminRows) != 1 || adminRows[0].UserEmail != "delivery-history@example.com" || adminRows[0].Body != "The body" {
		t.Fatalf("admin delivery rows=%#v err=%v", adminRows, err)
	}
	summary, err := s.AdminDeliveryHistorySummary(ctx, 0, -1)
	if err != nil || len(summary) != 1 || summary[0].Body != "" || summary[0].LastError != "" {
		t.Fatalf("admin delivery summary=%#v err=%v", summary, err)
	}
	detail, err := s.AdminDeliveryDetail(ctx, deliveryID)
	if err != nil || detail.DeliveryID != deliveryID || detail.Body != "The body" || detail.Destination != "Primary" {
		t.Fatalf("admin delivery detail=%#v err=%v", detail, err)
	}
	if _, err := s.AdminDeliveryDetail(ctx, 999999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing admin delivery error=%v", err)
	}
	if err := s.RenameDestination(ctx, userID, destinationID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, userID, destinationID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, userID, destinationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second destination delete error=%v", err)
	}
}

func TestDestinationHealthAndDeliveryAttemptAudit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "health@example.com", "hash", "member", "UTC", "health-user")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Phone", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destination := destinations[0]
	now := time.Date(2026, time.August, 8, 10, 0, 0, 0, time.UTC)
	attempt, err := s.StartDeliveryAttempt(ctx, 42, 0, destination, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	if err := s.FinishDeliveryAttempt(ctx, attempt, destination.ID, false, "send failed to https://example.test/hook?token=secret", &next, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	health, err := s.DestinationHealthByUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	got := health[destination.ID]
	if got.Status != "degraded" || got.ConsecutiveFailures != 1 || got.LastError == "" || strings.Contains(got.LastError, "example.test") {
		t.Fatalf("destination health=%#v", got)
	}
	for attemptNumber := 2; attemptNumber <= 5; attemptNumber++ {
		attempt, err := s.StartDeliveryAttempt(ctx, int64(40+attemptNumber), 0, destination, attemptNumber, now.Add(time.Duration(attemptNumber)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDeliveryAttempt(ctx, attempt, destination.ID, false, "delivery failed", &next, now.Add(time.Duration(attemptNumber)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	health, err = s.DestinationHealthByUser(ctx, userID)
	if err != nil || health[destination.ID].Status != "paused" {
		t.Fatalf("destination was not paused after five failures: health=%#v err=%v", health, err)
	}
	// A send that was already in flight when the fifth failure paused the
	// circuit must not silently reopen it. Recovery is an explicit operator
	// action through RetryFailedDeliveries.
	inFlight, err := s.StartDeliveryAttempt(ctx, 99, 0, destination, 1, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeliveryAttempt(ctx, inFlight, destination.ID, true, "", nil, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	health, err = s.DestinationHealthByUser(ctx, userID)
	if err != nil || health[destination.ID].Status != "paused" {
		t.Fatalf("in-flight success reopened paused destination: health=%#v err=%v", health, err)
	}
	admin, err := s.AdminDestinationHealth(ctx)
	if err != nil || len(admin) != 1 || admin[0].UserEmail != "health@example.com" || admin[0].Status != "paused" {
		t.Fatalf("admin health=%#v err=%v", admin, err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "health-artist", Name: "Health Artist"})
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "health-release", artist.ID, "Health Album", "Album", "[]", "2026-08-08", 3, "https://musicbrainz.org/release-group/health", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := release.LastInsertId()
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, userID, releaseID, "announcement", "Health Album", "body", nowText())
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := event.LastInsertId()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`, eventID, destination.ID, "failed", 5, timeText(now.Add(time.Hour)), "old failure"); err != nil {
		t.Fatal(err)
	}
	if due, err := s.DueDeliveries(ctx, now.Add(2*time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("paused destination returned normal due deliveries=%#v err=%v", due, err)
	}
	digest, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, "daily", "2026-08-08", "Digest", "Body", 1, "pending", nowText())
	if err != nil {
		t.Fatal(err)
	}
	digestID, _ := digest.LastInsertId()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`, digestID, destination.ID, "failed", timeText(now)); err != nil {
		t.Fatal(err)
	}
	if due, err := s.DueDigestDeliveries(ctx, now.Add(2*time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("paused destination returned digest due deliveries=%#v err=%v", due, err)
	}
	if count, err := s.RetryFailedDeliveries(ctx, userID, destination.ID, now); err != nil || count != 2 {
		t.Fatalf("retry count=%d err=%v, want normal and digest rows", count, err)
	}
	var status string
	var attempts int
	if err := s.DB.QueryRowContext(ctx, `SELECT status,attempts FROM deliveries WHERE event_id=?`, eventID).Scan(&status, &attempts); err != nil || status != "pending" || attempts != 0 {
		t.Fatalf("requeued delivery status=%q attempts=%d err=%v", status, attempts, err)
	}
	var digestStatus string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE run_id=?`, digestID).Scan(&digestStatus); err != nil || digestStatus != "pending" {
		t.Fatalf("requeued digest status=%q err=%v", digestStatus, err)
	}
	health, err = s.DestinationHealthByUser(ctx, userID)
	if err != nil || health[destination.ID].Status != "healthy" {
		t.Fatalf("manual retry did not clear pause: health=%#v err=%v", health, err)
	}
	if due, err := s.DueDeliveries(ctx, now.Add(2*time.Hour), 10); err != nil || len(due) != 1 {
		t.Fatalf("healthy destination did not resume normal deliveries=%#v err=%v", due, err)
	}
	if due, err := s.DueDigestDeliveries(ctx, now.Add(2*time.Hour), 10); err != nil || len(due) != 1 {
		t.Fatalf("healthy destination did not resume digest deliveries=%#v err=%v", due, err)
	}
	if _, err := s.RetryFailedDeliveries(ctx, userID+1, destination.ID, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user retry error=%v", err)
	}
}

func TestUnsupportedDestinationRemainsVisibleAndQueuesBlockedWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "unsupported-destination@example.com", "hash", "member", "UTC", "unsupported")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "unsupported-destination-artist", Name: "Unsupported Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Legacy push", "gotify", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destination := destinations[0]
	if destination.TransportStatus != "unsupported" || destination.TransportMessage == "" {
		t.Fatalf("legacy destination status=%q message=%q", destination.TransportStatus, destination.TransportMessage)
	}

	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{
		MBID: "unsupported-destination-release", Title: "Blocked Album", PrimaryType: "Album",
		FirstReleaseDate: "2026-09-01", DatePrecision: 3,
	}}, now); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT d.status FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id WHERE dst.id=?`, destination.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" {
		t.Fatalf("unsupported destination delivery status=%q, want blocked", status)
	}
	if due, err := s.DueDeliveries(ctx, now.Add(24*time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("blocked destination returned due deliveries=%#v err=%v", due, err)
	}
	if count, err := s.RetryFailedDeliveries(ctx, userID, destination.ID, now); err != nil || count != 1 {
		t.Fatalf("blocked destination retry count=%d err=%v", count, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE destination_id=?`, destination.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" {
		t.Fatalf("unsupported retry status=%q, want blocked", status)
	}
	// Replacing the destination's transport is an explicit operator action;
	// replay then admits the retained event without creating a duplicate.
	if _, err := s.DB.ExecContext(ctx, `UPDATE destinations SET service='generic',transport_status='supported',transport_message='' WHERE id=?`, destination.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := s.RetryFailedDeliveries(ctx, userID, destination.ID, now); err != nil || count != 1 {
		t.Fatalf("recovered destination retry count=%d err=%v", count, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE destination_id=?`, destination.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("recovered destination status=%q, want pending", status)
	}
}

func TestPausingDestinationBlocksQueuedNormalAndDigestWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "paused-queue@example.com", "hash", "member", "UTC", "paused-queue")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Primary", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destination := destinations[0]
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "paused-queue-artist", Name: "Paused Queue Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 13, 10, 0, 0, 0, time.UTC)
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "paused-queue-release", artist.ID, "Paused Album", "Album", "[]", "2026-08-13", 3, "https://musicbrainz.org/release-group/paused-queue", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := release.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Paused Album", "body", nowText())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destination.ID, "pending", 0, timeText(now), ""); err != nil {
		t.Fatal(err)
	}
	digest, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, "daily", "2026-08-13", "Digest", "Body", 1, "pending", nowText())
	if err != nil {
		t.Fatal(err)
	}
	digestID, err := digest.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`,
		digestID, destination.ID, "pending", timeText(now)); err != nil {
		t.Fatal(err)
	}

	for attemptNumber := 1; attemptNumber <= 5; attemptNumber++ {
		attempt, err := s.StartDeliveryAttempt(ctx, int64(500+attemptNumber), 0, destination, attemptNumber, now.Add(time.Duration(attemptNumber)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDeliveryAttempt(ctx, attempt, destination.ID, false, "delivery failed", &now, now.Add(time.Duration(attemptNumber)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var normalStatus, digestStatus string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE event_id=?`, eventID).Scan(&normalStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE run_id=?`, digestID).Scan(&digestStatus); err != nil {
		t.Fatal(err)
	}
	if normalStatus != "blocked" || digestStatus != "blocked" {
		t.Fatalf("queued work statuses normal=%q digest=%q, want blocked", normalStatus, digestStatus)
	}
	if count, err := s.RetryFailedDeliveries(ctx, userID, destination.ID, now); err != nil || count != 2 {
		t.Fatalf("retry count=%d err=%v, want both blocked rows", count, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE event_id=?`, eventID).Scan(&normalStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE run_id=?`, digestID).Scan(&digestStatus); err != nil {
		t.Fatal(err)
	}
	if normalStatus != "pending" || digestStatus != "pending" {
		t.Fatalf("replayed work statuses normal=%q digest=%q, want pending", normalStatus, digestStatus)
	}
}

func TestDurableDeliveryClaimsRecoverExpiredWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateInitialAdmin(ctx, "claims@example.com", "hash", "UTC", "claims-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInitialAdmin(ctx, "second@example.com", "hash", "UTC", "second-admin"); !errors.Is(err, ErrSetupCompleted) {
		t.Fatalf("second initial admin error=%v, want ErrSetupCompleted", err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "claims-artist", Name: "Claims Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Primary", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destination := destinations[0]
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "claims-release", artist.ID, "Claims Album", "Album", "[]", "2026-08-13", 3, "https://musicbrainz.org/release-group/claims", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := release.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Claims Album", "body", nowText())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destination.ID, "pending", 0, timeText(now.Add(-time.Minute)), ""); err != nil {
		t.Fatal(err)
	}
	digest, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, "daily", "2026-08-13", "Digest", "Body", 1, "pending", nowText())
	if err != nil {
		t.Fatal(err)
	}
	digestID, err := digest.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`,
		digestID, destination.ID, "pending", timeText(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}

	normal, err := s.ClaimDueDeliveries(ctx, now, 10, "worker-one", time.Minute)
	if err != nil || len(normal) != 1 || normal[0].ClaimOwner != "worker-one" {
		t.Fatalf("claimed normal deliveries=%#v err=%v", normal, err)
	}
	if again, err := s.ClaimDueDeliveries(ctx, now, 10, "worker-two", time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("second worker claimed normal delivery=%#v err=%v", again, err)
	}
	digests, err := s.ClaimDueDigestDeliveries(ctx, now, 10, "worker-one", time.Minute)
	if err != nil || len(digests) != 1 || digests[0].ClaimOwner != "worker-one" {
		t.Fatalf("claimed digest deliveries=%#v err=%v", digests, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET claim_expires_at=? WHERE event_id=?`, timeText(now.Add(-time.Second)), eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE release_digest_deliveries SET claim_expires_at=? WHERE run_id=?`, timeText(now.Add(-time.Second)), digestID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RecoverExpiredWork(ctx, now)
	if err != nil || recovered != 2 {
		t.Fatalf("recovered=%d err=%v, want normal and digest claims", recovered, err)
	}
	var owner sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT claim_owner FROM deliveries WHERE event_id=?`, eventID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner.Valid {
		t.Fatalf("normal delivery claim owner=%q after recovery", owner.String)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT claim_owner FROM release_digest_deliveries WHERE run_id=?`, digestID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner.Valid {
		t.Fatalf("digest delivery claim owner=%q after recovery", owner.String)
	}
	claimedNormal, err := s.ClaimDueDeliveries(ctx, now, 10, "worker-one", time.Minute)
	if err != nil || len(claimedNormal) != 1 {
		t.Fatalf("reclaimed normal delivery=%#v err=%v", claimedNormal, err)
	}
	claimedDigest, err := s.ClaimDueDigestDeliveries(ctx, now, 10, "worker-one", time.Minute)
	if err != nil || len(claimedDigest) != 1 {
		t.Fatalf("reclaimed digest delivery=%#v err=%v", claimedDigest, err)
	}
	if err := s.MarkDeliverySentOwned(ctx, claimedNormal[0].ID, "worker-two", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lost normal delivery claim error=%v, want sql.ErrNoRows", err)
	}
	if err := s.MarkDigestDeliveryFailedOwned(ctx, claimedDigest[0].ID, 1, "temporary", "worker-two", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lost digest delivery claim error=%v, want sql.ErrNoRows", err)
	}
}
