package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCalendarReleasesAreOwnerScopedAndExposeHoldState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "calendar-owner@example.com", "hash", "member", "UTC", "calendar-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "calendar-other@example.com", "hash", "member", "UTC", "calendar-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "calendar-artist", Name: "Calendar Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, owner, artist.ID); err != nil {
		t.Fatal(err)
	}
	releaseDate := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "calendar-release", artist.ID, "Calendar Release", "Album", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/calendar-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_holds
		(user_id,release_group_id,event_type,title,body,reason,planned_at,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, owner, releaseID, "announcement", "held", "body", "provider conflict", nowText(), "held", nowText()); err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Format("2006-01-02")
	to := time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")
	items, err := s.CalendarReleases(ctx, owner, from, to, 20)
	if err != nil || len(items) != 1 {
		t.Fatalf("calendar items=%#v err=%v", items, err)
	}
	if items[0].ID != releaseID || !items[0].Held {
		t.Fatalf("calendar release=%#v, want held release %d", items[0], releaseID)
	}
	items, err = s.CalendarReleases(ctx, other, from, to, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("cross-user calendar items=%#v", items)
	}
}

func TestCalendarReleasesDoesNotNestReaderQueries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	s.Reader.SetMaxIdleConns(1)
	userID, err := s.CreateUser(ctx, "calendar-reader@example.com", "hash", "member", "UTC", "calendar-reader")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "calendar-reader-artist", Name: "Calendar Reader Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	releaseDate := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "calendar-reader-release", artist.ID, "Calendar Reader Release", "Album", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/calendar-reader-release", "musicbrainz", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Format("2006-01-02")
	to := time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	items, err := s.CalendarReleases(queryCtx, userID, from, to, 20)
	if err != nil {
		t.Fatalf("calendar query exhausted the reader pool: %v", err)
	}
	if len(items) != 1 || items[0].ID != releaseID {
		t.Fatalf("calendar items=%#v, want release %d", items, releaseID)
	}
	if len(items[0].FollowedArtists) != 1 || items[0].FollowedArtists[0] != "Calendar Reader Artist (primary)" {
		t.Fatalf("followed associations=%v", items[0].FollowedArtists)
	}
}

func TestCalendarReleasesBatchesLargeAssociationSets(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	s.Reader.SetMaxIdleConns(1)
	userID, err := s.CreateUser(ctx, "calendar-batch@example.com", "hash", "member", "UTC", "calendar-batch")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "calendar-batch-artist", Name: "Calendar Batch Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	releaseDate := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 501; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("calendar-batch-release-%03d", i), artist.ID,
			fmt.Sprintf("Calendar Batch Release %03d", i), "Album", "[]", releaseDate, 3,
			fmt.Sprintf("https://musicbrainz.org/release-group/calendar-batch-release-%03d", i), "musicbrainz", nowText(), nowText()); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Format("2006-01-02")
	to := time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := s.CalendarReleases(queryCtx, userID, from, to, 600)
	if err != nil {
		t.Fatalf("large calendar query failed: %v", err)
	}
	if len(items) != 501 {
		t.Fatalf("calendar items=%d, want 501", len(items))
	}
	for _, item := range items {
		if len(item.FollowedArtists) != 1 || item.FollowedArtists[0] != "Calendar Batch Artist (primary)" {
			t.Fatalf("release %d followed associations=%v", item.ID, item.FollowedArtists)
		}
	}
}

func TestQueueDueReleaseDigestsDeduplicatesPeriod(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "digest@example.com", "hash", "member", "UTC", "digest-user")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "digest-artist", Name: "Digest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true,
		DigestEnabled: true, DigestFrequency: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Digest destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if now.Hour() < 10 {
		now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	} else {
		now = now.Truncate(time.Minute)
	}
	releaseDate := now.AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-release", artist.ID, "Digest Release", "EP", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/digest-release", "musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	queued, err := s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	queued, err = s.QueueDueReleaseDigests(ctx, now.Add(time.Hour))
	if err != nil || queued != 0 {
		t.Fatalf("duplicate queued=%d err=%v", queued, err)
	}
	var runs, deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || deliveries != 1 {
		t.Fatalf("digest runs=%d deliveries=%d", runs, deliveries)
	}
	due, err := s.DueDigestDeliveries(ctx, now, 10)
	if err != nil || len(due) != 1 || due[0].Title == "" || due[0].Body == "" {
		t.Fatalf("due digest=%#v err=%v", due, err)
	}
	if !strings.Contains(due[0].Body, "Digest Release") {
		t.Fatalf("digest body=%q", due[0].Body)
	}
	if err := s.MarkDigestDeliverySent(ctx, due[0].ID, now); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRow(`SELECT status FROM release_digest_runs WHERE id=?`, due[0].RunID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("digest run status=%q, want sent", status)
	}
}

func TestQueueDueReleaseDigestsWeeklyAndSkipsInvalidSchedules(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	weeklyUser, err := s.CreateUser(ctx, "weekly-digest@example.com", "hash", "member", "UTC", "weekly-digest")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "weekly-digest-artist", Name: "Weekly Digest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, weeklyUser, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: weeklyUser, Albums: true, EPs: true, Singles: true, DigestEnabled: true, DigestFrequency: "weekly",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, weeklyUser, "Weekly destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "weekly-digest-release", artist.ID, "Weekly Digest Release", "Album", "[]", "2026-08-10", 3,
		"https://musicbrainz.org/release-group/weekly-digest-release", "musicbrainz", nowText(), nowText()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	queued, err := s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 1 {
		t.Fatalf("weekly queued=%d err=%v", queued, err)
	}
	queued, err = s.QueueDueReleaseDigests(ctx, now.Add(time.Hour))
	if err != nil || queued != 0 {
		t.Fatalf("weekly duplicate queued=%d err=%v", queued, err)
	}

	invalidUser, err := s.CreateUser(ctx, "invalid-zone@example.com", "hash", "member", "UTC", "invalid-zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET timezone='Not/AZone' WHERE id=?`, invalidUser); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: invalidUser, Albums: true, DigestEnabled: true, DigestFrequency: "weekly",
	}); err != nil {
		t.Fatal(err)
	}
	lateUser, err := s.CreateUser(ctx, "late-reminder@example.com", "hash", "member", "UTC", "late-reminder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, lateUser, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: lateUser, Albums: true, DigestEnabled: true, DigestFrequency: "weekly",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET reminder_time='23:00' WHERE id=?`, lateUser); err != nil {
		t.Fatal(err)
	}
	queued, err = s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 0 {
		t.Fatalf("invalid/future schedules queued=%d err=%v", queued, err)
	}
}
