package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

const artistProviderStatusColumns = `artist_id,provider,status,last_attempt_at,last_success_at,
	last_failure_at,next_check_at,release_count,last_error,updated_at`

// RecordArtistProviderStatus persists an individual artist/provider outcome.
// ReleaseCount may be set to -1 when an outcome did not return a release
// batch; in that case the previously observed count is retained.
func (s *Store) RecordArtistProviderStatus(ctx context.Context, status ArtistProviderStatus) error {
	provider := strings.ToLower(strings.TrimSpace(status.Provider))
	if status.ArtistID < 1 || (provider != "spotify" && provider != "itunes" && provider != "musicbrainz") {
		return errors.New("invalid artist provider status")
	}
	if status.Status == "" {
		status.Status = "pending"
	}
	if len(status.LastError) > 500 {
		status.LastError = status.LastError[:500]
	}
	updated := status.UpdatedAt
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO artist_provider_status
		(artist_id,provider,status,last_attempt_at,last_success_at,last_failure_at,next_check_at,
		 release_count,last_error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(artist_id,provider) DO UPDATE SET
		 status=excluded.status,
		 last_attempt_at=COALESCE(excluded.last_attempt_at,artist_provider_status.last_attempt_at),
		 last_success_at=COALESCE(excluded.last_success_at,artist_provider_status.last_success_at),
		 last_failure_at=COALESCE(excluded.last_failure_at,artist_provider_status.last_failure_at),
		 next_check_at=excluded.next_check_at,
		 release_count=CASE WHEN excluded.release_count>=0 THEN excluded.release_count ELSE artist_provider_status.release_count END,
		 last_error=excluded.last_error,
		 updated_at=excluded.updated_at`,
		status.ArtistID, provider, status.Status, nullableTime(status.LastAttemptAt),
		nullableTime(status.LastSuccessAt), nullableTime(status.LastFailureAt), nullableTime(status.NextCheckAt),
		status.ReleaseCount, status.LastError, timeText(updated))
	return err
}

func scanArtistProviderStatus(row interface{ Scan(...any) error }) (ArtistProviderStatus, error) {
	var status ArtistProviderStatus
	var attempt, success, failure, next, updated sql.NullString
	if err := row.Scan(&status.ArtistID, &status.Provider, &status.Status, &attempt, &success, &failure,
		&next, &status.ReleaseCount, &status.LastError, &updated); err != nil {
		return ArtistProviderStatus{}, err
	}
	status.LastAttemptAt = parseNullableStatusTime(attempt)
	status.LastSuccessAt = parseNullableStatusTime(success)
	status.LastFailureAt = parseNullableStatusTime(failure)
	status.NextCheckAt = parseNullableStatusTime(next)
	if updated.Valid {
		status.UpdatedAt, _ = parseTime(updated.String)
	}
	return status, nil
}

func parseNullableStatusTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil
	}
	return &parsed
}

type coverageReleaseStats struct {
	ReleaseCount         int
	ConfirmedReleases    int
	SingleSourceReleases int
	FallbackReleases     int
	LastObservedAt       *time.Time
}

// FollowedArtistCoveragePage returns a bounded, owner-scoped view suitable
// for the Trust Center. Provider status and release provenance are loaded in
// batches so large watchlists do not create one query per artist.
func (s *Store) FollowedArtistCoveragePage(ctx context.Context, userID int64, limit, offset int) ([]ArtistCoverage, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	artists, err := s.FollowedArtistsFilteredPage(ctx, userID, "", "", "", limit, offset)
	if err != nil {
		return nil, err
	}
	return s.coverageForArtists(ctx, artists)
}

func (s *Store) CoverageSummary(ctx context.Context, userID int64) (CoverageSummary, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,
		a.country,a.disambiguation,a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,
		a.spotify_next_check_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=?`, userID)
	if err != nil {
		return CoverageSummary{}, err
	}
	defer func() { _ = rows.Close() }()
	var artists []Artist
	for rows.Next() {
		var artist Artist
		var sid, surl, image, checked, spotifyNext, baseline sql.NullString
		if err := rows.Scan(&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type,
			&artist.Country, &artist.Disambiguation, &sid, &surl, &image, &checked, &spotifyNext, &baseline); err != nil {
			return CoverageSummary{}, err
		}
		artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
		if checked.Valid {
			value, _ := parseTime(checked.String)
			artist.LastCheckedAt = &value
		}
		if spotifyNext.Valid {
			value, _ := parseTime(spotifyNext.String)
			artist.SpotifyNextCheckAt = &value
		}
		artist.BaselineSynced = baseline.Valid
		artists = append(artists, artist)
	}
	if err := rows.Err(); err != nil {
		return CoverageSummary{}, err
	}
	coverage, err := s.coverageForArtists(ctx, artists)
	if err != nil {
		return CoverageSummary{}, err
	}
	var summary CoverageSummary
	summary.Artists = len(coverage)
	for _, item := range coverage {
		summary.ConfirmedReleases += item.ConfirmedReleases
		summary.SingleSourceReleases += item.SingleSourceReleases
		summary.FallbackReleases += item.FallbackReleases
		switch item.OverallStatus {
		case "fresh", "confirmed":
			summary.FreshArtists++
		case "attention":
			summary.AttentionArtists++
		default:
			summary.PendingArtists++
		}
	}
	return summary, nil
}

func (s *Store) coverageForArtists(ctx context.Context, artists []Artist) ([]ArtistCoverage, error) {
	if len(artists) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}
	statuses, err := s.artistProviderStatuses(ctx, ids)
	if err != nil {
		return nil, err
	}
	releaseStats, err := s.coverageReleaseStats(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]ArtistCoverage, 0, len(artists))
	for _, artist := range artists {
		item := ArtistCoverage{Artist: artist, ProviderStatuses: statuses[artist.ID]}
		ProviderStatusOrder(item.ProviderStatuses)
		if stats, ok := releaseStats[artist.ID]; ok {
			item.ReleaseCount = stats.ReleaseCount
			item.ConfirmedReleases = stats.ConfirmedReleases
			item.SingleSourceReleases = stats.SingleSourceReleases
			item.FallbackReleases = stats.FallbackReleases
			item.LastObservedAt = stats.LastObservedAt
		}
		item.NextCheckAt = earliestCoverageCheck(item.ProviderStatuses)
		item.OverallStatus = coverageStatus(item)
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) artistProviderStatuses(ctx context.Context, ids []int64) (map[int64][]ArtistProviderStatus, error) {
	result := make(map[int64][]ArtistProviderStatus, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := s.readerDB().QueryContext(ctx, `SELECT `+artistProviderStatusColumns+`
			FROM artist_provider_status WHERE artist_id IN (`+placeholders+`) ORDER BY artist_id,provider`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			status, err := scanArtistProviderStatus(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[status.ArtistID] = append(result[status.ArtistID], status)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return result, nil
}

func (s *Store) coverageReleaseStats(ctx context.Context, ids []int64) (map[int64]coverageReleaseStats, error) {
	result := make(map[int64]coverageReleaseStats, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := s.readerDB().QueryContext(ctx, `SELECT artist_id,COUNT(*),
			SUM(CASE WHEN provider_count>=2 THEN 1 ELSE 0 END),
			SUM(CASE WHEN provider_count=1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN provider_count=1 AND itunes_provider=1 THEN 1 ELSE 0 END),MAX(last_observed_at)
			FROM (
				SELECT rg.id,rg.artist_id,COUNT(DISTINCT po.provider) AS provider_count,
					MAX(po.observed_at) AS last_observed_at,
					MAX(CASE WHEN po.provider='itunes' THEN 1 ELSE 0 END) AS itunes_provider
				FROM release_groups rg LEFT JOIN provider_observations po ON po.release_group_id=rg.id
				WHERE rg.artist_id IN (`+placeholders+`)
				GROUP BY rg.id,rg.artist_id
			) grouped GROUP BY artist_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var artistID int64
			var count, confirmed, singleSource, fallback int
			var observed sql.NullString
			if err := rows.Scan(&artistID, &count, &confirmed, &singleSource, &fallback, &observed); err != nil {
				_ = rows.Close()
				return nil, err
			}
			stats := coverageReleaseStats{ReleaseCount: count, ConfirmedReleases: confirmed,
				SingleSourceReleases: singleSource, FallbackReleases: fallback}
			stats.LastObservedAt = parseNullableStatusTime(observed)
			result[artistID] = stats
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return result, nil
}

func earliestCoverageCheck(statuses []ArtistProviderStatus) *time.Time {
	var earliest *time.Time
	for _, status := range statuses {
		if status.NextCheckAt == nil {
			continue
		}
		if earliest == nil || status.NextCheckAt.Before(*earliest) {
			value := *status.NextCheckAt
			earliest = &value
		}
	}
	return earliest
}

func coverageStatus(item ArtistCoverage) string {
	if len(item.ProviderStatuses) == 0 {
		switch {
		case item.ConfirmedReleases > 0:
			return "confirmed"
		case item.FallbackReleases > 0:
			return "fallback"
		case item.ReleaseCount > 0:
			return "fresh"
		default:
			return "pending"
		}
	}
	for _, status := range item.ProviderStatuses {
		if status.Status == "failed" || status.Status == "cooldown" {
			return "attention"
		}
	}
	if item.ConfirmedReleases > 0 {
		return "confirmed"
	}
	if item.FallbackReleases > 0 {
		return "fallback"
	}
	for _, status := range item.ProviderStatuses {
		if status.Status == "healthy" {
			return "fresh"
		}
	}
	return "pending"
}

// ProviderStatusOrder keeps the Trust Center stable even when providers are
// first seen in a different order after a fallback.
func ProviderStatusOrder(statuses []ArtistProviderStatus) {
	order := map[string]int{"spotify": 0, "itunes": 1, "musicbrainz": 2}
	sort.SliceStable(statuses, func(i, j int) bool {
		return order[statuses[i].Provider] < order[statuses[j].Provider]
	})
}
