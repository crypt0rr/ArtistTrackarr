package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCalendarFeedTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "calendar-feed@example.com", "hash", "member", "UTC", "calendar-feed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CalendarFeedTokenStatus(ctx, userID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing token status error=%v, want sql.ErrNoRows", err)
	}
	first, err := s.CreateCalendarFeedToken(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 {
		t.Fatalf("token length=%d, want opaque token", len(first))
	}
	if got, err := s.UserIDByCalendarFeedToken(ctx, first); err != nil || got != userID {
		t.Fatalf("first token lookup=(%d,%v), want user %d", got, err, userID)
	}
	status, err := s.CalendarFeedTokenStatus(ctx, userID)
	if err != nil || !status.Active || status.ExpiresAt.Before(status.CreatedAt) {
		t.Fatalf("token status=%#v err=%v", status, err)
	}
	second, err := s.CreateCalendarFeedToken(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserIDByCalendarFeedToken(ctx, first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rotated token remained valid: %v", err)
	}
	if got, err := s.UserIDByCalendarFeedToken(ctx, second); err != nil || got != userID {
		t.Fatalf("rotated token lookup=(%d,%v), want user %d", got, err, userID)
	}
	if err := s.RevokeCalendarFeedToken(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserIDByCalendarFeedToken(ctx, second); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked token remained valid: %v", err)
	}
	status, err = s.CalendarFeedTokenStatus(ctx, userID)
	if err != nil || status.Active || status.RevokedAt == nil {
		t.Fatalf("revoked status=%#v err=%v", status, err)
	}
	if err := s.RevokeCalendarFeedToken(ctx, userID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second revoke error=%v, want sql.ErrNoRows", err)
	}

	third, err := s.CreateCalendarFeedToken(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE calendar_feed_tokens SET expires_at=? WHERE user_id=?`, timeText(time.Now().UTC().Add(-time.Minute)), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserIDByCalendarFeedToken(ctx, third); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired token remained valid: %v", err)
	}
}

func TestReminderMinutesNormalizesLegacyValues(t *testing.T) {
	cases := []struct {
		value string
		want  int
		ok    bool
	}{
		{value: "09:00", want: 9 * 60, ok: true},
		{value: "9:05", want: 9*60 + 5, ok: true},
		{value: " 23:59 ", want: 23*60 + 59, ok: true},
		{value: "24:00", ok: false},
		{value: "not-a-time", ok: false},
	}
	for _, tc := range cases {
		got, ok := reminderMinutes(tc.value)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("reminderMinutes(%q)=(%d,%v), want (%d,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeReminderTimeCanonicalizesLegacyInput(t *testing.T) {
	cases := map[string]string{
		"09:00":   "09:00",
		"9:05":    "09:05",
		" 23:59 ": "23:59",
	}
	for value, want := range cases {
		got, ok := normalizeReminderTime(value)
		if !ok || got != want {
			t.Errorf("normalizeReminderTime(%q)=(%q,%v), want (%q,true)", value, got, ok, want)
		}
	}
	if got, ok := normalizeReminderTime("24:00"); ok || got != "" {
		t.Fatalf("invalid reminder normalized to (%q,%v)", got, ok)
	}
}

func TestBuildDigestBodyIsBoundedAndValidUTF8(t *testing.T) {
	releases := make([]CalendarRelease, 0, 100)
	for i := 0; i < 100; i++ {
		releases = append(releases, CalendarRelease{Release: Release{
			Title: strings.Repeat("é", 120), PrimaryType: "Album", FirstReleaseDate: "2026-08-21", DatePrecision: 3,
			ArtistName: "Digest Artist",
		}, CalendarDate: "2026-08-21"})
	}
	body := buildDigestBody(releases, "daily")
	if len([]byte(body)) > 3500 {
		t.Fatalf("digest body bytes=%d, want <=3500", len([]byte(body)))
	}
	if !utf8.ValidString(body) {
		t.Fatal("digest body is not valid UTF-8")
	}
	if !strings.Contains(body, "additional releases omitted") {
		t.Fatalf("bounded digest did not explain truncation: %q", body)
	}
}

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

func TestCalendarReleasesConcurrentReadersDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	s.Reader.SetMaxIdleConns(1)
	userID, err := s.CreateUser(ctx, "calendar-concurrent@example.com", "hash", "member", "UTC", "calendar-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "calendar-concurrent-artist", Name: "Calendar Concurrent Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	releaseDate := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "calendar-concurrent-release", artist.ID, "Calendar Concurrent Release", "Album", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/calendar-concurrent-release", "musicbrainz", nowText(), nowText()); err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Format("2006-01-02")
	to := time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			items, err := s.CalendarReleases(queryCtx, userID, from, to, 20)
			if err != nil {
				errs <- err
				return
			}
			if len(items) != 1 || len(items[0].FollowedArtists) != 1 {
				errs <- fmt.Errorf("calendar items=%d followed=%d", len(items), len(items[0].FollowedArtists))
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent calendar query failed: %v", err)
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
	// The 501-row association batch is intentionally large and race
	// instrumentation makes the JSON/date projection substantially slower on
	// constrained CI runners. Keep a finite bound while avoiding a false
	// failure that leaves the package's SQLite migrations waiting behind a
	// cancelled reader.
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	items, err := s.CalendarReleases(queryCtx, userID, from, to, 600)
	if err != nil {
		t.Fatalf("large calendar query failed: %v", err)
	}
	if len(items) != 501 {
		t.Fatalf("calendar items=%d, want 501", len(items))
	}
	firstPage, err := s.CalendarReleasesPage(queryCtx, userID, from, to, 25, 0)
	if err != nil || len(firstPage) != 25 {
		t.Fatalf("first calendar page len=%d err=%v", len(firstPage), err)
	}
	secondPage, err := s.CalendarReleasesPage(queryCtx, userID, from, to, 25, 25)
	if err != nil || len(secondPage) != 25 {
		t.Fatalf("second calendar page len=%d err=%v", len(secondPage), err)
	}
	if firstPage[0].ID == secondPage[0].ID {
		t.Fatalf("calendar pages overlap at release %d", firstPage[0].ID)
	}
	for _, item := range items {
		if len(item.FollowedArtists) != 1 || item.FollowedArtists[0] != "Calendar Batch Artist (primary)" {
			t.Fatalf("release %d followed associations=%v", item.ID, item.FollowedArtists)
		}
	}
}

func TestQueueDueReleaseDigestsDeduplicatesPeriod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	// The digest scheduler performs nested calendar and rule reads for each
	// eligible user. A one-reader pool makes sure the outer user scan is fully
	// materialized and closed before those nested queries begin.
	s.Reader.SetMaxOpenConns(1)
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
	// A digest must remain visible when no destination exists, but adding a
	// destination later must not backfill this historical run.
	queued, err := s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 0 {
		t.Fatalf("orphan digest queued=%d err=%v", queued, err)
	}
	var orphanRuns int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&orphanRuns); err != nil {
		t.Fatal(err)
	}
	if orphanRuns != 1 {
		t.Fatalf("orphan digest runs=%d, want 1 pending run", orphanRuns)
	}
	if err := s.AddDestination(ctx, userID, "Digest destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	// Keep the destination newer than the orphaned run. The production query
	// intentionally admits only destinations that existed when a digest was
	// created; using the wall clock here made this fixture depend on when the
	// test happened to run relative to its logical test time.
	setDestinationCreatedAt(t, s, userID, now.Add(time.Hour))
	queued, err = s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 0 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	var historicalDeliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_deliveries`).Scan(&historicalDeliveries); err != nil {
		t.Fatal(err)
	}
	if historicalDeliveries != 0 {
		t.Fatalf("historical digest deliveries=%d, want 0", historicalDeliveries)
	}

	// A digest created after the destination exists is admitted normally. This
	// distinguishes future work from replay of the orphaned historical run.
	futureReleaseDate := now.AddDate(0, 0, 3).Format("2006-01-02")
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-future-release", artist.ID, "Future Digest Release", "EP", "[]",
		futureReleaseDate, 3, "https://musicbrainz.org/release-group/digest-future-release", "musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	queued, err = s.QueueDueReleaseDigests(ctx, now.Add(time.Hour))
	if err != nil || queued != 0 {
		t.Fatalf("duplicate queued=%d err=%v", queued, err)
	}
	queued, err = s.QueueDueReleaseDigests(ctx, now.AddDate(0, 0, 2))
	if err != nil || queued != 1 {
		t.Fatalf("future queued=%d err=%v", queued, err)
	}
	var runs, deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if runs != 2 || deliveries != 1 {
		t.Fatalf("digest runs=%d deliveries=%d", runs, deliveries)
	}
	due, err := s.DueDigestDeliveries(ctx, now.AddDate(0, 0, 2), 10)
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

func TestQueueDueReleaseDigestsUsesCreditedFollowRule(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "digest-credit@example.com", "hash", "member", "UTC", "digest-credit")
	if err != nil {
		t.Fatal(err)
	}
	primary, err := s.UpsertArtist(ctx, Artist{MBID: "digest-credit-primary", Name: "Primary Digest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.UpsertArtist(ctx, Artist{MBID: "digest-credit-guest", Name: "Guest Digest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, guest.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true,
		DigestEnabled: true, DigestFrequency: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Digest credit destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	setDestinationCreatedAt(t, s, userID, now.Add(-time.Hour))
	result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-credit-release", primary.ID, "Credited Digest Release", "Album", "[]",
		"2026-08-21", 3, "https://musicbrainz.org/release-group/digest-credit-release", "musicbrainz", timeText(now), timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_credits
		(release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,provider_url,confidence,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, releaseID, guest.ID, "musicbrainz", "digest-credit-track", "guest", "Guest track",
		"Primary Digest Artist feat. Guest Digest Artist", "https://musicbrainz.org/recording/digest-credit-track", "confirmed", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}

	// The release is visible through the guest credit, but an explicit off rule
	// must keep it out of the digest. This catches regressions that consult only
	// the canonical release artist's (non-followed) rule.
	off := defaultFollowNotificationRule(userID, guest.ID, now)
	off.DeliveryMode = FollowDeliveryOff
	if err := s.UpdateFollowNotificationRule(ctx, userID, guest.ID, off); err != nil {
		t.Fatal(err)
	}
	items, err := s.CalendarReleases(ctx, userID, "2026-08-20", "2026-08-27", 20)
	if err != nil || len(items) != 1 || len(items[0].FollowedAssociations) != 1 || items[0].FollowedAssociations[0].ArtistID != guest.ID || items[0].FollowedAssociations[0].Role != "guest" {
		t.Fatalf("credited calendar associations=%#v err=%v", items, err)
	}
	if queued, err := s.QueueDueReleaseDigests(ctx, now); err != nil || queued != 0 {
		t.Fatalf("off credited follow queued=%d err=%v", queued, err)
	}

	// Switching the same follow to digest mode admits the existing release in
	// the same period and includes the followed association in the body.
	digest := off
	digest.DeliveryMode = FollowDeliveryDigest
	if err := s.UpdateFollowNotificationRule(ctx, userID, guest.ID, digest); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.QueueDueReleaseDigests(ctx, now.Add(time.Hour)); err != nil || queued != 1 {
		t.Fatalf("digest credited follow queued=%d err=%v", queued, err)
	}
	due, err := s.DueDigestDeliveries(ctx, now.Add(time.Hour), 10)
	if err != nil || len(due) != 1 || !strings.Contains(due[0].Body, "Guest Digest Artist (guest)") {
		t.Fatalf("credited digest=%#v err=%v", due, err)
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
	setDestinationCreatedAt(t, s, weeklyUser, now.Add(-time.Hour))
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

func TestQueueDueReleaseDigestsTreatsTimezoneChangesAsOnePeriod(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "digest-timezone@example.com", "hash", "member", "UTC", "digest-timezone")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "digest-timezone-artist", Name: "Digest Timezone Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true, DigestEnabled: true, DigestFrequency: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET reminder_time='00:00' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Timezone destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 21, 1, 0, 0, 0, time.UTC)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-timezone-release", artist.ID, "Timezone Release", "Album", "[]",
		"2026-08-21", 3, "https://musicbrainz.org/release-group/digest-timezone-release", "musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	// The fixed test clock must also control destination age. Otherwise a
	// destination created after this historical instant is excluded, making
	// the expected initial digest depend on the host wall clock.
	setDestinationCreatedAt(t, s, userID, now.Add(-time.Hour))
	if queued, err := s.QueueDueReleaseDigests(ctx, now); err != nil || queued != 1 {
		t.Fatalf("initial digest queued=%d err=%v", queued, err)
	}
	// The same instant is still within the original UTC period after changing
	// to a timezone whose local date is the previous day. It must not create a
	// second digest for that one logical daily period.
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET timezone='America/Los_Angeles' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.QueueDueReleaseDigests(ctx, now.Add(time.Hour)); err != nil || queued != 0 {
		t.Fatalf("timezone-change digest queued=%d err=%v", queued, err)
	}
	var runs int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("digest runs=%d, want 1", runs)
	}
}

func TestDigestOnlyFollowGetsADigestRunWithTheAccountDigestOff(t *testing.T) {
	// release_digest_enabled defaults to 0. A follow set to "Digest only" then
	// produced a notification event with no delivery rows and no digest run:
	// the alert went nowhere, and because notification_events is unique per
	// (user, release, event type) it could never be re-queued afterwards.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	userID, err := s.CreateUser(ctx, "digest-only@example.com", "hash", "member", "UTC", "digest-only")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "digest-only-artist", Name: "Digest Only Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	// Account digest deliberately left off, which is the shipped default.
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true,
		DigestEnabled: false, DigestFrequency: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.DeliveryMode = FollowDeliveryDigest
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "digest-only-release", artist.ID, "Digest Only Release", "EP", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/digest-only-release", "musicbrainz",
		timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("digest runs=%d for a digest-only follow with the account digest off, want 1", runs)
	}
}

func TestCalendarKeepsMusicBrainzOnlyReleases(t *testing.T) {
	// The provider-preference clause used to ask whether the ARTIST had any
	// provider release, so a genuinely MusicBrainz-only release vanished from
	// the calendar, the ICS export and the digest the moment that artist gained
	// a single Spotify release - while the announcement path still notified the
	// member about it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "mb-only@example.com", "hash", "member", "UTC", "mb-only")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "mb-only-artist", Name: "MB Only Artist", SpotifyID: "spotify-artist-id"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insert := func(mbid, title, source, date string) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, mbid, artist.ID, title, "Album", "[]", date, 3,
			"https://musicbrainz.org/release-group/"+mbid, source, timeText(now), timeText(now)); err != nil {
			t.Fatal(err)
		}
	}
	spotifyDate := now.AddDate(0, 0, 3).Format("2006-01-02")
	// Far enough from the Spotify release that it cannot be a duplicate of it.
	mbDate := now.AddDate(0, 0, 20).Format("2006-01-02")
	insert("spotify-release", "Provider Album", "spotify", spotifyDate)
	insert("musicbrainz-release", "Catalogue Only Album", "musicbrainz", mbDate)

	items, err := s.CalendarReleases(ctx, userID, now.Format("2006-01-02"), now.AddDate(0, 2, 0).Format("2006-01-02"), 50)
	if err != nil {
		t.Fatal(err)
	}
	titles := make(map[string]bool, len(items))
	for _, item := range items {
		titles[item.Title] = true
	}
	if !titles["Provider Album"] {
		t.Fatalf("the provider release is missing: %v", titles)
	}
	if !titles["Catalogue Only Album"] {
		t.Fatalf("a MusicBrainz-only release was filtered out of the calendar: %v", titles)
	}

	// A MusicBrainz row that really is an unmerged duplicate of the provider row
	// is still suppressed. The title must match: the clause compares case-folded
	// titles rather than reimplementing the Go normaliser in SQL, so it
	// deliberately suppresses less than releaseIdentityMatches would.
	insert("musicbrainz-duplicate", "provider album", "musicbrainz", spotifyDate)
	items, err = s.CalendarReleases(ctx, userID, now.Format("2006-01-02"), now.AddDate(0, 2, 0).Format("2006-01-02"), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Title == "provider album" {
			t.Fatalf("an unmerged duplicate of a provider release was shown: %v", item)
		}
	}

	// A differently titled release on the same day is a different release, not a
	// duplicate, and must stay visible. Comparing only artist and date hid these.
	insert("musicbrainz-sameday", "A Completely Different Record", "musicbrainz", spotifyDate)
	items, err = s.CalendarReleases(ctx, userID, now.Format("2006-01-02"), now.AddDate(0, 2, 0).Format("2006-01-02"), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Title == "A Completely Different Record" {
			found = true
		}
	}
	if !found {
		t.Fatal("a different release sharing a provider release's date was hidden from the calendar")
	}
}

func TestTimezoneChangeDoesNotSuppressTheNextWeeklyDigest(t *testing.T) {
	// The duplicate-period lookup widens its window when the stored run carries a
	// different timezone, to catch the same logical period under a new offset.
	// Widening it by a whole period meant the PREVIOUS period's run - which
	// necessarily carries the old timezone - fell inside the window, so a weekly
	// subscriber who changed timezone lost a full week of digests.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	userID, err := s.CreateUser(ctx, "tz-digest@example.com", "hash", "member", "UTC", "tz-digest")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "tz-digest-artist", Name: "TZ Digest Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true,
		DigestEnabled: true, DigestFrequency: "weekly",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if now.Hour() < 10 {
		now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	} else {
		now = now.Truncate(time.Minute)
	}
	releaseDate := now.AddDate(0, 0, 2).Format("2006-01-02")
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "tz-digest-release", artist.ID, "TZ Digest Release", "EP", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/tz-digest-release", "musicbrainz",
		timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	// Control: with no prior run at all, a digest must be created. If this fails
	// the scenario is wrong, not the deduplication.
	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}
	var control int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&control); err != nil {
		t.Fatal(err)
	}
	if control != 1 {
		t.Fatalf("control: a weekly digest was not created at all (runs=%d)", control)
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM release_digest_runs WHERE user_id=?`, userID); err != nil {
		t.Fatal(err)
	}

	// Last week's run, recorded under the member's previous timezone. Weekly
	// periods start on Monday, so anchor it to the previous Monday rather than
	// to an arbitrary day, which is what the scheduler actually writes.
	thisMonday := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)).Truncate(24 * time.Hour)
	lastMonday := thisMonday.AddDate(0, 0, -7)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs
		(user_id,frequency,period_start,timezone,title,body,release_count,status,created_at)
		VALUES(?,?,?,?,?,?,?, 'sent',?)`, userID, "weekly",
		lastMonday.Format("2006-01-02"), "America/New_York", "Last week", "body", 1,
		timeText(lastMonday.Add(12*time.Hour))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}

	var runs int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=? AND created_at>=?`,
		userID, timeText(now.Add(-time.Hour))).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("this week's digest runs=%d after a timezone change, want 1", runs)
	}
}

// TestQueuedDigestIsNotRebuiltOnEveryTick pins the pre-check that makes the
// scheduler cheap once a member's digest for the period is settled. The
// scheduler runs on a sixty-second tick and on every UI-triggered wake, and
// used to run FollowNotificationRules plus up to five CalendarReleasesPage
// scans - the heaviest read in the store - build the digest body, and only then
// open a write transaction whose first statement discovered the run already
// existed. For a daily digest that is roughly nine hundred full rebuilds and
// discarded write transactions per member per day, against a four-connection
// reader pool and a single writer shared with the web UI.
//
// "Did not scan" is made observable by taking the table the scan reads out of
// reach. The control case at the end proves the trap actually springs.
func TestQueuedDigestIsNotRebuiltOnEveryTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	userID, err := s.CreateUser(ctx, "settled@example.com", "hash", "member", "UTC", "settled-user")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "settled-artist", Name: "Settled Artist"})
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "settled-release", artist.ID, "Settled Release", "EP", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/settled-release", "musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	// The destination must predate the run for deliveries to attach, which is
	// what settles the period.
	if err := s.AddDestination(ctx, userID, "Digest destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	setDestinationCreatedAt(t, s, userID, now.Add(-time.Hour))

	queued, err := s.QueueDueReleaseDigests(ctx, now)
	if err != nil || queued != 1 {
		t.Fatalf("first tick queued=%d err=%v, want 1", queued, err)
	}
	var deliveries int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM release_digest_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries == 0 {
		t.Fatal("precondition: the run must have deliveries for the period to be settled")
	}

	hideReleaseGroups(t, s)
	queued, err = s.QueueDueReleaseDigests(ctx, now)
	if err != nil {
		t.Fatalf("a settled period still ran the digest scan: %v", err)
	}
	if queued != 0 {
		t.Fatalf("second tick queued=%d, want 0", queued)
	}
	restoreReleaseGroups(t, s)

	// Control: with no run for the period, the same call must reach the scan
	// and fail - otherwise the assertion above would pass for the wrong reason.
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM release_digest_deliveries`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM release_digest_runs`); err != nil {
		t.Fatal(err)
	}
	hideReleaseGroups(t, s)
	defer restoreReleaseGroups(t, s)
	if _, err := s.QueueDueReleaseDigests(ctx, now); err == nil {
		t.Fatal("control: an unsettled period did not reach the scan, so this test proves nothing")
	}
}

// hideReleaseGroups puts the table the digest scan reads out of reach without
// touching anything the de-duplication pre-check needs.
func hideReleaseGroups(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB.Exec(`ALTER TABLE release_groups RENAME TO release_groups_hidden`); err != nil {
		t.Fatal(err)
	}
}

func restoreReleaseGroups(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB.Exec(`ALTER TABLE release_groups_hidden RENAME TO release_groups`); err != nil {
		t.Fatal(err)
	}
}

// TestTimezoneChangeDoesNotSuppressTheNextDailyDigest is the daily half of the
// same class. A weekly period is 168h, so the 26h tolerance cannot reach the
// previous week's run - which is all the original fix was verified against. A
// daily period is 24h, and yesterday's run is created at the member's reminder
// time, so it falls inside [periodStart-26h, ...) at every reminder time. The
// first tick after any timezone change therefore found yesterday's run, saw a
// non-pending status and skipped the whole day: no run row, no delivery, no log
// line, and a return value of 0 rather than an error.
//
// The reminder offset is the reason a wider or narrower window cannot fix this
// on its own - at 23:00 the previous run sits an hour before the current period
// even begins - so the previous period is excluded by key instead.
func TestTimezoneChangeDoesNotSuppressTheNextDailyDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	userID, err := s.CreateUser(ctx, "tz-daily@example.com", "hash", "member", "UTC", "tz-daily")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "tz-daily-artist", Name: "TZ Daily Artist"})
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "tz-daily-release", artist.ID, "TZ Daily Release", "EP", "[]",
		releaseDate, 3, "https://musicbrainz.org/release-group/tz-daily-release", "musicbrainz",
		timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	// Control: with no prior run, a daily digest must be created.
	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}
	var control int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&control); err != nil {
		t.Fatal(err)
	}
	if control != 1 {
		t.Fatalf("control: a daily digest was not created at all (runs=%d)", control)
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM release_digest_runs WHERE user_id=?`, userID); err != nil {
		t.Fatal(err)
	}

	// Yesterday's run, recorded under the member's previous timezone and created
	// at their reminder time - the shape the scheduler actually writes.
	today := now.Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_digest_runs
		(user_id,frequency,period_start,timezone,title,body,release_count,status,created_at)
		VALUES(?,?,?,?,?,?,?, 'sent',?)`, userID, "daily",
		yesterday.Format("2006-01-02"), "America/New_York", "Yesterday", "body", 1,
		timeText(yesterday.Add(9*time.Hour))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=? AND created_at>=?`,
		userID, timeText(now.Add(-time.Hour))).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("today's daily digest runs=%d after a timezone change, want 1", runs)
	}
}

// TestSameTimezoneDailyDigestIsStillDeduplicated keeps the exclusion from
// reopening the duplicate it exists to prevent: a member who did NOT change
// timezone must still get exactly one digest per period.
func TestSameTimezoneDailyDigestIsStillDeduplicated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)
	userID, err := s.CreateUser(ctx, "tz-dedup@example.com", "hash", "member", "UTC", "tz-dedup")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "tz-dedup-artist", Name: "TZ Dedup Artist"})
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
	now := time.Now().UTC()
	if now.Hour() < 10 {
		now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	} else {
		now = now.Truncate(time.Minute)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "tz-dedup-release", artist.ID, "TZ Dedup Release", "EP", "[]",
		now.AddDate(0, 0, 1).Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/tz-dedup-release",
		"musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDueReleaseDigests(ctx, now); err != nil {
		t.Fatal(err)
	}
	// A second tick in the same period must not create another run.
	if _, err := s.QueueDueReleaseDigests(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, userID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("daily digest runs=%d in one period, want exactly 1", runs)
	}
}

// TestOneMemberDigestFailureDoesNotStarveTheRest is #277. The sweep isolated
// exactly one per-member failure - an unloadable timezone - and propagated every
// other one, abandoning all remaining members in the slice. The member list
// comes from a query with no ORDER BY, so SQLite returns it in a stable rowid
// order and the same members were consistently the ones that never got a digest.
func TestOneMemberDigestFailureDoesNotStarveTheRest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testStore(t)
	s.Reader.SetMaxOpenConns(1)

	now := time.Now().UTC()
	if now.Hour() < 10 {
		now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	} else {
		now = now.Truncate(time.Minute)
	}

	// The first member carries an unloadable timezone, which the sweep already
	// skips; the second must still be served. Both are created in order, so the
	// broken one is reached first.
	broken, err := s.CreateUser(ctx, "broken-tz@example.com", "hash", "member", "UTC", "broken-tz")
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := s.CreateUser(ctx, "healthy-tz@example.com", "hash", "member", "UTC", "healthy-tz")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "starve-artist", Name: "Starve Artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{broken, healthy} {
		if _, err := s.Follow(ctx, id, artist.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
			UserID: id, Albums: true, EPs: true, Singles: true, Announcements: true, ReleaseDay: true,
			DigestEnabled: true, DigestFrequency: "daily",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A malformed stored paused_until makes FollowNotificationRules fail for this
	// member alone - a failure that previously propagated and abandoned every
	// remaining member in the slice.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE follow_notification_rules SET paused_until='not-a-timestamp' WHERE user_id=?`, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "starve-release", artist.ID, "Starve Release", "EP", "[]",
		now.AddDate(0, 0, 1).Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/starve-release",
		"musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}

	// The failure is reported rather than swallowed...
	_, err = s.QueueDueReleaseDigests(ctx, now)
	if err == nil {
		t.Fatal("the per-member failure was swallowed entirely")
	}
	if !strings.Contains(err.Error(), "digest for user") {
		t.Fatalf("the error does not identify the failing member: %v", err)
	}
	// ...and every other member is still served.
	var runs int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM release_digest_runs WHERE user_id=?`, healthy).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("the healthy member's digest runs=%d after another member failed, want 1", runs)
	}
}
