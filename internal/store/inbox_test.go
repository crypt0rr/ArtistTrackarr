package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReleaseInboxTracksLatestEventAndOwnerState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ownerID, err := s.CreateUser(ctx, "inbox-owner@example.com", "hash", "member", "UTC", "owner")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.CreateUser(ctx, "inbox-other@example.com", "hash", "member", "UTC", "other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "inbox-artist", Name: "Inbox Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, ownerID, artist.ID); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "inbox-release", artist.ID, "Inbox Release", "Album", "[]", "2026-08-05", 3,
		"https://musicbrainz.org/release-group/inbox-release", timeText(created), timeText(created), "musicbrainz")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
		VALUES(?,?,?,?,?,?)`, ownerID, releaseID, "announcement", "New release", "body", timeText(created)); err != nil {
		t.Fatal(err)
	}
	later := created.Add(time.Hour)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
		VALUES(?,?,?,?,?,?)`, ownerID, releaseID, "release_day", "Released today", "body", timeText(later)); err != nil {
		t.Fatal(err)
	}

	items, err := s.ReleaseInbox(ctx, ownerID, "", "", "", 50, 0, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != releaseID || items[0].EventType != "release_day" || items[0].State != "unread" {
		t.Fatalf("inbox items=%#v", items)
	}
	count, err := s.ReleaseInboxUnreadCount(ctx, ownerID, later)
	if err != nil || count != 1 {
		t.Fatalf("unread count=%d err=%v", count, err)
	}
	if err := s.SetReleaseInboxState(ctx, ownerID, releaseID, "read", nil); err != nil {
		t.Fatal(err)
	}
	count, err = s.ReleaseInboxUnreadCount(ctx, ownerID, later)
	if err != nil || count != 0 {
		t.Fatalf("read count=%d err=%v", count, err)
	}
	items, err = s.ReleaseInbox(ctx, ownerID, "read", "", "", 50, 0, later)
	if err != nil || len(items) != 1 || items[0].State != "read" {
		t.Fatalf("read items=%#v err=%v", items, err)
	}
	if err := s.SetReleaseInboxState(ctx, otherID, releaseID, "read", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user state error=%v, want sql.ErrNoRows", err)
	}
	if err := s.SetReleaseInboxState(ctx, ownerID, releaseID, "dismissed", nil); err != nil {
		t.Fatal(err)
	}
	items, err = s.ReleaseInbox(ctx, ownerID, "", "", "", 50, 0, later)
	if err != nil || len(items) != 0 {
		t.Fatalf("dismissed items=%#v err=%v", items, err)
	}
	items, err = s.ReleaseInbox(ctx, ownerID, "dismissed", "", "", 50, 0, later)
	if err != nil || len(items) != 1 || items[0].State != "dismissed" {
		t.Fatalf("dismissed filter items=%#v err=%v", items, err)
	}
}

func TestReleaseInboxSnoozeExpires(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "snooze@example.com", "hash", "member", "UTC", "snoozer")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "snooze-artist", Name: "Snooze Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	// Keep the fixture relative to the clock used by SetReleaseInboxState so
	// this validation remains stable as the calendar advances.
	now := time.Now().UTC().Truncate(time.Second)
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "snooze-release", artist.ID, "Snooze Release", "EP", "[]", "2026-08-05", 3,
		"https://musicbrainz.org/release-group/snooze-release", timeText(now), timeText(now), "itunes")
	if err != nil {
		t.Fatal(err)
	}
	releaseID, _ := result.LastInsertId()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
		VALUES(?,?,?,?,?,?)`, userID, releaseID, "announcement", "Snooze me", "body", timeText(now)); err != nil {
		t.Fatal(err)
	}
	until := now.Add(24 * time.Hour)
	if err := s.SetReleaseInboxState(ctx, userID, releaseID, "snoozed", &until); err != nil {
		t.Fatal(err)
	}
	count, err := s.ReleaseInboxUnreadCount(ctx, userID, now)
	if err != nil || count != 0 {
		t.Fatalf("snoozed unread count=%d err=%v", count, err)
	}
	count, err = s.ReleaseInboxUnreadCount(ctx, userID, until.Add(time.Second))
	if err != nil || count != 1 {
		t.Fatalf("expired snooze count=%d err=%v", count, err)
	}
}

func TestReleaseInboxOrdersUnreadAndExpiredSnoozesFirst(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "inbox-order@example.com", "hash", "member", "UTC", "inbox-order")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "inbox-order-artist", Name: "Inbox Order Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	insertRelease := func(mbid, title string, created time.Time) int64 {
		t.Helper()
		result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at,source)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, mbid, artist.ID, title, "Album", "[]", "2026-08-01", 3,
			"https://musicbrainz.org/release-group/"+mbid, timeText(created), timeText(created), "spotify")
		if err != nil {
			t.Fatal(err)
		}
		releaseID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events(user_id,release_group_id,event_type,title,body,created_at)
			VALUES(?,?,?,?,?,?)`, userID, releaseID, "announcement", title, "body", timeText(created)); err != nil {
			t.Fatal(err)
		}
		return releaseID
	}
	readID := insertRelease("inbox-order-read", "Newest read", now)
	unreadID := insertRelease("inbox-order-unread", "Older unread", now.Add(-time.Hour))
	if err := s.SetReleaseInboxState(ctx, userID, readID, "read", nil); err != nil {
		t.Fatal(err)
	}
	expiredID := insertRelease("inbox-order-expired", "Expired snooze", now.Add(-2*time.Hour))
	if err := s.SetReleaseInboxState(ctx, userID, expiredID, "snoozed", ptrTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	items, err := s.ReleaseInbox(ctx, userID, "", "", "", 20, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].ID != unreadID || items[0].State != "unread" || items[1].ID != expiredID || items[1].State != "unread" || items[2].ID != readID || items[2].State != "read" {
		t.Fatalf("inbox order=%+v", items)
	}
}
