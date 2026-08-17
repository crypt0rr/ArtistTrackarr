package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
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
			_, err = tx.ExecContext(ctx, `UPDATE release_groups SET mbid=?,source='both',
				artist_credit_role=CASE WHEN ?='primary' THEN 'primary' ELSE artist_credit_role END WHERE id=?`,
				release.MBID, normalizedArtistCreditRole(release.ArtistCreditRole), releaseID)
		}
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,artist_credit_role,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,'musicbrainz',?,?)`,
			release.MBID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			normalizedArtistCreditRole(release.ArtistCreditRole), timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			title=?,primary_type=?,secondary_types=?,
			first_release_date=CASE WHEN ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=MAX(date_precision,?),musicbrainz_url=?,
			artist_credit_role=CASE WHEN ?='primary' THEN 'primary' ELSE artist_credit_role END,
			source=CASE WHEN spotify_id IS NULL AND itunes_id IS NULL THEN 'musicbrainz' ELSE 'both' END,updated_at=?
			WHERE id=?`,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			normalizedArtistCreditRole(release.ArtistCreditRole),
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "musicbrainz", release.MBID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	creditNew, err := upsertReleaseCreditsTx(ctx, tx, "musicbrainz", artistID, releaseID, release, observed)
	if err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	saved.Credits = append([]ReleaseCredit(nil), release.Credits...)
	return syncedRelease{release: saved, isNew: !existed, creditNew: creditNew, provider: "musicbrainz"}, err
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
			 musicbrainz_url,spotify_id,spotify_url,spotify_image_url,artist_credit_role,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'spotify',?,?)`,
			"spotify:"+release.SpotifyID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, "", release.SpotifyID, release.SpotifyURL,
			release.SpotifyImageURL, normalizedArtistCreditRole(release.ArtistCreditRole), timeText(observed), timeText(observed))
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
			artist_credit_role=CASE WHEN artist_credit_role='primary' OR ?='primary' THEN 'primary' ELSE 'featured' END,
			source=CASE WHEN source IN ('musicbrainz','itunes') THEN 'both' ELSE source END,updated_at=?
			WHERE id=?`,
			release.SpotifyID, release.SpotifyURL, release.SpotifyImageURL,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision,
			normalizedArtistCreditRole(release.ArtistCreditRole), timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "spotify", release.SpotifyID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	creditNew, err := upsertReleaseCreditsTx(ctx, tx, "spotify", artistID, releaseID, release, observed)
	if err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	saved.Credits = append([]ReleaseCredit(nil), release.Credits...)
	return syncedRelease{release: saved, isNew: !existed, creditNew: creditNew, provider: "spotify"}, err
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
			 itunes_artwork_next_check_at,artist_credit_role,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'itunes',?,?)`,
			"itunes:"+release.ITunesID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, "", release.ITunesID, release.ITunesURL,
			artworkURL, timeText(observed), timeText(observed.Add(30*24*time.Hour)), normalizedArtistCreditRole(release.ArtistCreditRole),
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
			artist_credit_role=CASE WHEN artist_credit_role='primary' OR ?='primary' THEN 'primary' ELSE 'featured' END,
			source=CASE WHEN source IN ('musicbrainz','spotify') THEN 'both' ELSE source END,updated_at=?
			WHERE id=?`,
			release.ITunesID, release.ITunesURL, artworkURL, artworkURL,
			timeText(observed), artworkURL, timeText(observed.Add(30*24*time.Hour)),
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision,
			normalizedArtistCreditRole(release.ArtistCreditRole),
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "itunes", release.ITunesID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	creditNew, err := upsertReleaseCreditsTx(ctx, tx, "itunes", artistID, releaseID, release, observed)
	if err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	saved.Credits = append([]ReleaseCredit(nil), release.Credits...)
	return syncedRelease{release: saved, isNew: !existed, creditNew: creditNew, provider: "itunes"}, err
}

// upsertReleaseCreditsTx persists provider-backed credit evidence without
// deleting older observations. The returned flag is true only when a new
// non-primary credit was discovered, which lets the existing notification
// pipeline alert on a newly discovered collaboration without treating every
// unchanged provider response as a new release.
func upsertReleaseCreditsTx(
	ctx context.Context, tx *sql.Tx, provider string, artistID, releaseID int64,
	release Release, observed time.Time,
) (bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	providerID := releaseProviderID(provider, release)
	if providerID == "" {
		return false, nil
	}
	credits := append([]ReleaseCredit(nil), release.Credits...)
	if len(credits) == 0 {
		role := normalizedCreditRole(release.ArtistCreditRole)
		credits = []ReleaseCredit{{Role: role, ProviderID: providerID, ProviderURL: releaseProviderURL(provider, release)}}
	}
	creditNew := false
	for _, credit := range credits {
		role := normalizedCreditRole(credit.Role)
		creditID := strings.TrimSpace(credit.ProviderID)
		if creditID == "" {
			creditID = providerID
		}
		if creditID == "" {
			continue
		}
		trackTitle := strings.TrimSpace(credit.TrackTitle)
		creditName := strings.TrimSpace(credit.CreditName)
		providerURL := strings.TrimSpace(credit.ProviderURL)
		if providerURL == "" {
			providerURL = releaseProviderURL(provider, release)
		}
		confidence := strings.ToLower(strings.TrimSpace(credit.Confidence))
		if confidence != "probable" && confidence != "inferred" {
			confidence = "confirmed"
		}
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM release_credits
			WHERE release_group_id=? AND artist_id=? AND provider=? AND provider_id=? AND role=? AND track_title=? LIMIT 1`,
			releaseID, artistID, provider, creditID, role, trackTitle).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			if role != "primary" {
				creditNew = true
			}
		} else if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO release_credits(
			release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,
			provider_url,confidence,first_seen_at,last_seen_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(release_group_id,artist_id,provider,provider_id,role,track_title)
			DO UPDATE SET credit_name=CASE WHEN excluded.credit_name<>'' THEN excluded.credit_name ELSE release_credits.credit_name END,
			provider_url=CASE WHEN excluded.provider_url<>'' THEN excluded.provider_url ELSE release_credits.provider_url END,
			confidence=excluded.confidence,last_seen_at=excluded.last_seen_at`,
			releaseID, artistID, provider, creditID, role, trackTitle, creditName,
			providerURL, confidence, timeText(observed), timeText(observed)); err != nil {
			return false, err
		}
	}
	return creditNew, nil
}

func releaseProviderID(provider string, release Release) string {
	switch provider {
	case "spotify":
		return strings.TrimSpace(release.SpotifyID)
	case "itunes":
		return strings.TrimSpace(release.ITunesID)
	case "musicbrainz":
		return strings.TrimSpace(release.MBID)
	default:
		return ""
	}
}

func releaseProviderURL(provider string, release Release) string {
	switch provider {
	case "spotify":
		return strings.TrimSpace(release.SpotifyURL)
	case "itunes":
		return strings.TrimSpace(release.ITunesURL)
	case "musicbrainz":
		return strings.TrimSpace(release.MusicBrainzURL)
	default:
		return ""
	}
}

func normalizedCreditRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "guest":
		return "guest"
	case "featured", "appearance", "appears_on":
		return "featured"
	default:
		return "primary"
	}
}

func releaseHasNonPrimaryCredit(release Release) bool {
	if normalizedCreditRole(release.ArtistCreditRole) != "primary" {
		return true
	}
	for _, credit := range release.Credits {
		if normalizedCreditRole(credit.Role) != "primary" {
			return true
		}
	}
	return false
}

func releaseCreditRoles(release Release, provider string) []string {
	roles := make(map[string]bool)
	for _, credit := range release.Credits {
		if strings.TrimSpace(credit.Provider) != "" && !strings.EqualFold(strings.TrimSpace(credit.Provider), provider) {
			continue
		}
		role := normalizedCreditRole(credit.Role)
		if role != "primary" {
			roles[role] = true
		}
	}
	if len(roles) == 0 && releaseHasNonPrimaryCredit(release) {
		roles[normalizedCreditRole(release.ArtistCreditRole)] = true
	}
	result := make([]string, 0, len(roles))
	for role := range roles {
		result = append(result, role)
	}
	sort.Strings(result)
	return result
}

func mergeReleaseCredits(existing, incoming []ReleaseCredit) []ReleaseCredit {
	result := append([]ReleaseCredit(nil), existing...)
	seen := make(map[string]bool, len(result))
	for _, credit := range result {
		seen[strings.ToLower(strings.TrimSpace(credit.Provider))+"\x00"+strings.TrimSpace(credit.ProviderID)+"\x00"+
			strings.ToLower(strings.TrimSpace(credit.Role))+"\x00"+strings.TrimSpace(credit.TrackTitle)] = true
	}
	for _, credit := range incoming {
		key := strings.ToLower(strings.TrimSpace(credit.Provider)) + "\x00" + strings.TrimSpace(credit.ProviderID) + "\x00" +
			strings.ToLower(strings.TrimSpace(credit.Role)) + "\x00" + strings.TrimSpace(credit.TrackTitle)
		if key == "\x00\x00\x00" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, credit)
	}
	return result
}

// ensureCreditBaselineTx returns true when this call created the baseline.
// Baselines are owner-scoped and provider/role-specific so one provider's
// historical backlog cannot suppress a newly observed credit from another.
func ensureCreditBaselineTx(ctx context.Context, tx *sql.Tx, userID, artistID int64,
	provider, role string, observed time.Time) (bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	role = normalizedCreditRole(role)
	if role == "primary" || (provider != "spotify" && provider != "itunes" && provider != "musicbrainz") {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follow_credit_baselines
		(user_id,artist_id,provider,role,baseline_synced_at) VALUES(?,?,?,?,?)`,
		userID, artistID, provider, role, timeText(observed))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
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

func normalizedArtistCreditRole(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "featured") {
		return "featured"
	}
	return "primary"
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
	if err != nil {
		return err
	}
	return upsertReleaseProviderEvidenceTx(ctx, tx, provider, providerID, releaseID, release, observed)
}

func upsertReleaseProviderEvidenceTx(
	ctx context.Context, tx *sql.Tx, provider, providerID string, releaseID int64,
	release Release, observed time.Time,
) error {
	providerURL := ""
	switch provider {
	case "spotify":
		providerURL = release.SpotifyURL
	case "itunes":
		providerURL = release.ITunesURL
	case "musicbrainz":
		providerURL = release.MusicBrainzURL
	default:
		return fmt.Errorf("unsupported evidence provider %q", provider)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO release_provider_evidence
		(provider,provider_id,release_group_id,title,primary_type,first_release_date,date_precision,provider_url,artist_credit_role,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,provider_id) DO UPDATE SET release_group_id=excluded.release_group_id,
		title=excluded.title,primary_type=excluded.primary_type,first_release_date=excluded.first_release_date,
		date_precision=excluded.date_precision,provider_url=excluded.provider_url,
		artist_credit_role=excluded.artist_credit_role,observed_at=excluded.observed_at`,
		provider, providerID, releaseID, release.Title, release.PrimaryType, release.FirstReleaseDate,
		release.DatePrecision, strings.TrimSpace(providerURL), normalizedArtistCreditRole(release.ArtistCreditRole), timeText(observed))
	return err
}
func releasePayloadHash(release Release) string {
	secondary, _ := json.Marshal(release.SecondaryTypes)
	credits, _ := json.Marshal(release.Credits)
	payloadHash := sha256.Sum256([]byte(release.Title + "\x00" + release.PrimaryType + "\x00" +
		string(secondary) + "\x00" + release.FirstReleaseDate + "\x00" + release.SpotifyURL + "\x00" +
		release.ITunesURL + "\x00" + release.ITunesArtworkURL + "\x00" + normalizedArtistCreditRole(release.ArtistCreditRole) +
		"\x00" + string(credits)))
	return fmt.Sprintf("%x", payloadHash)
}
func releaseByIDTx(ctx context.Context, tx *sql.Tx, releaseID int64) (Release, error) {
	// Keep transaction reads byte-for-byte aligned with dashboard/inbox reads.
	// This avoids silently dropping nullable provider, truth, guest-credit, or
	// artwork fields when a newly saved release is returned to the caller.
	return scanReleaseWithExtra(tx.QueryRowContext(ctx,
		`SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id WHERE rg.id=?`, releaseID))
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
	creditRole, trackTitle := releaseMessageCredit(release)
	if eventType == "release_day" {
		if creditRole == "guest" {
			return "Guest appearance released today: " + release.Title,
				fmt.Sprintf("%s is credited on %q, released today%s.\n%s", artist.Name, release.Title, creditTrackSuffix(trackTitle), link)
		}
		if creditRole == "featured" {
			return "Featured appearance released today: " + release.Title,
				fmt.Sprintf("%s appears on %q, released today.\n%s", artist.Name, release.Title, link)
		}
		return "Released today: " + release.Title,
			fmt.Sprintf("%s's %q is out today.\n%s", artist.Name, release.Title, link)
	}
	if start.After(today) {
		if creditRole == "guest" {
			return "Upcoming guest appearance from " + artist.Name,
				fmt.Sprintf("%s is credited on %q, expected %s%s.\n%s", artist.Name, release.Title,
					release.FirstReleaseDate, creditTrackSuffix(trackTitle), link)
		}
		if creditRole == "featured" {
			return "Upcoming featured appearance from " + artist.Name,
				fmt.Sprintf("%s appears on %q, expected %s.\n%s", artist.Name, release.Title,
					release.FirstReleaseDate, link)
		}
		return "Upcoming release from " + artist.Name,
			fmt.Sprintf("%s's %q is expected %s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, link)
	}
	if creditRole == "guest" {
		return "Latest guest appearance from " + artist.Name,
			fmt.Sprintf("%s is credited on %q (%s)%s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, creditTrackSuffix(trackTitle), link)
	}
	if creditRole == "featured" {
		return "Latest featured appearance from " + artist.Name,
			fmt.Sprintf("%s appears on %q (%s).\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, link)
	}
	return "Latest release from " + artist.Name,
		fmt.Sprintf("%s's latest known release is %q (%s).\n%s", artist.Name, release.Title,
			release.FirstReleaseDate, link)
}

func releaseAnnouncementMessage(artist Artist, release Release) (string, string) {
	creditRole, trackTitle := releaseMessageCredit(release)
	if creditRole == "guest" {
		return "New guest appearance from " + artist.Name,
			fmt.Sprintf("%s is credited on %q for %s%s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, creditTrackSuffix(trackTitle), releaseExternalURL(release))
	}
	if creditRole == "featured" {
		return "New featured appearance from " + artist.Name,
			fmt.Sprintf("%s appears on %q for %s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, releaseExternalURL(release))
	}
	return "New release from " + artist.Name,
		fmt.Sprintf("%s has announced %q for %s.\n%s", artist.Name, release.Title,
			release.FirstReleaseDate, releaseExternalURL(release))
}

func releaseMessageCredit(release Release) (role, trackTitle string) {
	role = normalizedCreditRole(release.ArtistCreditRole)
	// The release-level primary projection intentionally wins when Spotify
	// exposes the same item through both its catalogue and appears_on. The
	// graph still retains both evidence rows, but the user should not receive a
	// guest label for their own primary release.
	if role == "primary" {
		return role, ""
	}
	for _, credit := range release.Credits {
		candidate := normalizedCreditRole(credit.Role)
		if candidate == "guest" {
			return "guest", strings.TrimSpace(credit.TrackTitle)
		}
		if candidate == "featured" {
			role = "featured"
		}
	}
	return role, ""
}

func creditTrackSuffix(trackTitle string) string {
	trackTitle = strings.TrimSpace(trackTitle)
	if trackTitle == "" {
		return ""
	}
	return fmt.Sprintf(" on the track %q", trackTitle)
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
	return enqueueEventTxMode(ctx, tx, userID, releaseID, eventType, title, body, now, false)
}

// enqueueApprovedEventTx queues an event after an explicit evidence/truth
// approval. It follows the same owner, preference, follow-rule, and delivery
// admission checks as normal events, but bypasses only the conflict-hold gate
// that the approval is intended to resolve.
func enqueueApprovedEventTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	return enqueueEventTxMode(ctx, tx, userID, releaseID, eventType, title, body, now, true)
}

func enqueueEventTxMode(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time, bypassConflictHold bool) error {
	var p NotificationPreferences
	var albums, eps, singles, announcements, releaseDay, holdConflicts int
	var primary, role, mode string
	var includePrimary, includeFeatured int
	var ruleAlbums, ruleEPs, ruleSingles, compilations, ruleAnnouncements, ruleReleaseDay int
	var paused, updated sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(p.albums,1),COALESCE(p.eps,1),COALESCE(p.singles,1),
		COALESCE(p.announcements,1),COALESCE(p.release_day,1),COALESCE(p.hold_conflicting_notifications,0),
		rg.primary_type,COALESCE(rg.artist_credit_role,'primary'),
		COALESCE(nr.delivery_mode,'inherit'),COALESCE(nr.include_primary,1),COALESCE(nr.include_featured,1),
		COALESCE(nr.albums,1),COALESCE(nr.eps,1),COALESCE(nr.singles,1),COALESCE(nr.compilations,1),
		COALESCE(nr.announcements,1),COALESCE(nr.release_day,1),nr.paused_until,COALESCE(nr.updated_at,'')
		FROM release_groups rg
		JOIN follows f ON f.user_id=? AND (f.artist_id=rg.artist_id OR EXISTS (
			SELECT 1 FROM release_credits owner_credit
			WHERE owner_credit.release_group_id=rg.id AND owner_credit.artist_id=f.artist_id
		))
		LEFT JOIN notification_preferences p ON p.user_id=?
		LEFT JOIN follow_notification_rules nr ON nr.user_id=? AND nr.artist_id=f.artist_id
		WHERE rg.id=?
		ORDER BY CASE WHEN f.artist_id=rg.artist_id THEN 0 ELSE 1 END LIMIT 1`, userID, userID, userID, releaseID).Scan(&albums, &eps, &singles, &announcements, &releaseDay,
		&holdConflicts, &primary, &role, &mode, &includePrimary, &includeFeatured, &ruleAlbums, &ruleEPs, &ruleSingles,
		&compilations, &ruleAnnouncements, &ruleReleaseDay, &paused, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The owner no longer follows this artist (or the release was
			// removed). Do not create an orphaned notification event.
			return nil
		}
		return fmt.Errorf("load notification rule for release: %w", err)
	}
	p.Albums, p.EPs, p.Singles, p.Announcements, p.ReleaseDay = albums != 0, eps != 0, singles != 0, announcements != 0, releaseDay != 0
	p.HoldConflictingNotifications = holdConflicts != 0
	rule, err := followRuleFromColumns(mode, includePrimary, includeFeatured, ruleAlbums, ruleEPs, ruleSingles, compilations, ruleAnnouncements, ruleReleaseDay, paused.String, updated.String, userID, 0)
	if err != nil {
		return err
	}
	if !releaseTypeEnabled(p, primary) || !rule.AllowsContent(primary, role, eventType, now) {
		return nil
	}
	title, body, err = decorateReleaseMessageTx(ctx, tx, userID, releaseID, title, body)
	if err != nil {
		return err
	}
	if p.HoldConflictingNotifications && rule.queuesImmediate(now) && !bypassConflictHold {
		if err := holdConflictingNotificationTx(ctx, tx, userID, releaseID, eventType, title, body, now); err != nil {
			return err
		}
		return nil
	}
	return insertNotificationEventTxMode(ctx, tx, userID, releaseID, eventType, title, body, now, rule.queuesImmediate(now))
}

func decorateReleaseMessageTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, title, body string) (string, string, error) {
	var names sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT `+followedReleaseCreditNames+` FROM release_groups rg WHERE rg.id=?`, userID, releaseID).Scan(&names)
	if err != nil {
		return title, body, fmt.Errorf("decorate release message: %w", err)
	}
	if !names.Valid || strings.TrimSpace(names.String) == "" {
		return title, body, nil
	}
	return title, strings.TrimSpace(body) + "\nFollowed artist association(s): " + names.String, nil
}

func insertNotificationEventTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	return insertNotificationEventTxMode(ctx, tx, userID, releaseID, eventType, title, body, now, true)
}

func insertNotificationEventTxMode(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time, queueDeliveries bool) error {
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, eventType, title, body, timeText(now))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	eventID, _ := result.LastInsertId()
	if n == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM notification_events WHERE user_id=? AND release_group_id=? AND event_type=?`, userID, releaseID, eventType).Scan(&eventID); err != nil {
			return err
		}
	}
	if !queueDeliveries {
		return nil
	}
	// The event insert is idempotent, so the delivery fan-out must be too. A
	// repeated release-day queue run should never turn an already queued event
	// into a duplicate-delivery error (or duplicate rows).
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries(event_id,destination_id,status,next_attempt_at)
		SELECT ?,d.id,`+destinationQueueStatus("d")+`,? FROM destinations d
		LEFT JOIN destination_health dh ON dh.destination_id=d.id
		WHERE d.user_id=? AND d.enabled=1`, eventID, timeText(now), userID)
	return err
}

type releaseRowScanner interface {
	Scan(dest ...any) error
}

func scanReleaseWithExtra(row releaseRowScanner, extra ...any) (Release, error) {
	var r Release
	var secondary, observed string
	var sourceCount int
	var observedProviders, lastObserved sql.NullString
	var truthState, truthProvider, truthProviderID, truthReason, truthUpdatedAt sql.NullString
	var truthIssueCount int
	var guestCreditCount int
	var spotifyID, spotifyURL, spotifyImageURL, itunesID, itunesURL, itunesArtworkURL, artistCreditRole sql.NullString
	destinations := []any{&r.ID, &r.MBID, &r.ArtistID, &r.ArtistName, &r.Title, &r.PrimaryType,
		&secondary, &r.FirstReleaseDate, &r.DatePrecision, &r.MusicBrainzURL,
		&spotifyID, &spotifyURL, &spotifyImageURL, &itunesID, &itunesURL, &itunesArtworkURL, &artistCreditRole, &r.Source, &observed,
		&sourceCount, &observedProviders, &lastObserved, &truthState, &truthProvider, &truthProviderID,
		&truthReason, &truthUpdatedAt, &truthIssueCount, &guestCreditCount}
	destinations = append(destinations, extra...)
	if err := row.Scan(destinations...); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(secondary), &r.SecondaryTypes); err != nil {
		return r, err
	}
	r.SpotifyID, r.SpotifyURL, r.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImageURL.String
	r.ITunesID, r.ITunesURL = itunesID.String, itunesURL.String
	r.ITunesArtworkURL = itunesArtworkURL.String
	r.ArtistCreditRole = normalizedArtistCreditRole(artistCreditRole.String)
	firstObserved, parseErr := parseStoredTime(observed, "release first_observed_at")
	if parseErr != nil {
		return r, parseErr
	}
	r.FirstObservedAt = firstObserved
	r.SourceCount = sourceCount
	r.Sources = splitReleaseProviders(observedProviders.String)
	r.Confidence = releaseConfidence(r.Source, sourceCount)
	r.TruthProvider = truthProvider.String
	r.TruthProviderID = truthProviderID.String
	r.TruthReason = truthReason.String
	r.TruthUpdatedAt, parseErr = parseTruthUpdatedAt(truthUpdatedAt)
	if parseErr != nil {
		return r, parseErr
	}
	r.TruthIssueCount = truthIssueCount
	r.GuestCreditCount = guestCreditCount
	r.TruthState = releaseTruthState(truthState.String, r.Source, sourceCount, r.Sources, truthIssueCount)
	if r.LastObservedAt, parseErr = parseNullableStatusTime(lastObserved, "release last_observed_at"); parseErr != nil {
		return r, parseErr
	}
	return r, nil
}

func scanRelease(row releaseRowScanner) (Release, error) {
	return scanReleaseWithExtra(row)
}

func scanReleases(rows *sql.Rows) ([]Release, error) {
	var result []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func splitReleaseProviders(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	providers := strings.Split(value, ",")
	sort.Strings(providers)
	return providers
}

func releaseConfidence(source string, sourceCount int) string {
	if sourceCount >= 2 || source == "both" {
		return "confirmed"
	}
	if source == "musicbrainz" {
		return "canonical"
	}
	if source == "spotify" {
		return "spotify"
	}
	if source == "itunes" {
		return "itunes"
	}
	if sourceCount == 1 {
		return "unconfirmed"
	}
	return "unconfirmed"
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
