package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

func saveMusicBrainzReleaseTx(
	ctx context.Context, tx *sql.Tx, artistID int64, release Release, observed time.Time,
) (syncedRelease, error) {
	if strings.TrimSpace(release.MBID) == "" {
		return syncedRelease{}, errors.New("MusicBrainz release group ID is required")
	}
	var releaseID int64
	existed := true
	err := tx.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, release.MBID).Scan(&releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		existed = false
		releaseID, err = matchingReleaseIDTx(ctx, tx, artistID, release, true)
		if errors.Is(err, sql.ErrNoRows) {
			releaseID = 0
			err = nil
		} else if err == nil {
			existed = true
			_, err = tx.ExecContext(ctx, `UPDATE release_groups SET mbid=?,source='both' WHERE id=?`,
				release.MBID, releaseID)
		}
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?, 'musicbrainz',?,?)`,
			release.MBID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			title=?,primary_type=?,secondary_types=?,
			first_release_date=CASE WHEN ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=MAX(date_precision,?),musicbrainz_url=?,
			source=CASE WHEN spotify_id IS NULL AND itunes_id IS NULL THEN 'musicbrainz' ELSE 'both' END,updated_at=?
			WHERE id=?`,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "musicbrainz", release.MBID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	return syncedRelease{release: saved, isNew: !existed, provider: "musicbrainz"}, err
}
func saveSpotifyReleaseTx(
	ctx context.Context, tx *sql.Tx, artistID int64, release Release, observed time.Time,
) (syncedRelease, error) {
	if strings.TrimSpace(release.SpotifyID) == "" {
		return syncedRelease{}, errors.New("Spotify release ID is required")
	}
	var releaseID int64
	existed := true
	err := tx.QueryRowContext(ctx, `SELECT release_group_id FROM provider_observations
		WHERE provider='spotify' AND provider_id=?`, release.SpotifyID).Scan(&releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE spotify_id=?`, release.SpotifyID).
			Scan(&releaseID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		releaseID, err = matchingReleaseIDTx(ctx, tx, artistID, release, false)
	}
	if errors.Is(err, sql.ErrNoRows) {
		existed, releaseID, err = false, 0, nil
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,spotify_id,spotify_url,spotify_image_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'spotify',?,?)`,
			"spotify:"+release.SpotifyID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, "", release.SpotifyID, release.SpotifyURL,
			release.SpotifyImageURL, timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			spotify_id=COALESCE(spotify_id,?),spotify_url=?,spotify_image_url=?,
			title=CASE WHEN source='spotify' THEN ? ELSE title END,
			primary_type=CASE WHEN source='spotify' THEN ? ELSE primary_type END,
			secondary_types=CASE WHEN source='spotify' THEN ? ELSE secondary_types END,
			first_release_date=CASE WHEN source='spotify' AND ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=CASE WHEN source='spotify' THEN MAX(date_precision,?) ELSE date_precision END,
			source=CASE WHEN source IN ('musicbrainz','itunes') THEN 'both' ELSE source END,updated_at=?
			WHERE id=?`,
			release.SpotifyID, release.SpotifyURL, release.SpotifyImageURL,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision,
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "spotify", release.SpotifyID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	return syncedRelease{release: saved, isNew: !existed, provider: "spotify"}, err
}
func saveITunesReleaseTx(
	ctx context.Context, tx *sql.Tx, artistID int64, release Release, observed time.Time,
) (syncedRelease, error) {
	if strings.TrimSpace(release.ITunesID) == "" {
		return syncedRelease{}, errors.New("iTunes release ID is required")
	}
	artworkURL := strings.TrimSpace(release.ITunesArtworkURL)
	if !validITunesArtworkURL(artworkURL) {
		artworkURL = ""
	}
	var releaseID int64
	existed := true
	err := tx.QueryRowContext(ctx, `SELECT release_group_id FROM provider_observations
		WHERE provider='itunes' AND provider_id=?`, release.ITunesID).Scan(&releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE itunes_id=?`, release.ITunesID).Scan(&releaseID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		releaseID, err = matchingReleaseIDTx(ctx, tx, artistID, release, false)
	}
	if errors.Is(err, sql.ErrNoRows) {
		existed, releaseID, err = false, 0, nil
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,itunes_id,itunes_url,itunes_artwork_url,itunes_artwork_checked_at,
			 itunes_artwork_next_check_at,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?, 'itunes',?,?)`,
			"itunes:"+release.ITunesID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, "", release.ITunesID, release.ITunesURL,
			artworkURL, timeText(observed), timeText(observed.Add(30*24*time.Hour)),
			timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			itunes_id=COALESCE(itunes_id,?),itunes_url=?,
			itunes_artwork_url=CASE WHEN ?>'' THEN ? ELSE itunes_artwork_url END,
			itunes_artwork_checked_at=?,
			itunes_artwork_next_check_at=CASE WHEN ?>'' THEN NULL ELSE ? END,
			title=CASE WHEN source='itunes' THEN ? ELSE title END,
			primary_type=CASE WHEN source='itunes' THEN ? ELSE primary_type END,
			secondary_types=CASE WHEN source='itunes' THEN ? ELSE secondary_types END,
			first_release_date=CASE WHEN source='itunes' AND ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=CASE WHEN source='itunes' THEN MAX(date_precision,?) ELSE date_precision END,
			source=CASE WHEN source IN ('musicbrainz','spotify') THEN 'both' ELSE source END,updated_at=?
			WHERE id=?`,
			release.ITunesID, release.ITunesURL, artworkURL, artworkURL,
			timeText(observed), artworkURL, timeText(observed.Add(30*24*time.Hour)),
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision,
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "itunes", release.ITunesID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	return syncedRelease{release: saved, isNew: !existed, provider: "itunes"}, err
}
func validITunesArtworkURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Path == "" || parsed.Port() != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "mzstatic.com" || strings.HasSuffix(host, ".mzstatic.com") ||
		host == "itunes.apple.com" || strings.HasSuffix(host, ".itunes.apple.com")
}
func normalizedReleaseTitle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}} {
		start := strings.LastIndex(value, pair[0])
		if start < 0 || !strings.HasSuffix(value, pair[1]) {
			continue
		}
		suffix := value[start:]
		for _, marker := range []string{"deluxe", "remaster", "expanded", "anniversary", "edition"} {
			if strings.Contains(suffix, marker) {
				value = strings.TrimSpace(value[:start])
				break
			}
		}
	}
	var normalized strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			normalized.WriteRune(r)
			space = false
		} else if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}
func upsertProviderObservationTx(
	ctx context.Context, tx *sql.Tx, provider, providerID string, releaseID int64,
	release Release, observed time.Time,
) error {
	payloadHash := releasePayloadHash(release)
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES(?,?,?,?,?)
		ON CONFLICT(provider,provider_id) DO UPDATE SET release_group_id=excluded.release_group_id,
		payload_hash=excluded.payload_hash,observed_at=excluded.observed_at`,
		provider, providerID, releaseID, payloadHash, timeText(observed))
	return err
}
func releasePayloadHash(release Release) string {
	secondary, _ := json.Marshal(release.SecondaryTypes)
	payloadHash := sha256.Sum256([]byte(release.Title + "\x00" + release.PrimaryType + "\x00" +
		string(secondary) + "\x00" + release.FirstReleaseDate + "\x00" + release.SpotifyURL + "\x00" +
		release.ITunesURL + "\x00" + release.ITunesArtworkURL))
	return fmt.Sprintf("%x", payloadHash)
}
func releaseByIDTx(ctx context.Context, tx *sql.Tx, releaseID int64) (Release, error) {
	var release Release
	var secondary, observed string
	var spotifyID, spotifyURL, spotifyImageURL, itunesID, itunesURL, itunesArtworkURL sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,mbid,artist_id,title,primary_type,secondary_types,
		first_release_date,date_precision,musicbrainz_url,spotify_id,spotify_url,spotify_image_url,
		itunes_id,itunes_url,itunes_artwork_url,source,first_observed_at FROM release_groups WHERE id=?`, releaseID).Scan(
		&release.ID, &release.MBID, &release.ArtistID, &release.Title, &release.PrimaryType, &secondary,
		&release.FirstReleaseDate, &release.DatePrecision, &release.MusicBrainzURL,
		&spotifyID, &spotifyURL, &spotifyImageURL, &itunesID, &itunesURL, &itunesArtworkURL, &release.Source, &observed,
	)
	if err != nil {
		return Release{}, err
	}
	_ = json.Unmarshal([]byte(secondary), &release.SecondaryTypes)
	release.SpotifyID, release.SpotifyURL, release.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImageURL.String
	release.ITunesID, release.ITunesURL = itunesID.String, itunesURL.String
	release.ITunesArtworkURL = itunesArtworkURL.String
	release.FirstObservedAt, _ = parseTime(observed)
	return release, nil
}
func selectInitialRelease(items []syncedRelease, observed time.Time) (syncedRelease, string, bool) {
	var zero syncedRelease
	today := dayUTC(observed)
	var upcoming syncedRelease
	var upcomingStart time.Time
	var latest syncedRelease
	var latestStart time.Time
	hasUpcoming, hasLatest := false, false
	for _, item := range items {
		release := item.release
		start, valid := comparableReleaseDate(release.FirstReleaseDate)
		if !valid {
			continue
		}
		if start.After(today) {
			if !hasUpcoming || start.Before(upcomingStart) ||
				(start.Equal(upcomingStart) && release.MBID < upcoming.release.MBID) {
				upcoming, upcomingStart, hasUpcoming = item, start, true
			}
			continue
		}
		if !hasLatest || start.After(latestStart) ||
			(start.Equal(latestStart) && release.MBID < latest.release.MBID) {
			latest, latestStart, hasLatest = item, start, true
		}
	}
	if hasUpcoming {
		return upcoming, "announcement", true
	}
	if !hasLatest {
		return zero, "", false
	}
	release := latest.release
	if release.DatePrecision == 3 && release.FirstReleaseDate == today.Format("2006-01-02") {
		return latest, "release_day", true
	}
	return latest, "announcement", true
}
func initialReleaseMessage(artist Artist, release Release, eventType string, observed time.Time) (string, string) {
	today := dayUTC(observed)
	start, _ := comparableReleaseDate(release.FirstReleaseDate)
	link := releaseExternalURL(release)
	if eventType == "release_day" {
		return "Released today: " + release.Title,
			fmt.Sprintf("%s's %q is out today.\n%s", artist.Name, release.Title, link)
	}
	if start.After(today) {
		return "Upcoming release from " + artist.Name,
			fmt.Sprintf("%s's %q is expected %s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, link)
	}
	return "Latest release from " + artist.Name,
		fmt.Sprintf("%s's latest known release is %q (%s).\n%s", artist.Name, release.Title,
			release.FirstReleaseDate, link)
}
func releaseExternalURL(release Release) string {
	if release.SpotifyURL != "" {
		return release.SpotifyURL
	}
	if release.ITunesURL != "" {
		return release.ITunesURL
	}
	return release.MusicBrainzURL
}
func comparableReleaseDate(value string) (time.Time, bool) {
	layout := ""
	switch len(value) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
		layout = "2006-01-02"
	default:
		return time.Time{}, false
	}
	parsed, err := time.Parse(layout, value)
	return parsed, err == nil
}
func enqueueEventTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	var p NotificationPreferences
	var albums, eps, singles, announcements, releaseDay int
	var primary string
	err := tx.QueryRowContext(ctx, `SELECT p.albums,p.eps,p.singles,p.announcements,p.release_day,rg.primary_type FROM notification_preferences p JOIN release_groups rg ON rg.id=? WHERE p.user_id=?`, releaseID, userID).Scan(&albums, &eps, &singles, &announcements, &releaseDay, &primary)
	if err == nil {
		p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = albums != 0, eps != 0, singles != 0, announcements != 0, releaseDay != 0
		if !releaseTypeEnabled(p, primary) || (eventType == "announcement" && !p.Announcements) || (eventType == "release_day" && !p.ReleaseDay) {
			return nil
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, eventType, title, body, timeText(now))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,next_attempt_at)
		SELECT ?,id,'pending',? FROM destinations WHERE user_id=? AND enabled=1`, eventID, timeText(now), userID)
	return err
}
func scanReleases(rows *sql.Rows) ([]Release, error) {
	var result []Release
	for rows.Next() {
		var r Release
		var secondary, observed string
		var spotifyID, spotifyURL, spotifyImageURL, itunesID, itunesURL, itunesArtworkURL sql.NullString
		if err := rows.Scan(&r.ID, &r.MBID, &r.ArtistID, &r.ArtistName, &r.Title, &r.PrimaryType,
			&secondary, &r.FirstReleaseDate, &r.DatePrecision, &r.MusicBrainzURL,
			&spotifyID, &spotifyURL, &spotifyImageURL, &itunesID, &itunesURL, &itunesArtworkURL, &r.Source, &observed); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(secondary), &r.SecondaryTypes)
		r.SpotifyID, r.SpotifyURL, r.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImageURL.String
		r.ITunesID, r.ITunesURL = itunesID.String, itunesURL.String
		r.ITunesArtworkURL = itunesArtworkURL.String
		r.FirstObservedAt, _ = parseTime(observed)
		result = append(result, r)
	}
	return result, rows.Err()
}
func releaseDate(value string) (time.Time, bool) {
	if len(value) != 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", value)
	return t, err == nil
}
func dayUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
