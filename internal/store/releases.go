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
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		var savedReleases []syncedRelease
		savedIndexes := make(map[string]int)
		spotifyObserved := false
		seenProviders := make(map[string]bool)
		for _, batch := range batches {
			provider := strings.ToLower(strings.TrimSpace(batch.Provider))
			if seenProviders[provider] {
				_ = tx.Rollback()
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
					_ = tx.Rollback()
					return fmt.Errorf("save %s release: %w", provider, err)
				}
				if err := evaluateReleaseEvidenceTx(ctx, tx, saved.release.ID, observed); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("evaluate %s release evidence: %w", provider, err)
				}
				// One Spotify release can arrive through both the direct catalogue
				// and appears_on. Keep one notification candidate and let a primary
				// credit win even if Spotify returned the featured copy first.
				key := saved.provider + "\x00" + fmt.Sprint(saved.release.ID)
				if index, exists := savedIndexes[key]; exists {
					previous := savedReleases[index]
					saved.release.Credits = mergeReleaseCredits(previous.release.Credits, saved.release.Credits)
					saved.creditNew = previous.creditNew || saved.creditNew
					if previous.release.ArtistCreditRole == "primary" && saved.release.ArtistCreditRole == "featured" {
						previous.isNew = previous.isNew || saved.isNew
						previous.creditNew = previous.creditNew || saved.creditNew
						previous.release.Credits = saved.release.Credits
						savedReleases[index] = previous
					} else {
						saved.isNew = saved.isNew || previous.isNew
						saved.creditNew = saved.creditNew || previous.creditNew
						savedReleases[index] = saved
					}
				} else {
					savedIndexes[key] = len(savedReleases)
					savedReleases = append(savedReleases, saved)
				}
			}
		}
		// A later synchronization may have made previously conflicting evidence
		// agree again. Drain those holds only after every provider batch in this
		// transaction has been evaluated, so an intermediate provider cannot
		// release a notification before the rest of the batch is visible.
		for _, item := range savedReleases {
			if err := drainResolvedNotificationHoldsTx(ctx, tx, item.release.ID, observed); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("drain release notification holds: %w", err)
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.user_id,u.timezone,f.baseline_synced_at,f.spotify_baseline_synced_at,f.spotify_appears_on_baseline_synced_at
		FROM follows f JOIN users u ON u.id=f.user_id WHERE f.artist_id=?`, artist.ID)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		type follower struct {
			id                        int64
			timezone                  string
			baseline                  bool
			spotifyBaseline           bool
			spotifyAppearanceBaseline bool
		}
		var followers []follower
		for rows.Next() {
			var f follower
			var baseline, spotifyBaseline, spotifyAppearanceBaseline sql.NullString
			if err := rows.Scan(&f.id, &f.timezone, &baseline, &spotifyBaseline, &spotifyAppearanceBaseline); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return err
			}
			f.baseline = baseline.Valid
			f.spotifyBaseline = spotifyBaseline.Valid
			f.spotifyAppearanceBaseline = spotifyAppearanceBaseline.Valid
			followers = append(followers, f)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return err
		}
		_ = rows.Close()
		for _, follower := range followers {
			location := userLocation(follower.timezone)
			if !follower.baseline {
				if selected, eventType, ok := selectInitialReleaseInLocation(savedReleases, observed, location); ok {
					title, body := initialReleaseMessageInLocation(artist, selected.release, eventType, observed, location)
					if err := enqueueEventTx(ctx, tx, follower.id, selected.release.ID, eventType, title, body, observed); err != nil {
						_ = tx.Rollback()
						return fmt.Errorf("enqueue initial release event: %w", err)
					}
				}
				for _, item := range savedReleases {
					for _, role := range releaseCreditRoles(item.release, item.provider) {
						if _, err := ensureCreditBaselineTx(ctx, tx, follower.id, artist.ID, item.provider, role, observed); err != nil {
							_ = tx.Rollback()
							return err
						}
					}
				}
				if _, err := tx.ExecContext(ctx, `UPDATE follows SET baseline_synced_at=? WHERE user_id=? AND artist_id=?`,
					timeText(observed), follower.id, artist.ID); err != nil {
					_ = tx.Rollback()
					return err
				}
				if spotifyObserved {
					if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
					WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
						_ = tx.Rollback()
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_appears_on_baseline_synced_at=?
					WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
						_ = tx.Rollback()
						return err
					}
				}
				continue
			}
			for _, item := range savedReleases {
				if item.provider == "spotify" && !follower.spotifyBaseline {
					continue
				}
				for _, role := range releaseCreditRoles(item.release, item.provider) {
					if _, err := ensureCreditBaselineTx(ctx, tx, follower.id, artist.ID, item.provider, role, observed); err != nil {
						_ = tx.Rollback()
						return err
					}
				}
				date, full := releaseDate(item.release.FirstReleaseDate)
				if (!item.isNew && !item.creditNew) || !full || date.Before(dayUTC(observed).AddDate(0, 0, -7)) {
					continue
				}
				title, body := releaseAnnouncementMessage(artist, item.release)
				if err := enqueueEventTx(ctx, tx, follower.id, item.release.ID, "announcement", title, body, observed); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
			if spotifyObserved && !follower.spotifyBaseline {
				if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
				WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
			if spotifyObserved && !follower.spotifyAppearanceBaseline {
				if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_appears_on_baseline_synced_at=?
				WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		return nil
	})
}
func matchingReleaseIDTx(
	ctx context.Context, tx *sql.Tx, artistID int64, candidate Release, spotifyOnly bool,
) (int64, error) {
	// Provider IDs are the preferred identity. This fallback is deliberately
	// narrow: only records for the same artist, provider family, type, date,
	// and precision are candidates before the normalized title comparison.
	if candidate.DatePrecision == 0 || strings.TrimSpace(candidate.FirstReleaseDate) == "" ||
		strings.TrimSpace(candidate.PrimaryType) == "" {
		return 0, sql.ErrNoRows
	}
	sourceClause := "source IN ('musicbrainz','spotify','itunes','both')"
	if spotifyOnly {
		sourceClause = "source IN ('spotify','itunes')"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,title,primary_type,first_release_date,date_precision
		FROM release_groups WHERE artist_id=? AND `+sourceClause+`
		AND primary_type=? AND date_precision=? AND first_release_date=?`,
		artistID, candidate.PrimaryType, candidate.DatePrecision, candidate.FirstReleaseDate)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
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
	if a.DatePrecision == 0 || a.DatePrecision != b.DatePrecision {
		return false
	}
	length := map[int]int{1: 4, 2: 7, 3: 10}[a.DatePrecision]
	if length == 0 || len(a.FirstReleaseDate) != length || len(b.FirstReleaseDate) != length {
		return false
	}
	return a.FirstReleaseDate == b.FirstReleaseDate
}
func (s *Store) QueueDueReleaseDays(ctx context.Context, now time.Time) error {
	today := dayUTC(now)
	from := today.AddDate(0, 0, -1).Format("2006-01-02")
	to := today.AddDate(0, 0, 1).Format("2006-01-02")
	rows, err := s.readerDB().QueryContext(ctx, `SELECT u.id,rg.id,u.timezone,u.reminder_time,a.name,rg.title,
		 rg.first_release_date,rg.musicbrainz_url,rg.spotify_url,rg.itunes_url,rg.artist_credit_role,
		 (SELECT COUNT(*) FROM release_credits rc WHERE rc.release_group_id=rg.id AND rc.role='guest')
		FROM users u JOIN release_groups rg ON 1=1
		JOIN artists a ON a.id=rg.artist_id
		WHERE `+followedReleasePredicate("u.id")+` AND rg.date_precision=3 AND rg.first_release_date BETWEEN ? AND ?`, from, to)
	if err != nil {
		return err
	}
	type due struct {
		userID, releaseID          int64
		timezone, reminder         string
		artist, title, releaseDate string
		musicBrainzURL             string
		spotifyURL, itunesURL      sql.NullString
		artistCreditRole           string
		guestCreditCount           int
	}
	var candidates []due
	for rows.Next() {
		var d due
		if err := rows.Scan(
			&d.userID, &d.releaseID, &d.timezone, &d.reminder, &d.artist, &d.title,
			&d.releaseDate, &d.musicBrainzURL, &d.spotifyURL, &d.itunesURL, &d.artistCreditRole, &d.guestCreditCount,
		); err != nil {
			_ = rows.Close()
			return err
		}
		candidates = append(candidates, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
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
		title := "Released today: " + d.title
		if d.artistCreditRole == "featured" && d.guestCreditCount > 0 {
			body = fmt.Sprintf("%s is credited on %q, released today.", d.artist, d.title)
			title = "Guest appearance released today: " + d.title
		} else if d.artistCreditRole == "featured" {
			body = fmt.Sprintf("%s appears on %q, released today.", d.artist, d.title)
			title = "Featured appearance released today: " + d.title
		}
		if link := firstNonEmpty(d.spotifyURL.String, d.itunesURL.String, d.musicBrainzURL); link != "" {
			body += "\n" + link
		}
		if err := s.EnqueueEvent(ctx, d.userID, d.releaseID, "release_day", title, body, now); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) EnqueueEvent(ctx context.Context, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return enqueueEventTx(ctx, tx, userID, releaseID, eventType, title, body, now)
	})
}
func (s *Store) RecentReleases(ctx context.Context, userID int64, limit int) ([]Release, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE `+followedReleasePredicate("?")+` ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	upcomingRows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE `+followedReleasePredicate("?")+` AND `+preferredProvider+` AND `+definitelyFuture+`
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	upcoming, err = scanReleases(upcomingRows)
	_ = upcomingRows.Close()
	if err != nil {
		return nil, nil, err
	}
	recentRows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
		WHERE `+followedReleasePredicate("?")+` AND `+preferredProvider+` AND NOT COALESCE(`+definitelyFuture+`,0)
		ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC,rg.id DESC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	recent, err = scanReleases(recentRows)
	_ = recentRows.Close()
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
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id WHERE `+followedReleasePredicate("?")+` AND rg.id=?`, userID, releaseID)
	if err != nil {
		return d, err
	}
	items, err := scanReleases(rows)
	_ = rows.Close()
	if err != nil {
		return d, err
	}
	if len(items) == 0 {
		return d, sql.ErrNoRows
	}
	d.Release = items[0]
	d.Release.FollowedArtists, err = s.followedReleaseArtists(ctx, userID, releaseID)
	if err != nil {
		return d, err
	}
	obs, err := s.readerDB().QueryContext(ctx, `SELECT provider,provider_id,observed_at FROM provider_observations WHERE release_group_id=? ORDER BY observed_at DESC`, releaseID)
	if err != nil {
		return d, err
	}
	defer func() { _ = obs.Close() }()
	for obs.Next() {
		var o ReleaseObservation
		var ts string
		if err := obs.Scan(&o.Provider, &o.ProviderID, &ts); err != nil {
			return d, err
		}
		o.ObservedAt, err = parseStoredTime(ts, "release observation observed_at")
		if err != nil {
			return d, err
		}
		d.Observations = append(d.Observations, o)
	}
	if err := obs.Err(); err != nil {
		return d, err
	}
	credits, err := s.readerDB().QueryContext(ctx, `SELECT rc.id,rc.release_group_id,rc.artist_id,
		rc.provider,rc.provider_id,rc.role,rc.track_title,rc.credit_name,rc.provider_url,
		rc.confidence,rc.first_seen_at,rc.last_seen_at
		FROM release_credits rc JOIN follows f ON f.artist_id=rc.artist_id
		WHERE f.user_id=? AND rc.release_group_id=? ORDER BY rc.role,rc.provider,rc.track_title`, userID, releaseID)
	if err != nil {
		return d, err
	}
	defer func() { _ = credits.Close() }()
	for credits.Next() {
		var credit ReleaseCredit
		var firstSeen, lastSeen string
		if err := credits.Scan(&credit.ID, &credit.ReleaseGroupID, &credit.ArtistID, &credit.Provider,
			&credit.ProviderID, &credit.Role, &credit.TrackTitle, &credit.CreditName, &credit.ProviderURL,
			&credit.Confidence, &firstSeen, &lastSeen); err != nil {
			return d, err
		}
		credit.FirstSeenAt, err = parseStoredTime(firstSeen, "release credit first_seen_at")
		if err != nil {
			return d, err
		}
		credit.LastSeenAt, err = parseStoredTime(lastSeen, "release credit last_seen_at")
		if err != nil {
			return d, err
		}
		d.Credits = append(d.Credits, credit)
	}
	return d, credits.Err()
}

// ReleaseGroupVisibleByMBID reports whether a release group identified by a
// canonical MusicBrainz ID is visible to the member. Artwork requests use
// this owner-scoped check before contacting the Cover Art Archive so a member
// cannot turn the route into an arbitrary UUID fetch oracle.
func (s *Store) ReleaseGroupVisibleByMBID(ctx context.Context, userID int64, mbid string) (bool, error) {
	var exists int
	err := s.readerDB().QueryRowContext(ctx, `SELECT 1 FROM release_groups rg
		WHERE rg.mbid=? AND `+followedReleasePredicate("?")+` LIMIT 1`, mbid, userID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}
