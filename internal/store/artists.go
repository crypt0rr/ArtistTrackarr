package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (s *Store) UpsertArtist(ctx context.Context, a Artist) (Artist, error) {
	now := nowText()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(excluded.spotify_id,artists.spotify_id),
		spotify_url=COALESCE(excluded.spotify_url,artists.spotify_url),
		spotify_image_url=COALESCE(excluded.spotify_image_url,artists.spotify_image_url),updated_at=excluded.updated_at`,
		a.MBID, a.Name, a.SortName, a.Type, a.Country, a.Disambiguation,
		nullString(a.SpotifyID), nullString(a.SpotifyURL), nullString(a.SpotifyImageURL), now, now)
	if err != nil {
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
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
	if checked.Valid {
		t, _ := parseTime(checked.String)
		a.LastCheckedAt = &t
	}
	return a, err
}
func (s *Store) Follow(ctx context.Context, userID, artistID int64) (bool, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
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
		if _, err := tx.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, now, artistID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}
func (s *Store) Unfollow(ctx context.Context, userID, artistID int64) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE user_id=? AND artist_id=?`, userID, artistID)
	return changedOrNotFound(result, err)
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
	now := nowText()
	result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO artist_resolutions
		(user_id,provider,provider_id,display_name,provider_url,image_url,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'pending',?,?,?)`,
		userID, provider, providerID, name, providerURL, strings.TrimSpace(imageURL), now, now, now)
	if err != nil {
		return ArtistResolution{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ArtistResolution{}, false, err
	}
	resolution, err := s.artistResolutionByProvider(ctx, userID, provider, providerID)
	return resolution, changed > 0, err
}

func scanArtistResolution(row interface{ Scan(...any) error }) (ArtistResolution, error) {
	var resolution ArtistResolution
	var candidates, nextAttempt, created, updated string
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
		_ = json.Unmarshal([]byte(candidates), &resolution.Candidates)
	}
	if nullableNext.Valid {
		nextAttempt = nullableNext.String
		parsed, parseErr := parseTime(nextAttempt)
		if parseErr == nil {
			resolution.NextAttempt = &parsed
		}
	}
	resolution.CreatedAt, _ = parseTime(created)
	resolution.UpdatedAt, _ = parseTime(updated)
	return resolution, nil
}
func (s *Store) artistResolutionByProvider(ctx context.Context, userID int64, provider, providerID string) (ArtistResolution, error) {
	return scanArtistResolution(s.readerDB().QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE user_id=? AND provider=? AND provider_id=?`, userID, provider, providerID))
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
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		ORDER BY COALESCE(next_attempt_at,'') LIMIT ?`, timeText(now), limit)
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
	result, err := s.DB.ExecContext(ctx, `UPDATE artist_resolutions
		SET status='review',candidate_json=?,next_attempt_at=NULL,last_error='',updated_at=?
		WHERE id=? AND user_id=?`, string(payload), nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) RetryArtistResolution(ctx context.Context, userID, resolutionID int64, attempts int, next time.Time, message string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE artist_resolutions
		SET status='pending',candidate_json='[]',attempts=?,next_attempt_at=?,last_error=?,updated_at=?
		WHERE id=? AND user_id=?`,
		attempts, timeText(next), message, nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) CancelArtistResolution(ctx context.Context, userID, resolutionID int64) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`, resolutionID, userID)
	return changedOrNotFound(result, err)
}
func (s *Store) CompleteArtistResolution(ctx context.Context, resolution ArtistResolution, artist Artist) (Artist, bool, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return Artist{}, false, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_resolutions WHERE id=? AND user_id=?`,
		resolution.ID, resolution.UserID).Scan(&exists); err != nil || exists == 0 {
		_ = tx.Rollback()
		if err != nil {
			return Artist{}, false, err
		}
		return Artist{}, false, sql.ErrNoRows
	}
	now := nowText()
	_, err = tx.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(excluded.spotify_id,artists.spotify_id),
		spotify_url=COALESCE(excluded.spotify_url,artists.spotify_url),
		spotify_image_url=COALESCE(excluded.spotify_image_url,artists.spotify_image_url),updated_at=excluded.updated_at`,
		artist.MBID, artist.Name, artist.SortName, artist.Type, artist.Country, artist.Disambiguation,
		nullString(artist.SpotifyID), nullString(artist.SpotifyURL), nullString(artist.SpotifyImageURL), now, now)
	if err != nil {
		_ = tx.Rollback()
		return Artist{}, false, err
	}
	var sid, surl, image, checked sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,last_checked_at FROM artists WHERE mbid=?`, artist.MBID).Scan(
		&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type, &artist.Country,
		&artist.Disambiguation, &sid, &surl, &image, &checked)
	if err != nil {
		_ = tx.Rollback()
		return Artist{}, false, err
	}
	artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
	if checked.Valid {
		parsed, _ := parseTime(checked.String)
		artist.LastCheckedAt = &parsed
	}
	if len(artist.Genres) > 0 {
		if err := replaceArtistGenresExec(ctx, tx, artist.ID, artist.Genres, "musicbrainz"); err != nil {
			_ = tx.Rollback()
			return Artist{}, false, err
		}
	}
	follow, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`,
		resolution.UserID, artist.ID, now)
	if err != nil {
		_ = tx.Rollback()
		return Artist{}, false, err
	}
	added, _ := follow.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`,
		resolution.ID, resolution.UserID); err != nil {
		_ = tx.Rollback()
		return Artist{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Artist{}, false, err
	}
	return artist, added > 0, nil
}
func (s *Store) FollowedArtists(ctx context.Context, userID int64) ([]Artist, error) {
	return s.FollowedArtistsFiltered(ctx, userID, "", "", "")
}
func (s *Store) FollowedArtistsFiltered(ctx context.Context, userID int64, genre, country, artistType string) ([]Artist, error) {
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
	rows, err := s.readerDB().QueryContext(ctx, `SELECT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,a.spotify_next_check_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE `+strings.Join(where, " AND ")+`
		ORDER BY lower(trim(a.name)), lower(trim(a.sort_name)), a.id`, args...)
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
		if checked.Valid {
			t, _ := parseTime(checked.String)
			a.LastCheckedAt = &t
		}
		if spotifyNext.Valid {
			t, _ := parseTime(spotifyNext.String)
			a.SpotifyNextCheckAt = &t
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
	return replaceArtistGenresExec(ctx, s.DB, artistID, genres, source)
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
		rows, err := s.readerDB().QueryContext(ctx, `SELECT artist_id,genre FROM artist_genres WHERE artist_id IN (`+placeholders+`) ORDER BY artist_id,weight DESC,genre`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var artistID int64
			var genre string
			if err := rows.Scan(&artistID, &genre); err != nil {
				_ = rows.Close()
				return err
			}
			genresByArtist[artistID] = append(genresByArtist[artistID], genre)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		statsRows, err := s.readerDB().QueryContext(ctx, `SELECT artist_id,total_listen_count,total_user_count,checked_at,next_check_at,last_error FROM artist_listenbrainz_stats WHERE artist_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return err
		}
		for statsRows.Next() {
			var stats ListenBrainzStats
			var checked, next sql.NullString
			if err := statsRows.Scan(&stats.ArtistID, &stats.TotalListenCount, &stats.TotalUserCount, &checked, &next, &stats.LastError); err != nil {
				_ = statsRows.Close()
				return err
			}
			if checked.Valid {
				value, parseErr := parseTime(checked.String)
				if parseErr == nil {
					stats.CheckedAt = &value
				}
			}
			if next.Valid {
				value, parseErr := parseTime(next.String)
				if parseErr == nil {
					stats.NextCheckAt = &value
				}
			}
			statsByArtist[stats.ArtistID] = stats
		}
		if err := statsRows.Err(); err != nil {
			_ = statsRows.Close()
			return err
		}
		_ = statsRows.Close()
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for artistID, value := range stats {
		if _, err := tx.ExecContext(ctx, `INSERT INTO artist_listenbrainz_stats(artist_id,total_listen_count,total_user_count,checked_at,next_check_at,last_error,attempts,updated_at) VALUES(?,?,?,?,?,'',0,?) ON CONFLICT(artist_id) DO UPDATE SET total_listen_count=excluded.total_listen_count,total_user_count=excluded.total_user_count,checked_at=excluded.checked_at,next_check_at=excluded.next_check_at,last_error='',attempts=0,updated_at=excluded.updated_at`, artistID, value.TotalListenCount, value.TotalUserCount, timeText(now), timeText(next), timeText(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) ScheduleListenBrainzRetry(ctx context.Context, artistIDs []int64, next time.Time, message string) error {
	for _, artistID := range artistIDs {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO artist_listenbrainz_stats(artist_id,next_check_at,last_error,attempts,updated_at) VALUES(?,?,?,1,?) ON CONFLICT(artist_id) DO UPDATE SET next_check_at=excluded.next_check_at,last_error=excluded.last_error,attempts=artist_listenbrainz_stats.attempts+1,updated_at=excluded.updated_at`, artistID, timeText(next), message, nowText()); err != nil {
			return err
		}
	}
	return nil
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
		if checked.Valid {
			value, _ := parseTime(checked.String)
			artist.ListenCheckedAt = &value
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
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.spotify_next_check_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE (a.next_check_at IS NULL OR a.next_check_at<=?)
		   OR (a.spotify_id IS NOT NULL AND (a.spotify_next_check_at IS NULL OR a.spotify_next_check_at<=?))
		ORDER BY COALESCE(a.next_check_at,a.spotify_next_check_at,'') LIMIT ?`,
		timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Artist
	for rows.Next() {
		var a Artist
		var spotifyID, spotifyURL, spotifyImage, spotifyNext sql.NullString
		if err := rows.Scan(
			&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&spotifyID, &spotifyURL, &spotifyImage, &spotifyNext,
		); err != nil {
			return nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImage.String
		if spotifyNext.Valid {
			t, _ := parseTime(spotifyNext.String)
			a.SpotifyNextCheckAt = &t
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) MarkArtistChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET last_checked_at=?,next_check_at=? WHERE id=?`,
		timeText(now), timeText(now.Add(interval)), artistID)
	return err
}
func (s *Store) ScheduleArtistCheck(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, timeText(next), artistID)
	return err
}
func (s *Store) MarkSpotifyChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	return s.MarkSpotifyCheckedAdaptive(ctx, artistID, now, interval, true, false)

}
func (s *Store) SpotifyPollingState(ctx context.Context, artistID int64) (SpotifyPollingState, error) {
	var state SpotifyPollingState
	var last sql.NullString
	err := s.readerDB().QueryRowContext(ctx, `SELECT spotify_unchanged_checks,spotify_last_change_at FROM artists WHERE id=?`, artistID).
		Scan(&state.UnchangedChecks, &last)
	if last.Valid {
		t, parseErr := parseTime(last.String)
		if parseErr == nil {
			state.LastChangeAt = &t
		}
	}
	return state, err
}
func (s *Store) MarkSpotifyCheckedAdaptive(ctx context.Context, artistID int64, now time.Time, interval time.Duration, changed, upcoming bool) error {
	state, err := s.SpotifyPollingState(ctx, artistID)
	if err != nil {
		return err
	}
	streak := state.UnchangedChecks
	lastChange := state.LastChangeAt
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
	_, err = s.DB.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=?,spotify_unchanged_checks=?,spotify_last_change_at=? WHERE id=?`,
		timeText(now.Add(delay)), streak, nullableTime(lastChange), artistID)
	return err
}
func (s *Store) ScheduleSpotifyCheck(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=? WHERE id=?`, timeText(next), artistID)
	return err
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
	err := s.readerDB().QueryRowContext(ctx, `SELECT a.id,a.name,MAX(rg.itunes_artwork_attempts)
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE rg.itunes_id IS NOT NULL AND rg.itunes_id<>'' AND rg.itunes_artwork_url=''
			AND (rg.itunes_artwork_next_check_at IS NULL OR rg.itunes_artwork_next_check_at<=?)
		GROUP BY a.id,a.name
		ORDER BY MIN(COALESCE(rg.itunes_artwork_next_check_at,'')),a.id LIMIT 1`, timeText(now)).Scan(
		&artist.ID, &artist.Name, &artist.Attempts)
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `SELECT id,itunes_id FROM release_groups
		WHERE artist_id=? AND itunes_id IS NOT NULL AND itunes_id<>'' AND itunes_artwork_url=''`, artistID)
	if err != nil {
		return 0, 0, err
	}
	type candidate struct {
		id       int64
		itunesID string
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.itunesID); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, err
	}
	_ = rows.Close()
	checkedAt := timeText(observed)
	negativeNext := timeText(observed.Add(30 * 24 * time.Hour))
	for _, item := range candidates {
		checked++
		artworkURL := byID[item.itunesID]
		if artworkURL != "" {
			result, execErr := tx.ExecContext(ctx, `UPDATE release_groups SET
				itunes_artwork_url=?,itunes_artwork_checked_at=?,itunes_artwork_next_check_at=NULL,
				itunes_artwork_attempts=0,updated_at=? WHERE id=?`, artworkURL, checkedAt, checkedAt, item.id)
			if execErr != nil {
				return 0, 0, execErr
			}
			n, _ := result.RowsAffected()
			updated += int(n)
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE release_groups SET
			itunes_artwork_checked_at=?,itunes_artwork_next_check_at=?,
			itunes_artwork_attempts=itunes_artwork_attempts+1,updated_at=? WHERE id=?`,
			checkedAt, negativeNext, checkedAt, item.id); err != nil {
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return checked, updated, nil
}

// ScheduleITunesArtworkRetry applies a bounded durable retry time to all
// existing artwork gaps for an artist after a transient provider failure.
func (s *Store) ScheduleITunesArtworkRetry(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE release_groups SET
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
