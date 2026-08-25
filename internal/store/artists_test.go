package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestArtworkDueQueryUsesItsIndex is #282. The due predicate was a disjunction,
// (col IS NULL OR col <= ?), which gives an index keyed on that bare column no
// usable range constraint - EXPLAIN QUERY PLAN reported "SCAN rg". The backfill
// runs on the 60-second tick, so that full scan executed 1,440 times a day on
// the four-connection reader pool, in steady state finding nothing.
//
// The index is now keyed on the same COALESCE expression the query and its
// ORDER BY already used, so one range constraint replaces the disjunction.
func TestArtworkDueQueryUsesItsIndex(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "artwork-plan", Name: "Artwork Plan"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 500; i++ {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,source,first_observed_at,updated_at,itunes_id,itunes_artwork_url)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("aw-%d", i), artist.ID, "T", "Album", "[]", "2026-01-01", 3, "u", "itunes",
			timeText(now), timeText(now), fmt.Sprintf("it-%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	// Steady state: almost nothing is due.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE release_groups SET itunes_artwork_next_check_at=? WHERE id > 2`,
		timeText(now.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	rows, err := s.DB.QueryContext(ctx, `EXPLAIN QUERY PLAN `+dueITunesArtworkQuery, timeText(now))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	plan := ""
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "release_groups_itunes_artwork_due") {
		t.Fatalf("the artwork due query does not use its index:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN rg") {
		t.Fatalf("the artwork due query still full-scans release_groups:\n%s", plan)
	}
}
