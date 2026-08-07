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
