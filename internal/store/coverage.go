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
// providerNotContacted reports whether a status means the provider was
// deliberately not called on this pass, so the observation carries no fresh
// evidence and must not overwrite the evidence already stored.
//
// The set is written out because the gate previously read
// `status == "standby" || status == "skipped"`, and those two values are not
// the ones the scheduler emits: internal/jobs/providers.go produces standby,
// deferred, not_configured and cooldown, and emits "skipped" nowhere at all.
// So deferred, not_configured and cooldown took the destructive branch and
// wrote last_error=” over a real provider failure on the artist's next tick.
//
// "skipped" is retained only because internal/web/core.go still renders it as
// "Standby" for any legacy row; nothing writes it.
//
// Everything absent from this list - healthy, failed, degraded, not_found,
// ambiguous - is the outcome of an actual call and must overwrite.
func providerNotContacted(status string) bool {
	switch status {
	case "standby", "skipped", "deferred", "not_configured", "cooldown":
		return true
	default:
		return false
	}
}

func (s *Store) RecordArtistProviderStatus(ctx context.Context, status ArtistProviderStatus) error {
	provider := strings.ToLower(strings.TrimSpace(status.Provider))
	status.Status = strings.ToLower(strings.TrimSpace(status.Status))
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
	// A negative ReleaseCount is the caller's "no batch this time, keep whatever
	// was observed before" sentinel. It is decoded only by the ON CONFLICT
	// clauses, so on the FIRST row for an (artist, provider) pair it used to be
	// written into the column verbatim - and every later upsert then took the
	// preserve branch and kept -1 forever, until that provider first returned a
	// healthy batch. On a deployment with no Spotify credentials that is never,
	// so the Trust Center read "-1 releases returned" permanently.
	//
	// Bind the stored value clamped, and carry the sentinel separately for the
	// branch that needs it.
	storedCount := status.ReleaseCount
	if storedCount < 0 {
		storedCount = 0
	}
	preservePreviousCount := 0
	if status.ReleaseCount < 0 {
		preservePreviousCount = 1
	}
	// A not-contacted result must update the current per-artist state without
	// erasing the last meaningful failure, success, error, or retry deadline.
	// Replacing a previous failure with an empty status would make the Trust
	// Center lose the evidence needed to explain recovery.
	if providerNotContacted(status.Status) {
		_, err := s.execWriteContext(ctx, `INSERT INTO artist_provider_status
			(artist_id,provider,status,last_attempt_at,last_success_at,last_failure_at,next_check_at,
			 release_count,last_error,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(artist_id,provider) DO UPDATE SET
			 status=excluded.status,
			 last_attempt_at=COALESCE(excluded.last_attempt_at,artist_provider_status.last_attempt_at),
			 last_success_at=COALESCE(excluded.last_success_at,artist_provider_status.last_success_at),
			 last_failure_at=COALESCE(excluded.last_failure_at,artist_provider_status.last_failure_at),
			 next_check_at=COALESCE(excluded.next_check_at,artist_provider_status.next_check_at),
			release_count=artist_provider_status.release_count,
			 last_error=CASE WHEN excluded.last_error<>'' THEN excluded.last_error ELSE artist_provider_status.last_error END,
			 updated_at=excluded.updated_at`,
			status.ArtistID, provider, status.Status, nullableTime(status.LastAttemptAt),
			nullableTime(status.LastSuccessAt), nullableTime(status.LastFailureAt), nullableTime(status.NextCheckAt),
			storedCount, status.LastError, timeText(updated))
		return err
	}
	_, err := s.execWriteContext(ctx, `INSERT INTO artist_provider_status
		(artist_id,provider,status,last_attempt_at,last_success_at,last_failure_at,next_check_at,
		 release_count,last_error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(artist_id,provider) DO UPDATE SET
		 status=excluded.status,
		 last_attempt_at=COALESCE(excluded.last_attempt_at,artist_provider_status.last_attempt_at),
		 last_success_at=COALESCE(excluded.last_success_at,artist_provider_status.last_success_at),
		 last_failure_at=COALESCE(excluded.last_failure_at,artist_provider_status.last_failure_at),
		 next_check_at=excluded.next_check_at,
		 release_count=CASE WHEN ?=1 THEN artist_provider_status.release_count ELSE excluded.release_count END,
		 last_error=excluded.last_error,
		 updated_at=excluded.updated_at`,
		status.ArtistID, provider, status.Status, nullableTime(status.LastAttemptAt),
		nullableTime(status.LastSuccessAt), nullableTime(status.LastFailureAt), nullableTime(status.NextCheckAt),
		// Positional binds: the CASE placeholder appears after every VALUES
		// placeholder in the statement text, so it is bound last.
		storedCount, status.LastError, timeText(updated), preservePreviousCount)
	return err
}

func scanArtistProviderStatus(row interface{ Scan(...any) error }) (ArtistProviderStatus, error) {
	var status ArtistProviderStatus
	var attempt, success, failure, next, updated sql.NullString
	if err := row.Scan(&status.ArtistID, &status.Provider, &status.Status, &attempt, &success, &failure,
		&next, &status.ReleaseCount, &status.LastError, &updated); err != nil {
		return ArtistProviderStatus{}, err
	}
	var parseErr error
	if status.LastAttemptAt, parseErr = parseStoredNullableTime(attempt, "artist provider last_attempt_at"); parseErr != nil {
		return ArtistProviderStatus{}, parseErr
	}
	if status.LastSuccessAt, parseErr = parseStoredNullableTime(success, "artist provider last_success_at"); parseErr != nil {
		return ArtistProviderStatus{}, parseErr
	}
	if status.LastFailureAt, parseErr = parseStoredNullableTime(failure, "artist provider last_failure_at"); parseErr != nil {
		return ArtistProviderStatus{}, parseErr
	}
	if status.NextCheckAt, parseErr = parseStoredNullableTime(next, "artist provider next_check_at"); parseErr != nil {
		return ArtistProviderStatus{}, parseErr
	}
	if updated.Valid {
		status.UpdatedAt, parseErr = parseStoredTime(updated.String, "artist provider updated_at")
		if parseErr != nil {
			return ArtistProviderStatus{}, parseErr
		}
	}
	return status, nil
}

func parseNullableStatusTime(value sql.NullString, field string) (*time.Time, error) {
	return parseStoredNullableTime(value, field)
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
	summary, _, err := s.CoverageOverview(ctx, userID, 0)
	return summary, err
}

// CoverageOverview returns the summary used by the dashboard and Trust
// Center in one batched projection. This avoids repeating the followed-artist,
// provider-status, and release-stat queries when both views are rendered.
func (s *Store) CoverageOverview(ctx context.Context, userID int64, limit int) (CoverageSummary, AssuranceSummary, error) {
	artists, err := s.followedArtistsForCoverage(ctx, userID)
	if err != nil {
		return CoverageSummary{}, AssuranceSummary{}, err
	}
	coverage, err := s.coverageForArtists(ctx, artists)
	if err != nil {
		return CoverageSummary{}, AssuranceSummary{}, err
	}
	return summarizeCoverage(coverage), summarizeAssurance(coverage, limit), nil
}

// WatchlistAssurance returns the complete owner-scoped assurance counts and a
// small severity-ranked list for dashboard use. The underlying projections
// are batched, matching the Trust Center's query behavior.
func (s *Store) WatchlistAssurance(ctx context.Context, userID int64, limit int) (AssuranceSummary, error) {
	_, summary, err := s.CoverageOverview(ctx, userID, limit)
	return summary, err
}

func summarizeCoverage(coverage []ArtistCoverage) CoverageSummary {
	summary := CoverageSummary{Artists: len(coverage)}
	for _, item := range coverage {
		summary.ConfirmedReleases += item.ConfirmedReleases
		summary.SingleSourceReleases += item.SingleSourceReleases
		summary.FallbackReleases += item.FallbackReleases
		switch item.AssuranceStatus {
		case "healthy":
			summary.HealthyArtists++
		case "delayed":
			summary.DelayedArtists++
		case "degraded":
			summary.DegradedArtists++
		default:
			summary.PendingAssuranceArtists++
		}
		switch item.OverallStatus {
		case "fresh", "confirmed":
			summary.FreshArtists++
		case "attention":
			summary.AttentionArtists++
		default:
			summary.PendingArtists++
		}
	}
	return summary
}

func summarizeAssurance(coverage []ArtistCoverage, limit int) AssuranceSummary {
	if limit < 1 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	summary := AssuranceSummary{Total: len(coverage)}
	for _, item := range coverage {
		switch item.AssuranceStatus {
		case "healthy":
			summary.Healthy++
		case "delayed":
			summary.Delayed++
		case "degraded":
			summary.Degraded++
		default:
			summary.Pending++
		}
	}
	sort.SliceStable(coverage, func(i, j int) bool {
		left, right := assuranceSeverity(coverage[i].AssuranceStatus), assuranceSeverity(coverage[j].AssuranceStatus)
		if left != right {
			return left < right
		}
		return strings.ToLower(coverage[i].Artist.Name) < strings.ToLower(coverage[j].Artist.Name)
	})
	for _, item := range coverage {
		if item.AssuranceStatus == "healthy" {
			break
		}
		if len(summary.AtRisk) == limit {
			break
		}
		summary.AtRisk = append(summary.AtRisk, item)
	}
	return summary
}

func (s *Store) followedArtistsForCoverage(ctx context.Context, userID int64) ([]Artist, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,
		a.country,a.disambiguation,a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,
		a.spotify_next_check_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var artists []Artist
	for rows.Next() {
		var artist Artist
		var sid, surl, image, checked, spotifyNext, baseline sql.NullString
		if err := rows.Scan(&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type,
			&artist.Country, &artist.Disambiguation, &sid, &surl, &image, &checked, &spotifyNext, &baseline); err != nil {
			return nil, err
		}
		artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
		var parseErr error
		if artist.LastCheckedAt, parseErr = parseStoredNullableTime(checked, "coverage artist last_checked_at"); parseErr != nil {
			return nil, parseErr
		}
		if artist.SpotifyNextCheckAt, parseErr = parseStoredNullableTime(spotifyNext, "coverage artist spotify_next_check_at"); parseErr != nil {
			return nil, parseErr
		}
		artist.BaselineSynced = baseline.Valid
		artists = append(artists, artist)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return artists, nil
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
	now := time.Now().UTC()
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
		item.AssuranceStatus, item.AssuranceReason, item.LastSuccessfulProvider = coverageAssurance(item, now)
		result = append(result, item)
	}
	return result, nil
}

const coverageFreshnessWindow = 48 * time.Hour

func coverageAssurance(item ArtistCoverage, now time.Time) (string, string, string) {
	var latestSuccess *time.Time
	provider := ""
	for _, status := range item.ProviderStatuses {
		if status.LastSuccessAt != nil && (latestSuccess == nil || status.LastSuccessAt.After(*latestSuccess)) {
			value := *status.LastSuccessAt
			latestSuccess = &value
			provider = status.Provider
		}
	}
	for _, status := range item.ProviderStatuses {
		if status.Status == "failed" || status.Status == "cooldown" || status.Status == "degraded" {
			return "degraded", "A provider is unavailable or rate-limited; fallback data may still be available.", provider
		}
	}
	if latestSuccess == nil {
		if item.LastObservedAt != nil && now.Sub(*item.LastObservedAt) <= coverageFreshnessWindow {
			return "healthy", "Recent release data is available.", provider
		}
		if item.ReleaseCount > 0 {
			return "delayed", "Release history exists, but a recent provider check is not recorded.", provider
		}
		return "pending", "Waiting for the first successful provider observation.", provider
	}
	if now.Sub(*latestSuccess) > coverageFreshnessWindow {
		return "delayed", "The latest successful provider check is more than 48 hours old.", provider
	}
	return "healthy", "Recent provider data is available.", provider
}

func assuranceSeverity(status string) int {
	switch status {
	case "degraded":
		return 0
	case "delayed":
		return 1
	case "pending":
		return 2
	default:
		return 3
	}
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
		if err := func() error {
			rows, err := s.readerDB().QueryContext(ctx, `SELECT `+artistProviderStatusColumns+`
				FROM artist_provider_status WHERE artist_id IN (`+placeholders+`) ORDER BY artist_id,provider`, args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				status, err := scanArtistProviderStatus(rows)
				if err != nil {
					return err
				}
				result[status.ArtistID] = append(result[status.ArtistID], status)
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
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
		queryArgs := append(append([]any(nil), args...), args...)
		if err := func() error {
			rows, err := s.readerDB().QueryContext(ctx, `SELECT followed_artist_id,COUNT(*),
				SUM(CASE WHEN provider_count>=2 THEN 1 ELSE 0 END),
				SUM(CASE WHEN provider_count=1 THEN 1 ELSE 0 END),
				SUM(CASE WHEN provider_count=1 AND itunes_provider=1 THEN 1 ELSE 0 END),MAX(last_observed_at)
				FROM (
					SELECT rg.id,rg.artist_id AS followed_artist_id,COUNT(DISTINCT po.provider) AS provider_count,
						MAX(po.observed_at) AS last_observed_at,
						MAX(CASE WHEN po.provider='itunes' THEN 1 ELSE 0 END) AS itunes_provider
					FROM release_groups rg LEFT JOIN provider_observations po ON po.release_group_id=rg.id
					WHERE rg.artist_id IN (`+placeholders+`)
					GROUP BY rg.id,rg.artist_id
					UNION
					SELECT rg.id,rc.artist_id AS followed_artist_id,COUNT(DISTINCT po.provider) AS provider_count,
						MAX(po.observed_at) AS last_observed_at,
						MAX(CASE WHEN po.provider='itunes' THEN 1 ELSE 0 END) AS itunes_provider
					FROM release_groups rg JOIN release_credits rc ON rc.release_group_id=rg.id
					LEFT JOIN provider_observations po ON po.release_group_id=rg.id
					WHERE rc.artist_id IN (`+placeholders+`)
					GROUP BY rg.id,rc.artist_id
				) grouped GROUP BY followed_artist_id`, queryArgs...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var artistID int64
				var count, confirmed, singleSource, fallback int
				var observed sql.NullString
				if err := rows.Scan(&artistID, &count, &confirmed, &singleSource, &fallback, &observed); err != nil {
					return err
				}
				stats := coverageReleaseStats{ReleaseCount: count, ConfirmedReleases: confirmed,
					SingleSourceReleases: singleSource, FallbackReleases: fallback}
				stats.LastObservedAt, err = parseNullableStatusTime(observed, "coverage last_observed_at")
				if err != nil {
					return err
				}
				result[artistID] = stats
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
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
		if status.Status == "failed" || status.Status == "cooldown" || status.Status == "degraded" {
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
