package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkDashboardReleases(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	database, err := Open(filepath.Join(b.TempDir(), "dashboard-releases-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	userID, err := database.CreateUser(ctx, "dashboard-benchmark@example.test", "unused", "member", "UTC", "dashboard-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	artistIDs, err := seedDashboardBenchmarkData(ctx, b, database, userID)
	if err != nil {
		b.Fatal(err)
	}
	_ = artistIDs
	today := time.Now().UTC().Format("2006-01-02")
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := database.DashboardReleases(ctx, userID, today, 20); err != nil {
			b.Fatal(err)
		}
	}
}

func seedDashboardBenchmarkData(ctx context.Context, b *testing.B, database *Store, userID int64) ([]int64, error) {
	const followedArtists = 300
	const otherArtists = 300
	const releaseGroups = 10000
	const unownedGroups = 6000
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	future := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	tx, err := database.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	artistIDs := make([]int64, 0, followedArtists+otherArtists)
	if err := func() (resultErr error) {
		artistStmt, err := tx.PrepareContext(ctx, `INSERT INTO artists(mbid,name,created_at,updated_at) VALUES(?,?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, artistStmt.Close()) }()
		for i := 0; i < followedArtists+otherArtists; i++ {
			result, err := artistStmt.ExecContext(ctx, fmt.Sprintf("dashboard-benchmark-artist-%03d", i), fmt.Sprintf("Benchmark Artist %03d", i), stamp, stamp)
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			artistIDs = append(artistIDs, id)
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	if err := func() (resultErr error) {
		followStmt, err := tx.PrepareContext(ctx, `INSERT INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, followStmt.Close()) }()
		for i := 0; i < followedArtists; i++ {
			if _, err := followStmt.ExecContext(ctx, userID, artistIDs[i], stamp); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	if err := func() (resultErr error) {
		releaseStmt, err := tx.PrepareContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,'Album','[]',?,3,?,'spotify',?,?)`)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, releaseStmt.Close()) }()
		creditStmt, err := tx.PrepareContext(ctx, `INSERT INTO release_credits
			(release_group_id,artist_id,provider,provider_id,role,first_seen_at,last_seen_at)
			VALUES(?,?,'musicbrainz',?,'guest',?,?)`)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, creditStmt.Close()) }()
		for i := 0; i < releaseGroups; i++ {
			artistIndex := i % otherArtists
			creditIndex := (i*7 + 1) % otherArtists
			if i >= unownedGroups {
				artistIndex = (i - unownedGroups) % followedArtists
				creditIndex = (artistIndex + 1) % followedArtists
			}
			mbid := fmt.Sprintf("dashboard-benchmark-release-%05d", i)
			result, err := releaseStmt.ExecContext(ctx, mbid, artistIDs[artistIndex], fmt.Sprintf("Benchmark release %05d", i), future,
				"https://musicbrainz.org/release-group/"+mbid, stamp, stamp)
			if err != nil {
				return err
			}
			releaseID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			creditArtistIndex := creditIndex
			if i < unownedGroups {
				creditArtistIndex += followedArtists
			}
			creditArtistID := artistIDs[creditArtistIndex]
			if _, err := creditStmt.ExecContext(ctx, releaseID, creditArtistID, mbid+"-credit", stamp, stamp); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return artistIDs, nil
}
