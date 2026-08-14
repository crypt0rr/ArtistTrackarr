package store

import (
	"context"
	"testing"
	"time"
)

func TestRetentionReportAndCleanupPreserveNotificationHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	old := timeText(now.Add(-10 * 24 * time.Hour))
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, old, "INFO", "old", "[]"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, timeText(now), "INFO", "current", "[]"); err != nil {
		t.Fatal(err)
	}
	userID, err := s.CreateUser(ctx, "retention@example.com", "hash", "member", "UTC", "retention")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at,created_at) VALUES(?,?,?,?,?)`, []byte("expired"), userID, "csrf", old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO login_attempts(key_hash,failures,first_at) VALUES(?,?,?)`, []byte("attempt"), 1, old); err != nil {
		t.Fatal(err)
	}

	report, err := s.RetentionReport(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Policy.ApplicationLogsDays != 7 || report.Policy.TransientStateDays != 30 {
		t.Fatalf("unexpected policy: %#v", report.Policy)
	}
	if report.PrunableApplicationLogs != 1 || report.PrunableTransientSessions != 1 || report.PrunableLoginAttempts != 1 {
		t.Fatalf("unexpected dry-run counts: %#v", report)
	}

	stats, err := s.CleanupRetention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ApplicationLogs != 1 || stats.Sessions != 1 || stats.LoginAttempts != 1 {
		t.Fatalf("unexpected cleanup stats: %#v", stats)
	}
	var logs int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM application_logs`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 1 {
		t.Fatalf("application logs=%d, want current row only", logs)
	}
	var sessions int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("sessions=%d, want expired session removed", sessions)
	}
}

func TestRetentionCleanupNeverDeletesDeliveryHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	userID, err := s.CreateUser(ctx, "history@example.com", "hash", "member", "UTC", "history")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "retention-history-artist", Name: "Retention History"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "mb-retention", artist.ID, "Retained release", "Album", "[]", "2026-01-01", 3, "https://musicbrainz.org/release-group/mb-retention", "musicbrainz", timeText(now.Add(-365*24*time.Hour)), timeText(now.Add(-365*24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, userID, releaseID, "announcement", "Retained release", "body", timeText(now.Add(-365*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	report, err := s.RetentionReport(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.NotificationEvents != 1 {
		t.Fatalf("notification events=%d, want 1", report.NotificationEvents)
	}
	if !report.HistoryReviewDue || report.HistoryAgeDays < report.Policy.HistoryReviewDays {
		t.Fatalf("history review=%v age=%d policy=%d", report.HistoryReviewDue, report.HistoryAgeDays, report.Policy.HistoryReviewDays)
	}
	if _, err := s.CleanupRetention(ctx, now); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events WHERE user_id=? AND release_group_id=?`, userID, releaseID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("notification events after cleanup=%d, want 1", events)
	}
}
