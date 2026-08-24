package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Unresolved provider identities are member-owned work. Keep the active set
// bounded so a repeated search/import cannot create an unbounded retry and
// review queue. Existing rows remain available until the member resolves or
// cancels them.
const maxArtistResolutionsPerUser = 100

// Keep one member's retry/review work from consuming the entire scheduler
// batch. The runner can still process more than this across household users,
// while a single noisy import remains bounded and fair.
const maxArtistResolutionBatchPerUser = 4

// artistSyncAdmissionPredicate is shared by the scheduler and operational
// diagnostics. Keep the identity terminal-state guard outside the normal and
// Spotify due branches so an artist cannot bypass it through SQL operator
// precedence when only its Spotify check is due.
func artistSyncAdmissionPredicate(alias string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		alias = "a"
	}
	return `NOT EXISTS (SELECT 1 FROM artist_identity_status ais
		WHERE ais.artist_id=` + alias + `.id AND ais.status='unresolvable')`
}

// UpsertArtist writes the household-shared artist row for one MusicBrainz ID.
// A stored Spotify identity is never replaced: the interactive follow path
// re-runs a Spotify name search on every follow and Enrich takes the first
// exact-name match, which is frequently a different band for a homonym. Letting
// that guess win would repoint every other member's release feed, artwork, and
// genres for an artist they already follow. A missing identity is still filled
// in, and the URL and image are only taken from an incoming record that agrees
// with the identity being kept. The CSV import path was hardened the same way.
func (s *Store) UpsertArtist(ctx context.Context, a Artist) (Artist, error) {
	now := nowText()
	_, err := s.execWriteContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(artists.spotify_id,excluded.spotify_id),
		spotify_url=CASE WHEN artists.spotify_id IS NULL OR artists.spotify_id=excluded.spotify_id
			THEN COALESCE(excluded.spotify_url,artists.spotify_url) ELSE artists.spotify_url END,
		spotify_image_url=CASE WHEN artists.spotify_id IS NULL OR artists.spotify_id=excluded.spotify_id
			THEN COALESCE(excluded.spotify_image_url,artists.spotify_image_url) ELSE artists.spotify_image_url END,
		updated_at=excluded.updated_at`,
		a.MBID, a.Name, a.SortName, a.Type, a.Country, a.Disambiguation,
		nullString(a.SpotifyID), nullString(a.SpotifyURL), nullString(a.SpotifyImageURL), now, now)
	if err != nil {
		return Artist{}, err
	}
	// Artists admitted from an exact MusicBrainz result are already canonical.
	// Keep a status row for them so imported identities can be distinguished
	// without expanding every artist projection with verification columns.
	if _, err := s.execWriteContext(ctx, `INSERT OR IGNORE INTO artist_identity_status
		(artist_id,status,attempts,next_check_at,last_error,updated_at)
		SELECT id,'verified',0,NULL,'',? FROM artists WHERE mbid=?`, now, a.MBID); err != nil {
		return Artist{}, err
	}
	stored, err := s.ArtistByMBID(ctx, a.MBID)
	if err != nil {
		return Artist{}, err
	}
	if len(a.Genres) > 0 {
		if err := s.replaceArtistGenres(ctx, stored.ID, a.Genres, "musicbrainz"); err != nil {
			return Artist{}, err
		}
	}
	stored.Genres = append([]string(nil), a.Genres...)
	return stored, nil
}
func (s *Store) ArtistByMBID(ctx context.Context, mbid string) (Artist, error) {
	var a Artist
	var sid, surl, image sql.NullString
	var checked sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,last_checked_at FROM artists WHERE mbid=?`, mbid).Scan(
		&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
		&sid, &surl, &image, &checked)
	if err != nil {
		return a, err
	}
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
	if checked.Valid {
		t, parseErr := parseStoredTime(checked.String, "artist last_checked_at")
		if parseErr != nil {
			return a, parseErr
		}
		a.LastCheckedAt = &t
	}
	return a, nil
}
func (s *Store) Follow(ctx context.Context, userID, artistID int64) (bool, error) {
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (bool, error) {
		now := nowText()
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`,
			userID, artistID, now)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if n > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
				CASE WHEN spotify_id IS NOT NULL AND (spotify_next_check_at IS NULL OR spotify_next_check_at>?)
				THEN ? ELSE spotify_next_check_at END WHERE id=?`, now, now, now, artistID); err != nil {
				return false, err
			}
		}
		// Keep the policy row present even for a legacy follow that predates the
		// policy migration. INSERT OR IGNORE also avoids resetting an existing
		// policy.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follow_notification_rules
			(user_id,artist_id,delivery_mode,include_primary,include_featured,albums,eps,singles,compilations,announcements,release_day,updated_at)
			VALUES(?,?, 'inherit',1,1,1,1,1,1,1,1,?)`, userID, artistID, now); err != nil {
			return false, err
		}
		// A new follow can outrank whichever follow currently governs an
		// already-queued delivery for the same release: a fresh rule is
		// "inherit", which is full priority, and on the canonical artist it also
		// wins admission's tie-break. Re-derive so the row lands on the new
		// governor's schedule.
		if err := realignQueuedDeliveriesTx(ctx, tx, userID, []int64{artistID}, time.Now().UTC()); err != nil {
			return false, err
		}
		return n > 0, nil
	})
}

// Unfollow removes a follow and cleans up the work it was the reason for.
//
// Removing an artist from the watchlist previously left already-queued
// deliveries in place, so an alert for an artist the member had removed still
// arrived - potentially months later, since a deferred or backed-off row can sit
// far in the future - with the artist absent from every page that would explain
// it.
func (s *Store) Unfollow(ctx context.Context, userID, artistID int64) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE user_id=? AND artist_id=?`, userID, artistID)
		if err != nil {
			return err
		}
		if err := changedOrNotFound(result, nil); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM follow_notification_rules WHERE user_id=? AND artist_id=?`, userID, artistID); err != nil {
			return err
		}
		// Both DELETEs must land first: the predicates below have to see
		// post-unfollow state.
		//
		// Cancellation asks the COARSE visibility question - does the member
		// still follow anything that qualifies for this release - and never the
		// governing-follow recompute. A recompute can come back empty for a
		// release the member plainly still follows (metadata drift, a tightened
		// account preference), and deleting on that would be permanent: the
		// event row is UNIQUE(user_id, release_group_id, event_type) and the
		// release is no longer newly observed, so the alert could never be
		// recreated. The unclaimed guard keeps this off a row a worker is
		// currently sending.
		if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE id IN (
			SELECT d.id FROM deliveries d
			JOIN notification_events ne ON ne.id=d.event_id
			JOIN release_groups rg ON rg.id=ne.release_group_id
			WHERE ne.user_id=? AND d.status IN ('pending','blocked')
			  AND (d.claim_owner IS NULL OR d.claim_expires_at IS NULL OR d.claim_expires_at<=?)
			  AND (rg.artist_id=? OR EXISTS (SELECT 1 FROM release_credits rc
				WHERE rc.release_group_id=rg.id AND rc.artist_id=?))
			  AND NOT `+followedReleasePredicate("ne.user_id")+`)`,
			userID, timeText(now), artistID, artistID); err != nil {
			return err
		}
		// Whatever survived is still reachable through another follow; re-key it
		// onto whichever follow governs it now.
		return realignQueuedDeliveriesTx(ctx, tx, userID, []int64{artistID}, now)
	})
}

func (s *Store) CreateArtistResolution(ctx context.Context, userID int64, provider, providerID, name, providerURL, imageURL string) (ArtistResolution, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	providerID, name, providerURL = strings.TrimSpace(providerID), strings.TrimSpace(name), strings.TrimSpace(providerURL)
	if provider != "spotify" && provider != "itunes" {
		return ArtistResolution{}, false, errors.New("unsupported artist resolution provider")
	}
	if providerID == "" || name == "" || providerURL == "" {
		return ArtistResolution{}, false, errors.New(provider + " artist identity is incomplete")
	}
	type resolutionInsertResult struct {
		resolution ArtistResolution
		created    bool
	}
	result, err := withWriteTxResult(s, ctx, func(tx *sql.Tx) (resolutionInsertResult, error) {
		// Duplicate selections must remain idempotent even when the active queue
		// has reached its cap; return the existing row rather than rejecting it.
		resolution, err := scanArtistResolution(tx.QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
			FROM artist_resolutions WHERE user_id=? AND provider=? AND provider_id=?`, userID, provider, providerID))
		if err == nil {
			return resolutionInsertResult{resolution: resolution}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return resolutionInsertResult{}, err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_resolutions
			WHERE user_id=? AND status IN ('pending','review')`, userID).Scan(&active); err != nil {
			return resolutionInsertResult{}, err
		}
		if active >= maxArtistResolutionsPerUser {
			return resolutionInsertResult{}, ErrArtistResolutionLimit
		}
		now := nowText()
		if _, err := tx.ExecContext(ctx, `INSERT INTO artist_resolutions
			(user_id,provider,provider_id,display_name,provider_url,image_url,status,next_attempt_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,'pending',?,?,?)`,
			userID, provider, providerID, name, providerURL, strings.TrimSpace(imageURL), now, now, now); err != nil {
			return resolutionInsertResult{}, err
		}
		resolution, err = scanArtistResolution(tx.QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
			FROM artist_resolutions WHERE user_id=? AND provider=? AND provider_id=?`, userID, provider, providerID))
		return resolutionInsertResult{resolution: resolution, created: true}, err
	})
	return result.resolution, result.created, err
}

func scanArtistResolution(row interface{ Scan(...any) error }) (ArtistResolution, error) {
	var resolution ArtistResolution
	var candidates, created, updated string
	var nullableNext sql.NullString
	err := row.Scan(
		&resolution.ID, &resolution.UserID, &resolution.Provider, &resolution.ProviderID,
		&resolution.DisplayName, &resolution.ProviderURL, &resolution.ImageURL, &resolution.Status,
		&candidates, &resolution.Attempts, &nullableNext, &resolution.LastError, &created, &updated,
	)
	if err != nil {
		return ArtistResolution{}, err
	}
	if candidates != "" {
		if err := json.Unmarshal([]byte(candidates), &resolution.Candidates); err != nil {
			return ArtistResolution{}, fmt.Errorf("invalid persisted artist resolution candidates: %w", err)
		}
	}
	if resolution.NextAttempt, err = parseStoredNullableTime(nullableNext, "artist resolution next_attempt_at"); err != nil {
		return ArtistResolution{}, err
	}
	resolution.CreatedAt, err = parseStoredTime(created, "artist resolution created_at")
	if err != nil {
		return ArtistResolution{}, err
	}
	resolution.UpdatedAt, err = parseStoredTime(updated, "artist resolution updated_at")
	if err != nil {
		return ArtistResolution{}, err
	}
	return resolution, nil
}
func (s *Store) ArtistResolution(ctx context.Context, userID, resolutionID int64) (ArtistResolution, error) {
	return scanArtistResolution(s.readerDB().QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE id=? AND user_id=?`, resolutionID, userID))
}
func (s *Store) ArtistResolutions(ctx context.Context, userID int64) ([]ArtistResolution, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE user_id=? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ArtistResolution
	for rows.Next() {
		resolution, err := scanArtistResolution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, resolution)
	}
	return result, rows.Err()
}
func (s *Store) DueArtistResolutions(ctx context.Context, now time.Time, limit int) ([]ArtistResolution, error) {
	if limit <= 0 {
		return nil, nil
	}
	perUser := min(maxArtistResolutionBatchPerUser, limit)
	rows, err := s.readerDB().QueryContext(ctx, `WITH due AS (
		SELECT `+artistResolutionColumns+`,
			ROW_NUMBER() OVER (
				PARTITION BY user_id
				ORDER BY COALESCE(next_attempt_at,''),created_at,id
			) AS user_rank
		FROM artist_resolutions
		WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
	)
	SELECT `+artistResolutionColumns+` FROM due
	WHERE user_rank<=?
	ORDER BY COALESCE(next_attempt_at,''),created_at,id
	LIMIT ?`, timeText(now), perUser, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ArtistResolution
	for rows.Next() {
		resolution, err := scanArtistResolution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, resolution)
	}
	return result, rows.Err()
}
func (s *Store) MarkArtistResolutionReview(ctx context.Context, userID, resolutionID int64, candidates []ResolutionCandidate) error {
	payload, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	result, err := s.execWriteContext(ctx, `UPDATE artist_resolutions
		SET status='review',candidate_json=?,next_attempt_at=NULL,last_error='',updated_at=?
		WHERE id=? AND user_id=?`, string(payload), nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) RetryArtistResolution(ctx context.Context, userID, resolutionID int64, attempts int, next time.Time, message string) error {
	result, err := s.execWriteContext(ctx, `UPDATE artist_resolutions
		SET status='pending',candidate_json='[]',attempts=?,next_attempt_at=?,last_error=?,updated_at=?
		WHERE id=? AND user_id=?`,
		attempts, timeText(next), message, nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) CancelArtistResolution(ctx context.Context, userID, resolutionID int64) error {
	result, err := s.execWriteContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`, resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) CompleteArtistResolution(ctx context.Context, resolution ArtistResolution, artist Artist) (Artist, bool, error) {
	var stored Artist
	var added bool
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_resolutions WHERE id=? AND user_id=?`,
			resolution.ID, resolution.UserID).Scan(&exists); err != nil || exists == 0 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		now := nowText()
		_, err := tx.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(artists.spotify_id,excluded.spotify_id),
		spotify_url=CASE WHEN artists.spotify_id IS NULL OR artists.spotify_id=excluded.spotify_id
			THEN COALESCE(excluded.spotify_url,artists.spotify_url) ELSE artists.spotify_url END,
		spotify_image_url=CASE WHEN artists.spotify_id IS NULL OR artists.spotify_id=excluded.spotify_id
			THEN COALESCE(excluded.spotify_image_url,artists.spotify_image_url) ELSE artists.spotify_image_url END,
		updated_at=excluded.updated_at`,
			artist.MBID, artist.Name, artist.SortName, artist.Type, artist.Country, artist.Disambiguation,
			nullString(artist.SpotifyID), nullString(artist.SpotifyURL), nullString(artist.SpotifyImageURL), now, now)
		if err != nil {
			return err
		}
		var sid, surl, image, checked sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,last_checked_at FROM artists WHERE mbid=?`, artist.MBID).Scan(
			&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type, &artist.Country,
			&artist.Disambiguation, &sid, &surl, &image, &checked)
		if err != nil {
			return err
		}
		artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
		if artist.LastCheckedAt, err = parseStoredNullableTime(checked, "artist last_checked_at"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artist_identity_status
			(artist_id,status,attempts,next_check_at,last_error,updated_at)
			VALUES(?, 'verified',0,NULL,'',?)
			ON CONFLICT(artist_id) DO UPDATE SET status='verified',attempts=0,next_check_at=NULL,last_error='',updated_at=excluded.updated_at`,
			artist.ID, now); err != nil {
			return err
		}
		if len(artist.Genres) > 0 {
			if err := replaceArtistGenresExec(ctx, tx, artist.ID, artist.Genres, "musicbrainz"); err != nil {
				return err
			}
		}
		follow, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`,
			resolution.UserID, artist.ID, now)
		if err != nil {
			return err
		}
		followAdded, err := follow.RowsAffected()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follow_notification_rules
		(user_id,artist_id,delivery_mode,include_primary,include_featured,albums,eps,singles,compilations,announcements,release_day,updated_at)
		VALUES(?,?, 'inherit',1,1,1,1,1,1,1,1,?)`, resolution.UserID, artist.ID, now); err != nil {
			return err
		}
		if resolution.Provider == "itunes" {
			if err := saveArtistProviderIdentityTx(ctx, tx, artist.ID, "itunes", resolution.ProviderID, resolution.ProviderURL); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`,
			resolution.ID, resolution.UserID); err != nil {
			return err
		}
		stored = artist
		added = followAdded > 0
		if added {
			if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
				CASE WHEN spotify_id IS NOT NULL AND (spotify_next_check_at IS NULL OR spotify_next_check_at>?)
				THEN ? ELSE spotify_next_check_at END WHERE id=?`, now, now, now, artist.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return stored, added, err
}
func (s *Store) FollowedArtists(ctx context.Context, userID int64) ([]Artist, error) {
	return s.FollowedArtistsFiltered(ctx, userID, "", "", "")
}

// FollowedArtistsExportPage returns a stable keyset-paginated page for CSV
// exports. It mirrors the alphabetical ordering used by the Artists page but
// uses the normalized name/sort-name/id tuple as its cursor, avoiding OFFSET
// skips and duplicates when the watchlist changes during a long export.
func (s *Store) FollowedArtistsExportPage(ctx context.Context, userID int64, limit int, after *ArtistExportCursor) ([]Artist, *ArtistExportCursor, error) {
	if limit < 1 {
		limit = 500
	}
	query := `SELECT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,a.spotify_next_check_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=?`
	args := []any{userID}
	if after != nil {
		query += ` AND (lower(trim(a.name)) > ?
			OR (lower(trim(a.name)) = ? AND lower(trim(a.sort_name)) > ?)
			OR (lower(trim(a.name)) = ? AND lower(trim(a.sort_name)) = ? AND a.id > ?))`
		args = append(args, after.Name, after.Name, after.SortName, after.Name, after.SortName, after.ID)
	}
	query += ` ORDER BY lower(trim(a.name)),lower(trim(a.sort_name)),a.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.readerDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Artist, 0, limit)
	var last *ArtistExportCursor
	for rows.Next() {
		var a Artist
		var sid, surl, image, checked, spotifyNext, baseline sql.NullString
		if err := rows.Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&sid, &surl, &image, &checked, &spotifyNext, &baseline); err != nil {
			return nil, nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
		var parseErr error
		if a.LastCheckedAt, parseErr = parseStoredNullableTime(checked, "artist export last_checked_at"); parseErr != nil {
			return nil, nil, parseErr
		}
		if a.SpotifyNextCheckAt, parseErr = parseStoredNullableTime(spotifyNext, "artist export spotify_next_check_at"); parseErr != nil {
			return nil, nil, parseErr
		}
		a.BaselineSynced = baseline.Valid
		result = append(result, a)
		last = &ArtistExportCursor{
			Name:     strings.ToLower(strings.TrimSpace(a.Name)),
			SortName: strings.ToLower(strings.TrimSpace(a.SortName)),
			ID:       a.ID,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return result, last, nil
}

func (s *Store) FollowedArtistsFiltered(ctx context.Context, userID int64, genre, country, artistType string) ([]Artist, error) {
	where, args := followedArtistFilters(userID, genre, country, artistType)
	return s.followedArtistsQuery(ctx, where, args, 0, 0)
}

// FollowedArtistsFilteredPage returns one alphabetically ordered page of the
// user's followed artists. Metadata enrichment is applied to only the page,
// keeping large watchlists from producing an oversized response or doing
// unnecessary work for artists outside the current page.
func (s *Store) FollowedArtistsFilteredPage(ctx context.Context, userID int64, genre, country, artistType string, limit, offset int) ([]Artist, error) {
	if limit < 1 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where, args := followedArtistFilters(userID, genre, country, artistType)
	return s.followedArtistsQuery(ctx, where, args, limit, offset)
}

// FollowedArtistsFilteredCount counts the distinct canonical artists matching
// the same filters used by FollowedArtistsFilteredPage.
func (s *Store) FollowedArtistsFilteredCount(ctx context.Context, userID int64, genre, country, artistType string) (int, error) {
	where, args := followedArtistFilters(userID, genre, country, artistType)
	var count int
	err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.id)
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE `+strings.Join(where, " AND "), args...).Scan(&count)
	return count, err
}

func followedArtistFilters(userID int64, genre, country, artistType string) ([]string, []any) {
	where := []string{"f.user_id=?"}
	args := []any{userID}
	if strings.TrimSpace(genre) != "" {
		if strings.EqualFold(strings.TrimSpace(genre), "unknown") {
			where = append(where, "NOT EXISTS (SELECT 1 FROM artist_genres ag WHERE ag.artist_id=a.id)")
		} else {
			where = append(where, "EXISTS (SELECT 1 FROM artist_genres ag WHERE ag.artist_id=a.id AND ag.genre_key=?)")
			args = append(args, normalizeGenreKey(genre))
		}
	}
	if strings.TrimSpace(country) != "" {
		if strings.EqualFold(strings.TrimSpace(country), "unknown") {
			where = append(where, "trim(a.country)=''")
		} else {
			where = append(where, "lower(a.country)=lower(?)")
			args = append(args, strings.TrimSpace(country))
		}
	}
	if strings.TrimSpace(artistType) != "" {
		if strings.EqualFold(strings.TrimSpace(artistType), "unknown") {
			where = append(where, "trim(a.artist_type)=''")
		} else {
			where = append(where, "lower(a.artist_type)=lower(?)")
			args = append(args, strings.TrimSpace(artistType))
		}
	}
	return where, args
}

func (s *Store) followedArtistsQuery(ctx context.Context, where []string, args []any, limit, offset int) ([]Artist, error) {
	query := `SELECT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,a.spotify_next_check_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY lower(trim(a.name)), lower(trim(a.sort_name)), a.id`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.readerDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Artist
	for rows.Next() {
		var a Artist
		var sid, surl, image, checked, spotifyNext, baseline sql.NullString
		if err := rows.Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&sid, &surl, &image, &checked, &spotifyNext, &baseline); err != nil {
			return nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
		var parseErr error
		if a.LastCheckedAt, parseErr = parseStoredNullableTime(checked, "artist last_checked_at"); parseErr != nil {
			return nil, parseErr
		}
		if a.SpotifyNextCheckAt, parseErr = parseStoredNullableTime(spotifyNext, "artist spotify_next_check_at"); parseErr != nil {
			return nil, parseErr
		}
		a.BaselineSynced = baseline.Valid
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.enrichArtistMetadata(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) FollowedArtistCount(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=?`, userID).Scan(&count)
	return count, err
}
func (s *Store) replaceArtistGenres(ctx context.Context, artistID int64, genres []string, source string) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return replaceArtistGenresExec(ctx, tx, artistID, genres, source)
	})
}
func (s *Store) ArtistGenres(ctx context.Context, artistID int64) ([]string, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT genre FROM artist_genres WHERE artist_id=? ORDER BY weight DESC,genre`, artistID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var genres []string
	for rows.Next() {
		var genre string
		if err := rows.Scan(&genre); err != nil {
			return nil, err
		}
		genres = append(genres, genre)
	}
	return genres, rows.Err()
}
func (s *Store) SaveArtistGenres(ctx context.Context, artistID int64, genres []string) error {
	if len(genres) == 0 {
		return nil
	}
	return s.replaceArtistGenres(ctx, artistID, genres, "musicbrainz")
}
func (s *Store) enrichArtistMetadata(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	genresByArtist := make(map[int64][]string, len(artists))
	statsByArtist := make(map[int64]ListenBrainzStats, len(artists))
	for start := 0; start < len(artists); start += 500 {
		end := min(start+500, len(artists))
		ids := make([]int64, end-start)
		args := make([]any, end-start)
		for index := range ids {
			ids[index] = artists[start+index].ID
			args[index] = ids[index]
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		if err := func() error {
			rows, err := s.readerDB().QueryContext(ctx, `SELECT artist_id,genre FROM artist_genres WHERE artist_id IN (`+placeholders+`) ORDER BY artist_id,weight DESC,genre`, args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var artistID int64
				var genre string
				if err := rows.Scan(&artistID, &genre); err != nil {
					return err
				}
				genresByArtist[artistID] = append(genresByArtist[artistID], genre)
			}
			return rows.Err()
		}(); err != nil {
			return err
		}

		if err := func() error {
			statsRows, err := s.readerDB().QueryContext(ctx, `SELECT artist_id,total_listen_count,total_user_count,checked_at,next_check_at,last_error FROM artist_listenbrainz_stats WHERE artist_id IN (`+placeholders+`)`, args...)
			if err != nil {
				return err
			}
			defer func() { _ = statsRows.Close() }()
			for statsRows.Next() {
				var stats ListenBrainzStats
				var checked, next sql.NullString
				if err := statsRows.Scan(&stats.ArtistID, &stats.TotalListenCount, &stats.TotalUserCount, &checked, &next, &stats.LastError); err != nil {
					return err
				}
				var parseErr error
				if stats.CheckedAt, parseErr = parseStoredNullableTime(checked, "ListenBrainz checked_at"); parseErr != nil {
					return parseErr
				}
				if stats.NextCheckAt, parseErr = parseStoredNullableTime(next, "ListenBrainz next_check_at"); parseErr != nil {
					return parseErr
				}
				statsByArtist[stats.ArtistID] = stats
			}
			return statsRows.Err()
		}(); err != nil {
			return err
		}
	}
	for index := range artists {
		artists[index].Genres = genresByArtist[artists[index].ID]
		if stats, ok := statsByArtist[artists[index].ID]; ok {
			artists[index].ListenCount, artists[index].ListenUsers = stats.TotalListenCount, stats.TotalUserCount
			artists[index].ListenCheckedAt = stats.CheckedAt
		}
	}
	return nil
}
func (s *Store) DueListenBrainzArtists(ctx context.Context, now time.Time, limit int) ([]Artist, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,a.spotify_id,a.spotify_url,a.spotify_image_url
		FROM artists a JOIN follows f ON f.artist_id=a.id LEFT JOIN artist_listenbrainz_stats ls ON ls.artist_id=a.id
		WHERE ls.artist_id IS NULL OR ls.next_check_at IS NULL OR ls.next_check_at<=?
		ORDER BY COALESCE(ls.next_check_at,''),a.id LIMIT ?`, timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Artist
	for rows.Next() {
		var artist Artist
		var sid, surl, image sql.NullString
		if err := rows.Scan(&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type, &artist.Country, &artist.Disambiguation, &sid, &surl, &image); err != nil {
			return nil, err
		}
		artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
		result = append(result, artist)
	}
	return result, rows.Err()
}
func (s *Store) SaveListenBrainzStats(ctx context.Context, stats map[int64]ListenBrainzStats, now, next time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for artistID, value := range stats {
			if _, err := tx.ExecContext(ctx, `INSERT INTO artist_listenbrainz_stats(artist_id,total_listen_count,total_user_count,checked_at,next_check_at,last_error,attempts,updated_at) VALUES(?,?,?,?,?,'',0,?) ON CONFLICT(artist_id) DO UPDATE SET total_listen_count=excluded.total_listen_count,total_user_count=excluded.total_user_count,checked_at=excluded.checked_at,next_check_at=excluded.next_check_at,last_error='',attempts=0,updated_at=excluded.updated_at`, artistID, value.TotalListenCount, value.TotalUserCount, timeText(now), timeText(next), timeText(now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ScheduleListenBrainzRefresh advances refresh timestamps for artists that
// were absent from an otherwise successful provider response. Existing
// aggregate totals are intentionally untouched.
func (s *Store) ScheduleListenBrainzRefresh(ctx context.Context, artistIDs []int64, next time.Time) error {
	if len(artistIDs) == 0 {
		return nil
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, artistID := range artistIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO artist_listenbrainz_stats(artist_id,next_check_at,last_error,attempts,updated_at)
				VALUES(?,?, '',0,?) ON CONFLICT(artist_id) DO UPDATE SET next_check_at=excluded.next_check_at,updated_at=excluded.updated_at`,
				artistID, timeText(next), timeText(next)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ScheduleListenBrainzRetry(ctx context.Context, artistIDs []int64, next time.Time, message string) error {
	if len(artistIDs) == 0 {
		return nil
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, artistID := range artistIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO artist_listenbrainz_stats(artist_id,next_check_at,last_error,attempts,updated_at) VALUES(?,?,?,1,?) ON CONFLICT(artist_id) DO UPDATE SET next_check_at=excluded.next_check_at,last_error=excluded.last_error,attempts=artist_listenbrainz_stats.attempts+1,updated_at=excluded.updated_at`, artistID, timeText(next), message, nowText()); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) TopListenBrainzArtists(ctx context.Context, userID int64, limit int) ([]Artist, error) {
	if limit < 1 || limit > 20 {
		limit = 5
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,a.spotify_id,a.spotify_url,a.spotify_image_url,ls.total_listen_count,ls.total_user_count,ls.checked_at
		FROM follows f JOIN artists a ON a.id=f.artist_id JOIN artist_listenbrainz_stats ls ON ls.artist_id=a.id WHERE f.user_id=? ORDER BY ls.total_listen_count DESC,a.name LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Artist
	for rows.Next() {
		var artist Artist
		var sid, surl, image, checked sql.NullString
		if err := rows.Scan(&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type, &artist.Country, &artist.Disambiguation, &sid, &surl, &image, &artist.ListenCount, &artist.ListenUsers, &checked); err != nil {
			return nil, err
		}
		artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
		var parseErr error
		if artist.ListenCheckedAt, parseErr = parseStoredNullableTime(checked, "ListenBrainz checked_at"); parseErr != nil {
			return nil, parseErr
		}
		result = append(result, artist)
	}
	return result, rows.Err()
}
func (s *Store) FollowedBreakdown(ctx context.Context, userID int64, dimension string) ([]ArtistBreakdown, error) {
	var query string
	switch dimension {
	case "genre":
		query = `SELECT COALESCE(ag.genre,'Unknown'),COUNT(DISTINCT a.id) FROM follows f JOIN artists a ON a.id=f.artist_id LEFT JOIN artist_genres ag ON ag.artist_id=a.id WHERE f.user_id=? GROUP BY COALESCE(ag.genre,'Unknown') ORDER BY COUNT(DISTINCT a.id) DESC,lower(COALESCE(ag.genre,'Unknown')) LIMIT 12`
	case "country":
		query = `SELECT CASE WHEN trim(a.country)='' THEN 'Unknown' ELSE a.country END,COUNT(DISTINCT a.id) FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=? GROUP BY CASE WHEN trim(a.country)='' THEN 'Unknown' ELSE a.country END ORDER BY COUNT(DISTINCT a.id) DESC,lower(a.country) LIMIT 12`
	case "type":
		query = `SELECT CASE WHEN trim(a.artist_type)='' THEN 'Unknown' ELSE a.artist_type END,COUNT(DISTINCT a.id) FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=? GROUP BY CASE WHEN trim(a.artist_type)='' THEN 'Unknown' ELSE a.artist_type END ORDER BY COUNT(DISTINCT a.id) DESC,lower(a.artist_type) LIMIT 12`
	default:
		return nil, errors.New("unsupported artist breakdown")
	}
	rows, err := s.readerDB().QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ArtistBreakdown
	for rows.Next() {
		var item ArtistBreakdown
		if err := rows.Scan(&item.Label, &item.Count); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (s *Store) IsFollowing(ctx context.Context, userID, artistID int64) (bool, error) {
	var n int
	err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=? AND artist_id=?`, userID, artistID).Scan(&n)
	return n > 0, err
}
func (s *Store) ArtistsDue(ctx context.Context, now time.Time, limit int) ([]Artist, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.spotify_next_check_at,a.last_checked_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE `+artistSyncAdmissionPredicate("a")+`
		  AND ((a.next_check_at IS NULL OR a.next_check_at<=?)
		   OR (a.spotify_id IS NOT NULL AND (a.spotify_next_check_at IS NULL OR a.spotify_next_check_at<=?)))
		ORDER BY CASE
			WHEN a.next_check_at IS NULL THEN COALESCE(a.spotify_next_check_at,'')
			WHEN a.spotify_next_check_at IS NULL THEN a.next_check_at
			WHEN a.next_check_at < a.spotify_next_check_at THEN a.next_check_at
			ELSE a.spotify_next_check_at
		END, a.id LIMIT ?`,
		timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Artist
	for rows.Next() {
		var a Artist
		var spotifyID, spotifyURL, spotifyImage, spotifyNext, lastChecked sql.NullString
		if err := rows.Scan(
			&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&spotifyID, &spotifyURL, &spotifyImage, &spotifyNext, &lastChecked,
		); err != nil {
			return nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImage.String
		if a.SpotifyNextCheckAt, err = parseStoredNullableTime(spotifyNext, "artist spotify_next_check_at"); err != nil {
			return nil, err
		}
		// The scheduled sync path widens the announcement cutoff to the previous
		// successful check after a downtime gap. Without this column that
		// widening is unreachable, so releases published during an outage longer
		// than the seven-day window are observed and then silently suppressed.
		if a.LastCheckedAt, err = parseStoredNullableTime(lastChecked, "artist last_checked_at"); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// DueArtistSyncBacklog returns the total number of distinct followed artists
// whose normal or Spotify schedule is due, along with the oldest due time.
// The admission predicate intentionally matches ArtistsDue so terminal
// imported identities are not counted as actionable work.
func (s *Store) DueArtistSyncBacklog(ctx context.Context, now time.Time) (ArtistSyncBacklog, error) {
	var backlog ArtistSyncBacklog
	var oldest sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `WITH due AS (
		SELECT a.id,COALESCE(a.next_check_at,?) AS due_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE `+artistSyncAdmissionPredicate("a")+` AND (a.next_check_at IS NULL OR a.next_check_at<=?)
		UNION ALL
		SELECT a.id,COALESCE(a.spotify_next_check_at,?) AS due_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE `+artistSyncAdmissionPredicate("a")+` AND a.spotify_id IS NOT NULL
			AND (a.spotify_next_check_at IS NULL OR a.spotify_next_check_at<=?)
	)
	SELECT COUNT(DISTINCT id),MIN(due_at) FROM due`,
		timeText(now), timeText(now), timeText(now), timeText(now)).
		Scan(&backlog.Count, &oldest)
	if err != nil {
		return ArtistSyncBacklog{}, err
	}
	var parseErr error
	if backlog.OldestDueAt, parseErr = parseStoredNullableTime(oldest, "oldest due artist sync"); parseErr != nil {
		return ArtistSyncBacklog{}, parseErr
	}
	return backlog, nil
}

func (s *Store) MarkArtistChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	next := now.Add(interval)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET last_checked_at=?,next_check_at=? WHERE id=?`,
			timeText(now), timeText(next), artistID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artist_provider_status SET next_check_at=? WHERE artist_id=? AND provider IN ('itunes','musicbrainz')`,
			timeText(next), artistID)
		return err
	})
}
func (s *Store) ScheduleArtistCheck(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.execWriteContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, timeText(next), artistID)
	return err
}

// ScheduleArtistRetry advances the normal schedule after a persistence
// failure. Any due Spotify/provider schedule is advanced with it so the
// artist cannot remain pinned by ArtistsDue's OR predicate, while a provider
// cooldown already in the future is preserved.
func (s *Store) ScheduleArtistRetry(ctx context.Context, artistID int64, now, next time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=?,spotify_next_check_at=
			CASE WHEN spotify_next_check_at IS NULL OR spotify_next_check_at<=? THEN ? ELSE spotify_next_check_at END
			WHERE id=?`, timeText(next), timeText(now), timeText(next), artistID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artist_provider_status SET next_check_at=?
			WHERE artist_id=? AND (next_check_at IS NULL OR next_check_at<=?)`, timeText(next), artistID, timeText(now))
		return err
	})
}

func (s *Store) MarkSpotifyChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	return s.MarkSpotifyCheckedAdaptive(ctx, artistID, now, interval, true, false)

}
func (s *Store) SpotifyPollingState(ctx context.Context, artistID int64) (SpotifyPollingState, error) {
	var state SpotifyPollingState
	var last sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT spotify_unchanged_checks,spotify_last_change_at FROM artists WHERE id=?`, artistID).
		Scan(&state.UnchangedChecks, &last)
	if err != nil {
		// Preserve lookup failures, especially sql.ErrNoRows. Parsing the
		// nullable timestamp before checking the scan result would overwrite a
		// missing/corrupt artist error with nil and reset adaptive polling.
		return SpotifyPollingState{}, err
	}
	if state.LastChangeAt, err = parseStoredNullableTime(last, "artist spotify_last_change_at"); err != nil {
		return state, err
	}
	return state, nil
}
func (s *Store) MarkSpotifyCheckedAdaptive(ctx context.Context, artistID int64, now time.Time, interval time.Duration, changed, upcoming bool) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var streak int
		var last sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT spotify_unchanged_checks,spotify_last_change_at FROM artists WHERE id=?`, artistID).Scan(&streak, &last); err != nil {
			return err
		}
		var lastChange *time.Time
		if last.Valid {
			parsed, parseErr := parseStoredNullableTime(last, "artist spotify_last_change_at")
			if parseErr != nil {
				return parseErr
			}
			lastChange = parsed
		}
		if changed {
			streak = 0
			t := now
			lastChange = &t
		} else {
			streak++
		}
		delay := spotifyPollDelay(artistID, interval)
		if !changed && !upcoming {
			backoff := interval
			for i := 0; i < min(streak, 3); i++ {
				backoff *= 2
			}
			if backoff > 7*24*time.Hour {
				backoff = 7 * 24 * time.Hour
			}
			delay = spotifyPollDelay(artistID, backoff)
			if delay > backoff {
				delay = backoff
			}
		}
		next := now.Add(delay)
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=?,spotify_unchanged_checks=?,spotify_last_change_at=? WHERE id=?`,
			timeText(next), streak, nullableTime(lastChange), artistID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artist_provider_status SET next_check_at=? WHERE artist_id=? AND provider='spotify'`,
			timeText(next), artistID)
		return err
	})
}
func (s *Store) ScheduleSpotifyCheck(ctx context.Context, artistID int64, next time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=? WHERE id=?`, timeText(next), artistID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artist_provider_status SET next_check_at=? WHERE artist_id=? AND provider='spotify'`,
			timeText(next), artistID)
		return err
	})
}
func (s *Store) LatestSpotifyReleaseDate(ctx context.Context, artistID int64) (string, error) {
	var date sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT MAX(first_release_date) FROM release_groups
		WHERE artist_id=? AND source IN ('spotify','both')`, artistID).Scan(&date)
	return date.String, err
}

// DueITunesArtworkArtist returns at most one artist per call so the background
// runner can spread artwork requests over time and respect Apple's limiter.
func (s *Store) DueITunesArtworkArtist(ctx context.Context, now time.Time) (ITunesArtworkArtist, bool, error) {
	var artist ITunesArtworkArtist
	err := s.readerDB().QueryRowContext(ctx, `SELECT a.id,a.mbid,a.name,MAX(rg.itunes_artwork_attempts)
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE rg.itunes_id IS NOT NULL AND rg.itunes_id<>'' AND rg.itunes_artwork_url=''
			AND (rg.itunes_artwork_next_check_at IS NULL OR rg.itunes_artwork_next_check_at<=?)
		GROUP BY a.id,a.mbid,a.name
		ORDER BY MIN(COALESCE(rg.itunes_artwork_next_check_at,'')),a.id LIMIT 1`, timeText(now)).Scan(
		&artist.ID, &artist.MBID, &artist.Name, &artist.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ITunesArtworkArtist{}, false, nil
	}
	if err != nil {
		return ITunesArtworkArtist{}, false, err
	}
	return artist, true, nil
}

// ApplyITunesArtworkBackfill updates only already persisted release rows. A
// successful response without artwork receives a long negative-cache period;
// it does not affect normal catalog scheduling or notifications.
func (s *Store) ApplyITunesArtworkBackfill(ctx context.Context, artistID int64, releases []Release, observed time.Time) (checked, updated int, err error) {
	byID := make(map[string]string, len(releases))
	for _, release := range releases {
		id := strings.TrimSpace(release.ITunesID)
		artworkURL := strings.TrimSpace(release.ITunesArtworkURL)
		if id != "" && validITunesArtworkURL(artworkURL) {
			byID[id] = artworkURL
		}
	}
	result, err := withWriteTxResult(s, ctx, func(tx *sql.Tx) (struct{ checked, updated int }, error) {
		var counts struct{ checked, updated int }
		type candidate struct {
			id       int64
			itunesID string
		}
		candidates, err := func() ([]candidate, error) {
			rows, err := tx.QueryContext(ctx, `SELECT id,itunes_id FROM release_groups
			WHERE artist_id=? AND itunes_id IS NOT NULL AND itunes_id<>'' AND itunes_artwork_url=''`, artistID)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			var candidates []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.id, &item.itunesID); err != nil {
					return nil, err
				}
				candidates = append(candidates, item)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return candidates, nil
		}()
		if err != nil {
			return counts, err
		}
		checkedAt := timeText(observed)
		negativeNext := timeText(observed.Add(30 * 24 * time.Hour))
		for _, item := range candidates {
			counts.checked++
			artworkURL := byID[item.itunesID]
			if artworkURL != "" {
				result, execErr := tx.ExecContext(ctx, `UPDATE release_groups SET
				itunes_artwork_url=?,itunes_artwork_checked_at=?,itunes_artwork_next_check_at=NULL,
				itunes_artwork_attempts=0,updated_at=? WHERE id=?`, artworkURL, checkedAt, checkedAt, item.id)
				if execErr != nil {
					return counts, execErr
				}
				n, _ := result.RowsAffected()
				counts.updated += int(n)
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE release_groups SET
			itunes_artwork_checked_at=?,itunes_artwork_next_check_at=?,
			itunes_artwork_attempts=itunes_artwork_attempts+1,updated_at=? WHERE id=?`,
				checkedAt, negativeNext, checkedAt, item.id); err != nil {
				return counts, err
			}
		}
		return counts, nil
	})
	return result.checked, result.updated, err
}

// ScheduleITunesArtworkRetry applies a bounded durable retry time to all
// existing artwork gaps for an artist after a transient provider failure.
func (s *Store) ScheduleITunesArtworkRetry(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.execWriteContext(ctx, `UPDATE release_groups SET
		itunes_artwork_next_check_at=?,itunes_artwork_attempts=itunes_artwork_attempts+1
		WHERE artist_id=? AND itunes_id IS NOT NULL AND itunes_id<>'' AND itunes_artwork_url=''`,
		timeText(next), artistID)
	return err
}

// SpotifyBatchChanged reports whether a successful provider response contains
// a release that is new or has changed since it was last observed. It is
// intentionally read before the release transaction so the scheduler can
// persist its adaptive state together with the completed observation.
func (s *Store) SpotifyBatchChanged(ctx context.Context, releases []Release) (bool, error) {
	for _, release := range releases {
		providerID := strings.TrimSpace(release.SpotifyID)
		if providerID == "" {
			continue
		}
		var hash string
		err := s.readerDB().QueryRowContext(ctx, `SELECT payload_hash FROM provider_observations WHERE provider='spotify' AND provider_id=?`, providerID).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if hash != releasePayloadHash(release) {
			return true, nil
		}
	}
	return false, nil
}
