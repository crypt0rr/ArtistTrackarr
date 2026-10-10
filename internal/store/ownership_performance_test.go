package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDashboardMaterializesFollowedArtistsAndCalendarUsesFullFollowPrimaryKey(t *testing.T) {
	s := testStore(t)
	today := "2026-10-10"
	from, to := "2026-01-01", "2027-01-01"

	dashboardUpcoming := `WITH ` + dashboardFollowedCandidateCTEs + `, candidates AS MATERIALIZED (
		SELECT rg.id,rg.first_release_date FROM candidate_release_ids candidate
		CROSS JOIN release_groups rg CROSS JOIN artists a
		WHERE rg.id=candidate.id AND a.id=rg.artist_id
		AND ` + calendarPreferredProvider + ` AND ` + dashboardDefinitelyFuture + `
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?
	)
	SELECT ` + releaseSelectColumns + ` FROM candidates candidate
	CROSS JOIN release_groups rg CROSS JOIN artists a
	WHERE rg.id=candidate.id AND a.id=rg.artist_id
	ORDER BY candidate.first_release_date ASC,candidate.id ASC`
	dashboardRecent := `WITH ` + dashboardFollowedCandidateCTEs + `, candidates AS MATERIALIZED (
		SELECT rg.id,CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END AS sort_date
		FROM candidate_release_ids candidate CROSS JOIN release_groups rg CROSS JOIN artists a
		WHERE rg.id=candidate.id AND a.id=rg.artist_id
		AND ` + calendarPreferredProvider + ` AND NOT COALESCE(` + dashboardDefinitelyFuture + `,0)
		ORDER BY sort_date DESC,rg.id DESC LIMIT ?
	)
	SELECT ` + releaseSelectColumns + ` FROM candidates candidate
	CROSS JOIN release_groups rg CROSS JOIN artists a
	WHERE rg.id=candidate.id AND a.id=rg.artist_id
	ORDER BY candidate.sort_date DESC,candidate.id DESC`
	calendar := `SELECT ` + releaseSelectColumns + `,
		EXISTS(SELECT 1 FROM notification_holds nh
			WHERE nh.user_id=? AND nh.release_group_id=rg.id AND nh.status='held')
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE ` + followedReleasePredicate("?") + ` AND ` + calendarPreferredProvider + `
		AND rg.date_precision=3 AND length(rg.first_release_date)=10
		AND rg.first_release_date BETWEEN ? AND ?
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ? OFFSET ?`

	assertDashboardCandidatePlan(t, s, "dashboard upcoming", dashboardUpcoming, int64(1), today, today, today, 20)
	assertDashboardCandidatePlan(t, s, "dashboard recent", dashboardRecent, int64(1), today, today, today, 20)
	assertCalendarFollowPrimaryKeyPlan(t, s, "calendar", calendar, int64(1), int64(1), from, to, 20, 0)
}

func explainQueryPlan(t *testing.T, s *Store, name, query string, args ...any) []string {
	t.Helper()
	rows, err := s.DB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %s: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan %s query plan: %v", name, err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s query plan: %v", name, err)
	}
	return details
}

func assertDashboardCandidatePlan(t *testing.T, s *Store, name, query string, args ...any) {
	t.Helper()
	details := explainQueryPlan(t, s, name, query, args...)
	plan := strings.Join(details, " | ")
	for _, expected := range []string{
		"MATERIALIZE followed_artists",
		"SEARCH follows USING COVERING INDEX sqlite_autoindex_follows_1 (user_id=?)",
		"SEARCH rg USING COVERING INDEX releases_artist (artist_id=?)",
		"SEARCH rc USING COVERING INDEX release_credits_artist_release (artist_id=?)",
	} {
		if !strings.Contains(plan, expected) {
			t.Fatalf("%s query did not use candidate-first step %q; plan: %s", name, expected, plan)
		}
	}
	t.Logf("%s query plan: %s", name, plan)
}

func assertCalendarFollowPrimaryKeyPlan(t *testing.T, s *Store, name, query string, args ...any) {
	t.Helper()
	details := explainQueryPlan(t, s, name, query, args...)
	plan := strings.Join(details, " | ")
	if !strings.Contains(plan, "SEARCH owner_follow USING COVERING INDEX sqlite_autoindex_follows_1 (user_id=? AND artist_id=?)") &&
		!strings.Contains(plan, "SEARCH owner_follow USING COVERING INDEX sqlite_autoindex_follows_1 (artist_id=? AND user_id=?)") {
		t.Fatalf("%s query did not search follows by both user_id and artist_id; plan: %s", name, plan)
	}
	t.Logf("%s query plan: %s", name, plan)
}

func TestDashboardAndCalendarKeepCreditedReleasesVisibleOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "ownership-query@example.test", "unused", "member", "UTC", "ownership-query")
	if err != nil {
		t.Fatal(err)
	}
	canonicalOnly, err := s.UpsertArtist(ctx, Artist{MBID: "ownership-query-canonical-only", Name: "Canonical Only"})
	if err != nil {
		t.Fatal(err)
	}
	canonicalFollowed, err := s.UpsertArtist(ctx, Artist{MBID: "ownership-query-canonical-followed", Name: "Canonical Followed"})
	if err != nil {
		t.Fatal(err)
	}
	credited, err := s.UpsertArtist(ctx, Artist{MBID: "ownership-query-credited", Name: "Credited Artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artistID := range []int64{canonicalFollowed.ID, credited.ID} {
		if _, err := s.Follow(ctx, userID, artistID); err != nil {
			t.Fatal(err)
		}
	}
	insertRelease := func(mbid string, canonicalArtistID int64, title, date string) int64 {
		t.Helper()
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, mbid, canonicalArtistID, title, "Album", "[]", date, 3,
			"https://musicbrainz.org/release-group/"+mbid, "spotify", stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		releaseID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_credits
			(release_group_id,artist_id,provider,provider_id,role,first_seen_at,last_seen_at)
			VALUES(?,?,'spotify',?,'guest',?,?)`, releaseID, credited.ID, mbid+"-credit", stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return releaseID
	}
	creditOnlyID := insertRelease("ownership-query-credit-only", canonicalOnly.ID, "Credit Only", "2026-11-01")
	bothPathsID := insertRelease("ownership-query-both-paths", canonicalFollowed.ID, "Both Paths", "2026-12-01")

	upcoming, _, err := s.DashboardReleases(ctx, userID, "2026-10-10", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(upcoming) != 2 || upcoming[0].ID != creditOnlyID || upcoming[1].ID != bothPathsID {
		t.Fatalf("dashboard upcoming release IDs=%v, want credit-only %d and both-paths %d once each", releaseIDs(upcoming), creditOnlyID, bothPathsID)
	}

	calendar, err := s.CalendarReleasesPage(ctx, userID, "2026-01-01", "2027-01-01", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calendar) != 2 || calendar[0].ID != creditOnlyID || calendar[1].ID != bothPathsID {
		t.Fatalf("calendar release IDs=%v, want credit-only %d and both-paths %d once each", calendarReleaseIDs(calendar), creditOnlyID, bothPathsID)
	}
}

func releaseIDs(releases []Release) []int64 {
	ids := make([]int64, 0, len(releases))
	for _, release := range releases {
		ids = append(ids, release.ID)
	}
	return ids
}

func calendarReleaseIDs(releases []CalendarRelease) []int64 {
	ids := make([]int64, 0, len(releases))
	for _, release := range releases {
		ids = append(ids, release.ID)
	}
	return ids
}
