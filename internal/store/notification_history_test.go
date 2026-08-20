package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestAdminDeliveryAuditIncludesDigestDeliveries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "digest-audit@example.com", "hash", "member", "UTC", "digest-audit")
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
	created := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	run, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs
		(user_id,frequency,period_start,title,body,release_count,status,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, "daily", "2026-08-20", "Daily digest", "Digest body", 2, "pending", timeText(created))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := run.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries
		(run_id,destination_id,status,attempts,next_attempt_at,last_error)
		VALUES(?,?,?,?,?,?)`, runID, destinations[0].ID, "failed", 2, timeText(created.Add(time.Hour)), "digest failed")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, err := delivery.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "digest-audit-artist", Name: "Digest Audit Artist"})
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "digest-audit-release", artist.ID, "Normal event", "Album", "[]", "2026-08-20", 3, "https://musicbrainz.test/digest-audit", timeText(created), timeText(created))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := release.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, userID, releaseID, "announcement", "Normal event", "Normal body", timeText(created))
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries
		(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`, eventID, destinations[0].ID, "sent", 1, timeText(created), ""); err != nil {
		t.Fatal(err)
	}

	if count, err := s.AdminDeliveryHistoryCount(ctx); err != nil || count != 2 {
		t.Fatalf("digest audit count=%d err=%v", count, err)
	}
	rows, err := s.AdminDeliveryHistory(ctx, 50, 0)
	if err != nil || len(rows) != 2 || rows[0].DeliveryKind != "digest" || rows[0].EventType != "digest" || rows[0].Body != "Digest body" || rows[1].DeliveryKind != "notification" {
		t.Fatalf("digest audit rows=%#v err=%v", rows, err)
	}
	summary, err := s.AdminDeliveryHistorySummary(ctx, 50, 0)
	if err != nil || len(summary) != 2 || summary[0].DeliveryKind != "digest" || summary[0].Body != "" || summary[0].LastError != "" {
		t.Fatalf("digest audit summary=%#v err=%v", summary, err)
	}
	detail, err := s.AdminDigestDeliveryDetail(ctx, deliveryID)
	if err != nil || detail.DeliveryKind != "digest" || detail.Body != "Digest body" || detail.LastError != "digest failed" {
		t.Fatalf("digest audit detail=%#v err=%v", detail, err)
	}
	exported, cursor, err := s.AdminDeliveryHistoryExportPage(ctx, 1, nil)
	if err != nil || len(exported) != 1 || exported[0].DeliveryKind != "digest" || cursor == nil || cursor.DeliveryKind != "digest" {
		t.Fatalf("digest audit export first page rows=%#v cursor=%#v err=%v", exported, cursor, err)
	}
	exported, cursor, err = s.AdminDeliveryHistoryExportPage(ctx, 1, cursor)
	if err != nil || len(exported) != 1 || exported[0].DeliveryKind != "notification" || cursor == nil || cursor.DeliveryKind != "notification" {
		t.Fatalf("digest audit export second page rows=%#v cursor=%#v err=%v", exported, cursor, err)
	}
	exported, cursor, err = s.AdminDeliveryHistoryExportPage(ctx, 1, cursor)
	if err != nil || len(exported) != 0 || cursor != nil {
		t.Fatalf("digest audit export final page rows=%#v cursor=%#v err=%v", exported, cursor, err)
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
	setDestinationCreatedAt(t, s, userID, now.Add(-time.Hour))
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

func TestDestinationsAddedAfterEventsAreNotBackfilled(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "future-destination@example.com", "hash", "member", "UTC", "future-destination")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "future-destination-artist", Name: "Future Destination Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{
		MBID: "historical-destination-release", Title: "Historical Release", PrimaryType: "Album",
		FirstReleaseDate: "2026-08-20", DatePrecision: 3,
	}}, now); err != nil {
		t.Fatal(err)
	}
	var historicalDeliveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`).Scan(&historicalDeliveries); err != nil {
		t.Fatal(err)
	}
	if historicalDeliveries != 0 {
		t.Fatalf("historical deliveries before destination=%d, want 0", historicalDeliveries)
	}
	if err := s.AddDestination(ctx, userID, "Future destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	// Keep the fixture clock independent of wall time. The first event is at
	// now, while the second is two hours later; place destination creation
	// between them so only the second event is admitted.
	setDestinationCreatedAt(t, s, userID, now.Add(90*time.Minute))
	// Re-running the idempotent event path must not admit the destination into
	// an event that predates it.
	var releaseID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, "historical-destination-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueEvent(ctx, userID, releaseID, "announcement", "Historical Release", "body", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`).Scan(&historicalDeliveries); err != nil {
		t.Fatal(err)
	}
	if historicalDeliveries != 0 {
		t.Fatalf("historical deliveries after destination=%d, want 0", historicalDeliveries)
	}

	// A new event created after the destination is configured is admitted.
	if err := s.ApplyReleaseSync(ctx, artist, []Release{{
		MBID: "future-destination-release", Title: "Future Release", PrimaryType: "Album",
		FirstReleaseDate: "2026-09-01", DatePrecision: 3,
	}}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var totalDeliveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`).Scan(&totalDeliveries); err != nil {
		t.Fatal(err)
	}
	if totalDeliveries != 1 {
		t.Fatalf("future deliveries=%d, want 1", totalDeliveries)
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
	if err := s.FinalizeDeliverySent(ctx, claimedNormal[0].ID, now); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE id=?`, claimedNormal[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("active normal claim status=%q, want pending", status)
	}
	if err := s.FinalizeDigestDeliverySent(ctx, claimedDigest[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE id=?`, claimedDigest[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("active digest claim status=%q, want pending", status)
	}
	if err := s.MarkDeliverySentOwned(ctx, claimedNormal[0].ID, "worker-two", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lost normal delivery claim error=%v, want sql.ErrNoRows", err)
	}
	if err := s.MarkDigestDeliveryFailedOwned(ctx, claimedDigest[0].ID, 1, "temporary", "worker-two", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lost digest delivery claim error=%v, want sql.ErrNoRows", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET claim_expires_at=? WHERE id=?`, timeText(now.Add(-time.Second)), claimedNormal[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE release_digest_deliveries SET claim_expires_at=? WHERE id=?`, timeText(now.Add(-time.Second)), claimedDigest[0].ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverExpiredWork(ctx, now); err != nil || recovered != 2 {
		t.Fatalf("recovered after active claim=%d err=%v, want two", recovered, err)
	}
	if err := s.FinalizeDeliverySent(ctx, claimedNormal[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeDigestDeliverySent(ctx, claimedDigest[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE id=?`, claimedNormal[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("finalized normal status=%q, want sent", status)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM release_digest_deliveries WHERE id=?`, claimedDigest[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("finalized digest status=%q, want sent", status)
	}
}

func TestRecoverExpiredWorkRequeuesStaleManualSyncWithoutLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateInitialAdmin(ctx, "stale-manual@example.com", "hash", "UTC", "stale-manual")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO manual_sync_requests(requested_by,scope,status,created_at,started_at) VALUES(?,?,?,?,?)`,
		userID, "retry", "running", timeText(old), timeText(old)); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverExpiredWork(ctx, now); err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v, want one stale manual request", recovered, err)
	}
	var status string
	var started sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT status,started_at FROM manual_sync_requests WHERE requested_by=?`, userID).Scan(&status, &started); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || started.Valid {
		t.Fatalf("stale manual request status=%q started=%v, want queued and NULL", status, started)
	}
}

func TestRepairClockSkewedDeliveriesRequeuesOnlyUnclaimedFarFutureRows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "clock-repair@example.com", "hash", "member", "UTC", "clock-repair")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "clock-repair-artist", Name: "Clock Repair Artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Primary", "Secondary", "Active"} {
		if err := s.AddDestination(ctx, userID, name, "generic", []byte("encrypted")); err != nil {
			t.Fatal(err)
		}
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 3 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "clock-repair-release", artist.ID, "Clock Repair", "Album", "[]", "2026-08-20", 3, "https://musicbrainz.org/release-group/clock-repair", timeText(now), timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := release.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Clock Repair", "body", timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	farFuture := now.Add(48 * time.Hour)
	nearFuture := now.Add(2 * time.Hour)
	activeFuture := now.Add(72 * time.Hour)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destinations[0].ID, "pending", 0, timeText(farFuture), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destinations[1].ID, "pending", 0, timeText(nearFuture), ""); err != nil {
		t.Fatal(err)
	}
	active, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error,claim_owner,claim_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		eventID, destinations[2].ID, "pending", 0, timeText(activeFuture), "", "live-worker", timeText(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	activeID, err := active.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, "daily", "2026-08-20", "Digest", "Body", 1, "pending", timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	digestID, err := digest.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`,
		digestID, destinations[0].ID, "blocked", timeText(farFuture)); err != nil {
		t.Fatal(err)
	}

	stats, err := s.RepairClockSkewedDeliveries(ctx, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deliveries != 1 || stats.DigestDeliveries != 1 {
		t.Fatalf("repair stats=%#v, want one normal and one digest row", stats)
	}
	var repaired, unchanged, near string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries WHERE id=(SELECT MIN(id) FROM deliveries)`).Scan(&repaired); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries WHERE destination_id=?`, destinations[1].ID).Scan(&near); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM deliveries WHERE id=?`, activeID).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if repaired != timeText(now) || near != timeText(nearFuture) || unchanged != timeText(activeFuture) {
		t.Fatalf("repaired=%q near=%q active=%q, want now, near unchanged, active unchanged", repaired, near, unchanged)
	}
	var digestNext string
	if err := s.DB.QueryRowContext(ctx, `SELECT next_attempt_at FROM release_digest_deliveries WHERE run_id=?`, digestID).Scan(&digestNext); err != nil {
		t.Fatal(err)
	}
	if digestNext != timeText(now) {
		t.Fatalf("digest next_attempt_at=%q, want %q", digestNext, timeText(now))
	}
}

func TestDeliveryClaimsShareBatchAcrossUsers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userOne, err := s.CreateInitialAdmin(ctx, "fair-one@example.com", "hash", "UTC", "fair-one")
	if err != nil {
		t.Fatal(err)
	}
	userTwo, err := s.CreateUser(ctx, "fair-two@example.com", "hash", "member", "UTC", "fair-two")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "fair-artist", Name: "Fair Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userOne, "One", "generic", []byte("encrypted-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userTwo, "Two", "generic", []byte("encrypted-two")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userOne)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("user one destinations=%#v err=%v", destinations, err)
	}
	destinationOne := destinations[0]
	destinations, err = s.Destinations(ctx, userTwo)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("user two destinations=%#v err=%v", destinations, err)
	}
	destinationTwo := destinations[0]
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		owner, destination := userOne, destinationOne
		if i == 6 {
			owner, destination = userTwo, destinationTwo
		}
		release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("fair-release-%d", i), artist.ID, fmt.Sprintf("Fair Release %d", i), "Album", "[]", "2026-08-13", 3, "", timeText(now), timeText(now))
		if err != nil {
			t.Fatal(err)
		}
		releaseID, err := release.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, owner, releaseID, "announcement", "Fair release", "body", timeText(now))
		if err != nil {
			t.Fatal(err)
		}
		eventID, err := event.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`, eventID, destination.ID, "pending", 0, timeText(now.Add(-time.Minute)), ""); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := s.ClaimDueDeliveries(ctx, now, 25, "fair-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 6 {
		t.Fatalf("claimed=%d, want five from user one plus one from user two", len(claimed))
	}
	counts := map[int64]int{}
	for _, delivery := range claimed {
		counts[delivery.Destination.UserID]++
	}
	if counts[userOne] != maxDeliveryClaimsPerUser || counts[userTwo] != 1 {
		t.Fatalf("claimed per user=%v, want user one=%d user two=1", counts, maxDeliveryClaimsPerUser)
	}
}

func TestReconcileStaleDeliveryAttemptsMarksOnlyExpiredAttempts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "stale-attempts@example.com", "hash", "member", "UTC", "stale-attempts")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "stale-attempts-artist", Name: "Stale Attempts Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Primary", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destination := destinations[0]
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	release, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "stale-attempts-release", artist.ID, "Stale Attempts", "Album", "[]", "2026-08-20", 3, "", timeText(now), timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := release.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, "announcement", "Stale Attempts", "body", timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`,
		eventID, destination.ID, "pending", 0, timeText(now), "")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, err := delivery.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	staleAttempt, err := s.StartDeliveryAttempt(ctx, deliveryID, 0, destination, 1, now.Add(-20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	freshAttempt, err := s.StartDeliveryAttempt(ctx, deliveryID, 0, destination, 2, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	digest, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs(user_id,frequency,period_start,title,body,release_count,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, "daily", "2026-08-20", "Digest", "Body", 1, "pending", timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	digestID, err := digest.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	digestDelivery, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_deliveries(run_id,destination_id,status,next_attempt_at) VALUES(?,?,?,?)`,
		digestID, destination.ID, "pending", timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	digestDeliveryID, err := digestDelivery.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	digestAttempt, err := s.StartDeliveryAttempt(ctx, 0, digestDeliveryID, destination, 1, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	reconciled, err := s.ReconcileStaleDeliveryAttempts(ctx, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled != 2 {
		t.Fatalf("reconciled=%d, want one normal and one digest stale attempt", reconciled)
	}
	for _, id := range []int64{staleAttempt, digestAttempt} {
		var status, finished, abandoned, lastError string
		if err := s.DB.QueryRowContext(ctx, `SELECT status,finished_at,abandoned_at,last_error FROM delivery_attempts WHERE id=?`, id).
			Scan(&status, &finished, &abandoned, &lastError); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || finished == "" || abandoned == "" || lastError != "worker attempt expired" {
			t.Fatalf("attempt %d status=%q finished=%q abandoned=%q error=%q, want failed audit", id, status, finished, abandoned, lastError)
		}
	}
	var freshStatus string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM delivery_attempts WHERE id=?`, freshAttempt).Scan(&freshStatus); err != nil {
		t.Fatal(err)
	}
	if freshStatus != "started" {
		t.Fatalf("fresh attempt status=%q, want started", freshStatus)
	}
}
