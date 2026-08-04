package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (s *Store) ApplyReleaseSync(ctx context.Context, artist Artist, releases []Release, observed time.Time) error {
	return s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "musicbrainz",
		Releases: releases,
	}}, observed)
}
func (s *Store) ApplyReleaseBatches(ctx context.Context, artist Artist, batches []ReleaseBatch, observed time.Time) error {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	var savedReleases []syncedRelease
	spotifyObserved := false
	seenProviders := make(map[string]bool)
	for _, batch := range batches {
		provider := strings.ToLower(strings.TrimSpace(batch.Provider))
		if seenProviders[provider] {
			tx.Rollback()
			return fmt.Errorf("duplicate release batch for %s", provider)
		}
		seenProviders[provider] = true
		if provider == "spotify" {
			spotifyObserved = true
		}
		for _, release := range batch.Releases {
			var saved syncedRelease
			switch provider {
			case "musicbrainz":
				saved, err = saveMusicBrainzReleaseTx(ctx, tx, artist.ID, release, observed)
			case "spotify":
				saved, err = saveSpotifyReleaseTx(ctx, tx, artist.ID, release, observed)
			case "itunes":
				saved, err = saveITunesReleaseTx(ctx, tx, artist.ID, release, observed)
			default:
				err = fmt.Errorf("unsupported release provider %q", provider)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			savedReleases = append(savedReleases, saved)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id,baseline_synced_at,spotify_baseline_synced_at
		FROM follows WHERE artist_id=?`, artist.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	type follower struct {
		id              int64
		baseline        bool
		spotifyBaseline bool
	}
	var followers []follower
	for rows.Next() {
		var f follower
		var baseline, spotifyBaseline sql.NullString
		if err := rows.Scan(&f.id, &baseline, &spotifyBaseline); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		f.baseline = baseline.Valid
		f.spotifyBaseline = spotifyBaseline.Valid
		followers = append(followers, f)
	}
	rows.Close()
	for _, follower := range followers {
		if !follower.baseline {
			if selected, eventType, ok := selectInitialRelease(savedReleases, observed); ok {
				title, body := initialReleaseMessage(artist, selected.release, eventType, observed)
				if err := enqueueEventTx(ctx, tx, follower.id, selected.release.ID, eventType, title, body, observed); err != nil {
					tx.Rollback()
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE follows SET baseline_synced_at=? WHERE user_id=? AND artist_id=?`,
				timeText(observed), follower.id, artist.ID); err != nil {
				tx.Rollback()
				return err
			}
			if spotifyObserved {
				if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
					WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
					tx.Rollback()
					return err
				}
			}
			continue
		}
		for _, item := range savedReleases {
			if item.provider == "spotify" && !follower.spotifyBaseline {
				continue
			}
			date, full := releaseDate(item.release.FirstReleaseDate)
			if !item.isNew || !full || date.Before(dayUTC(observed).AddDate(0, 0, -7)) {
				continue
			}
			if err := enqueueEventTx(ctx, tx, follower.id, item.release.ID, "announcement",
				"New release from "+artist.Name,
				fmt.Sprintf("%s has announced %q for %s.\n%s", artist.Name, item.release.Title,
					item.release.FirstReleaseDate, releaseExternalURL(item.release)),
				observed); err != nil {
				tx.Rollback()
				return err
			}
		}
		if spotifyObserved && !follower.spotifyBaseline {
			if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
				WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}
func matchingReleaseIDTx(
	ctx context.Context, tx *sql.Tx, artistID int64, candidate Release, spotifyOnly bool,
) (int64, error) {
	sourceClause := "source IN ('musicbrainz','spotify','itunes','both')"
	if spotifyOnly {
		sourceClause = "source IN ('spotify','itunes')"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,title,primary_type,first_release_date,date_precision
		FROM release_groups WHERE artist_id=? AND `+sourceClause, artistID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var matches []int64
	for rows.Next() {
		var id int64
		var title, primaryType, releaseDate string
		var precision int
		if err := rows.Scan(&id, &title, &primaryType, &releaseDate, &precision); err != nil {
			return 0, err
		}
		existing := Release{
			Title: title, PrimaryType: primaryType, FirstReleaseDate: releaseDate, DatePrecision: precision,
		}
		if releaseRecordsMatch(existing, candidate) {
			matches = append(matches, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(matches) != 1 {
		return 0, sql.ErrNoRows
	}
	return matches[0], nil
}
func releaseRecordsMatch(a, b Release) bool {
	if a.PrimaryType != b.PrimaryType || normalizedReleaseTitle(a.Title) != normalizedReleaseTitle(b.Title) {
		return false
	}
	if a.DatePrecision == 0 || b.DatePrecision == 0 ||
		a.FirstReleaseDate == "" || b.FirstReleaseDate == "" {
		return false
	}
	length := min(len(a.FirstReleaseDate), len(b.FirstReleaseDate))
	if length < 4 {
		return false
	}
	return a.FirstReleaseDate[:length] == b.FirstReleaseDate[:length]
}
func (s *Store) QueueDueReleaseDays(ctx context.Context, now time.Time) error {
	today := dayUTC(now)
	from := today.AddDate(0, 0, -1).Format("2006-01-02")
	to := today.AddDate(0, 0, 1).Format("2006-01-02")
	rows, err := s.readerDB().QueryContext(ctx, `SELECT f.user_id,rg.id,u.timezone,u.reminder_time,a.name,rg.title,
		 rg.first_release_date,rg.musicbrainz_url,rg.spotify_url,rg.itunes_url
		FROM follows f JOIN users u ON u.id=f.user_id JOIN release_groups rg ON rg.artist_id=f.artist_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE rg.date_precision=3 AND rg.first_release_date BETWEEN ? AND ?`, from, to)
	if err != nil {
		return err
	}
	type due struct {
		userID, releaseID          int64
		timezone, reminder         string
		artist, title, releaseDate string
		musicBrainzURL             string
		spotifyURL, itunesURL      sql.NullString
	}
	var candidates []due
	for rows.Next() {
		var d due
		if err := rows.Scan(
			&d.userID, &d.releaseID, &d.timezone, &d.reminder, &d.artist, &d.title,
			&d.releaseDate, &d.musicBrainzURL, &d.spotifyURL, &d.itunesURL,
		); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, d)
	}
	rows.Close()
	for _, d := range candidates {
		location, err := time.LoadLocation(d.timezone)
		if err != nil {
			continue
		}
		localNow := now.In(location)
		if d.releaseDate != localNow.Format("2006-01-02") || localNow.Format("15:04") < d.reminder {
			continue
		}
		body := fmt.Sprintf("%s's %q is out today.", d.artist, d.title)
		if link := firstNonEmpty(d.spotifyURL.String, d.itunesURL.String, d.musicBrainzURL); link != "" {
			body += "\n" + link
		}
		if err := s.EnqueueEvent(ctx, d.userID, d.releaseID, "release_day",
			"Released today: "+d.title, body, now); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) EnqueueEvent(ctx context.Context, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	if err := enqueueEventTx(ctx, tx, userID, releaseID, eventType, title, body, now); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
func (s *Store) RecentReleases(ctx context.Context, userID int64, limit int) ([]Release, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReleases(rows)
}
func (s *Store) DashboardReleases(
	ctx context.Context, userID int64, today string, limit int,
) (upcoming []Release, recent []Release, err error) {
	const definitelyFuture = `(
		(rg.date_precision=3 AND length(rg.first_release_date)=10
			AND date(rg.first_release_date) IS NOT NULL AND rg.first_release_date>?)
		OR (rg.date_precision=2 AND length(rg.first_release_date)=7
			AND date(rg.first_release_date || '-01') IS NOT NULL AND rg.first_release_date>substr(?,1,7))
		OR (rg.date_precision=1 AND length(rg.first_release_date)=4
			AND date(rg.first_release_date || '-01-01') IS NOT NULL AND rg.first_release_date>substr(?,1,4))
	)`
	const preferredProvider = `((a.spotify_id IS NULL AND NOT EXISTS (SELECT 1 FROM release_groups external_release WHERE external_release.artist_id=rg.artist_id AND external_release.source IN ('spotify','itunes','both'))) OR rg.source IN ('spotify','itunes','both') OR NOT EXISTS (
		SELECT 1 FROM release_groups newer WHERE newer.artist_id=rg.artist_id AND newer.source IN ('spotify','itunes','both')
	))`
	upcomingRows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? AND `+preferredProvider+` AND `+definitelyFuture+`
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	upcoming, err = scanReleases(upcomingRows)
	upcomingRows.Close()
	if err != nil {
		return nil, nil, err
	}
	recentRows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? AND `+preferredProvider+` AND NOT COALESCE(`+definitelyFuture+`,0)
		ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC,rg.id DESC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	recent, err = scanReleases(recentRows)
	recentRows.Close()
	if err != nil {
		return nil, nil, err
	}
	return upcoming, recent, nil
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func releaseTypeEnabled(p NotificationPreferences, primary string) bool {
	switch strings.ToLower(strings.TrimSpace(primary)) {
	case "album":
		return p.Albums
	case "ep":
		return p.EPs
	case "single":
		return p.Singles
	default:
		return true
	}
}
func (s *Store) ReleaseDetail(ctx context.Context, userID, releaseID int64) (ReleaseDetail, error) {
	var d ReleaseDetail
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id WHERE f.user_id=? AND rg.id=?`, userID, releaseID)
	if err != nil {
		return d, err
	}
	items, err := scanReleases(rows)
	rows.Close()
	if err != nil {
		return d, err
	}
	if len(items) == 0 {
		return d, sql.ErrNoRows
	}
	d.Release = items[0]
	obs, err := s.readerDB().QueryContext(ctx, `SELECT provider,provider_id,observed_at FROM provider_observations WHERE release_group_id=? ORDER BY observed_at DESC`, releaseID)
	if err != nil {
		return d, err
	}
	defer obs.Close()
	for obs.Next() {
		var o ReleaseObservation
		var ts string
		if err := obs.Scan(&o.Provider, &o.ProviderID, &ts); err != nil {
			return d, err
		}
		o.ObservedAt, _ = parseTime(ts)
		d.Observations = append(d.Observations, o)
	}
	return d, obs.Err()
}
