package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAdminDeliveryHistoryExportKeysetPaginationIsStable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "export-audit@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "export-audit-artist", Name: "Export Audit Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "Audit", "ntfy", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations=%#v err=%v", destinations, err)
	}
	destinationID := destinations[0].ID
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := range 24 {
		releaseResult, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("export-audit-release-%02d", i), artist.ID,
			fmt.Sprintf("Export Release %02d", i), "Album", "[]", "2026-08-01", 3,
			"https://musicbrainz.test/export", timeText(base), timeText(base))
		if err != nil {
			t.Fatal(err)
		}
		releaseID, _ := releaseResult.LastInsertId()
		eventResult, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
			(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, userID, releaseID,
			"announcement", fmt.Sprintf("Export Event %02d", i), "Body", timeText(base.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		eventID, _ := eventResult.LastInsertId()
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO deliveries
			(event_id,destination_id,status,attempts,next_attempt_at,last_error) VALUES(?,?,?,?,?,?)`, eventID,
			destinationID, "sent", 1, timeText(base), ""); err != nil {
			t.Fatal(err)
		}
	}

	var cursor *AdminDeliveryExportCursor
	seen := make(map[int64]bool)
	seenNoDelivery := 0
	for page := 0; ; page++ {
		rows, next, err := s.AdminDeliveryHistoryExportPage(ctx, 5, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.DeliveryID == 0 {
				seenNoDelivery++
				continue
			}
			if seen[row.DeliveryID] {
				t.Fatalf("duplicate delivery id on page %d: %#v", page, row)
			}
			seen[row.DeliveryID] = true
		}
		if page == 0 {
			// Insert an older row after the first page. It belongs after the
			// cursor and must still be included without shifting prior rows.
			releaseResult, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
				(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, "export-audit-late-release", artist.ID, "Late Export Release", "Album", "[]", "2026-08-01", 3,
				"https://musicbrainz.test/export-late", timeText(base), timeText(base))
			if err != nil {
				t.Fatal(err)
			}
			releaseID, _ := releaseResult.LastInsertId()
			if _, err := s.DB.ExecContext(ctx, `INSERT INTO notification_events
				(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`, userID, releaseID,
				"announcement", "Late Export Event", "Body", timeText(base.Add(-time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		if len(rows) == 0 {
			if next != nil {
				t.Fatalf("empty page returned cursor=%#v", next)
			}
			break
		}
		if next == nil {
			t.Fatalf("non-empty page returned no cursor")
		}
		cursor = next
	}
	if len(seen) != 24 || seenNoDelivery != 1 {
		t.Fatalf("export returned %d delivery rows and %d no-delivery rows, want 24 and 1", len(seen), seenNoDelivery)
	}
}

func TestFollowedArtistsExportKeysetPaginationIsStable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "export-artists@example.com", "unused", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 37 {
		artist, err := s.UpsertArtist(ctx, Artist{
			MBID: fmt.Sprintf("export-artist-%02d", i), Name: fmt.Sprintf("Artist %02d", i), SortName: fmt.Sprintf("Artist %02d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
			t.Fatal(err)
		}
	}

	var cursor *ArtistExportCursor
	seen := make(map[int64]bool)
	lastName := ""
	for {
		rows, next, err := s.FollowedArtistsExportPage(ctx, userID, 7, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, artist := range rows {
			if seen[artist.ID] {
				t.Fatalf("artist %d repeated", artist.ID)
			}
			seen[artist.ID] = true
			name := strings.ToLower(strings.TrimSpace(artist.Name))
			if lastName != "" && name < lastName {
				t.Fatalf("artist export order regressed from %q to %q", lastName, name)
			}
			lastName = name
		}
		if len(rows) == 0 {
			if next != nil {
				t.Fatalf("empty artist page returned cursor=%#v", next)
			}
			break
		}
		if next == nil {
			t.Fatalf("non-empty artist page returned no cursor")
		}
		cursor = next
	}
	if len(seen) != 37 {
		t.Fatalf("artist export returned %d rows, want 37", len(seen))
	}
}
