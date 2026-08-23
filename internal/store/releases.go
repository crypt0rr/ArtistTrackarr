package store

import (
	"context"
	"database/sql"
	"errors"
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
		if err := func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var f follower
				var baseline, spotifyBaseline, spotifyAppearanceBaseline sql.NullString
				if err := rows.Scan(&f.id, &f.timezone, &baseline, &spotifyBaseline, &spotifyAppearanceBaseline); err != nil {
					return err
				}
				f.baseline = baseline.Valid
				f.spotifyBaseline = spotifyBaseline.Valid
				f.spotifyAppearanceBaseline = spotifyAppearanceBaseline.Valid
				followers = append(followers, f)
			}
			return rows.Err()
		}(); err != nil {
			_ = tx.Rollback()
			return err
		}
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
				// Keep the seven-day discovery window for normal polling, but
				// widen it to the previous successful catalog check after a
				// downtime gap. This catches a release first observed after an
				// outage without turning old back-catalogue imports into alerts.
				cutoff := dayUTC(observed).AddDate(0, 0, -7)
				if artist.LastCheckedAt != nil {
					previousCheck := dayUTC(*artist.LastCheckedAt)
					if previousCheck.Before(cutoff) {
						cutoff = previousCheck
					}
				}
				if (!item.isNew && !item.creditNew) || !full || date.Before(cutoff) {
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
	// narrow: only records for the same artist, provider family, date, and
	// precision are candidates before the normalized title comparison. Spotify
	// and iTunes derive Album/EP/Single from imperfect provider metadata, so a
	// title/date match may be promoted across derived types. Exact type matches
	// are always preferred and an ambiguous result is rejected.
	if candidate.DatePrecision == 0 || strings.TrimSpace(candidate.FirstReleaseDate) == "" ||
		strings.TrimSpace(candidate.PrimaryType) == "" {
		return 0, sql.ErrNoRows
	}
	sourceClause := "source IN ('musicbrainz','spotify','itunes','both')"
	if spotifyOnly {
		sourceClause = "source IN ('spotify','itunes')"
	}
	dateClause := "date_precision=? AND first_release_date=?"
	dateArgs := []any{candidate.DatePrecision, candidate.FirstReleaseDate}
	if candidate.DatePrecision == 3 {
		// Regional providers can differ by a few days. Keep the candidate set
		// narrow by artist/source/precision and a small day-level window before
		// comparing normalized titles.
		dateClause = "date_precision=? AND date(first_release_date) BETWEEN date(?, '-3 day') AND date(?, '+3 day')"
		dateArgs = []any{candidate.DatePrecision, candidate.FirstReleaseDate, candidate.FirstReleaseDate}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,title,primary_type,first_release_date,date_precision,source
		FROM release_groups WHERE artist_id=? AND `+sourceClause+` AND `+dateClause,
		append([]any{artistID}, dateArgs...)...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	allowDerivedTypeMismatch := spotifyOnly || providerReleaseTypeDerived(candidate)
	var exactMatches, derivedMatches []int64
	for rows.Next() {
		var id int64
		var title, primaryType, releaseDate, source string
		var precision int
		if err := rows.Scan(&id, &title, &primaryType, &releaseDate, &precision, &source); err != nil {
			return 0, err
		}
		existing := Release{
			Title: title, PrimaryType: primaryType, FirstReleaseDate: releaseDate, DatePrecision: precision, Source: source,
		}
		if releaseRecordsMatch(existing, candidate) {
			exactMatches = append(exactMatches, id)
		} else if allowDerivedTypeMismatch && releaseIdentityMatches(existing, candidate) {
			derivedMatches = append(derivedMatches, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(exactMatches) == 1 {
		return exactMatches[0], nil
	}
	if len(exactMatches) > 1 || len(derivedMatches) != 1 {
		return 0, sql.ErrNoRows
	}
	return derivedMatches[0], nil
}
func releaseRecordsMatch(a, b Release) bool {
	return a.PrimaryType == b.PrimaryType && releaseIdentityMatches(a, b)
}
func releaseIdentityMatches(a, b Release) bool {
	if normalizedReleaseTitle(a.Title) != normalizedReleaseTitle(b.Title) {
		return false
	}
	if a.DatePrecision == 0 || a.DatePrecision != b.DatePrecision {
		return false
	}
	length := map[int]int{1: 4, 2: 7, 3: 10}[a.DatePrecision]
	if length == 0 || len(a.FirstReleaseDate) != length || len(b.FirstReleaseDate) != length {
		return false
	}
	if a.DatePrecision != 3 {
		return a.FirstReleaseDate == b.FirstReleaseDate
	}
	left, leftErr := time.Parse("2006-01-02", a.FirstReleaseDate)
	right, rightErr := time.Parse("2006-01-02", b.FirstReleaseDate)
	if leftErr != nil || rightErr != nil {
		return false
	}
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= 3*24*time.Hour
}

func providerReleaseTypeDerived(release Release) bool {
	if strings.EqualFold(strings.TrimSpace(release.Source), "spotify") ||
		strings.EqualFold(strings.TrimSpace(release.Source), "itunes") {
		return true
	}
	return strings.TrimSpace(release.SpotifyID) != "" || strings.TrimSpace(release.ITunesID) != ""
}

type releaseDayDue struct {
	userID, releaseID          int64
	timezone, reminder         string
	artist, title, releaseDate string
	musicBrainzURL             string
	spotifyURL, itunesURL      sql.NullString
	artistCreditRole           string
}

func (s *Store) QueueDueReleaseDays(ctx context.Context, now time.Time) error {
	today := dayUTC(now)
	from := today.AddDate(0, 0, -1).Format("2006-01-02")
	to := today.AddDate(0, 0, 1).Format("2006-01-02")
	var candidates []releaseDayDue
	if err := func() error {
		rows, err := s.readerDB().QueryContext(ctx, `SELECT u.id,rg.id,u.timezone,u.reminder_time,a.name,rg.title,
			 rg.first_release_date,rg.musicbrainz_url,rg.spotify_url,rg.itunes_url,rg.artist_credit_role
			FROM users u JOIN release_groups rg ON 1=1
			JOIN artists a ON a.id=rg.artist_id
			WHERE `+followedReleasePredicate("u.id")+` AND rg.date_precision=3 AND rg.first_release_date BETWEEN ? AND ?`, from, to)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d releaseDayDue
			if err := rows.Scan(
				&d.userID, &d.releaseID, &d.timezone, &d.reminder, &d.artist, &d.title,
				&d.releaseDate, &d.musicBrainzURL, &d.spotifyURL, &d.itunesURL, &d.artistCreditRole,
			); err != nil {
				return err
			}
			candidates = append(candidates, d)
		}
		return rows.Err()
	}(); err != nil {
		return err
	}
	// A release can be visible because the member follows a credited artist
	// rather than the canonical release artist. Resolve those owner-scoped
	// associations in batches before building the event text so release-day
	// alerts describe the followed guest/featured artist instead of silently
	// presenting the canonical artist as the member's own release.
	type dueKey struct {
		userID, releaseID int64
	}
	associationByCandidate := make(map[dueKey][]FollowedArtistAssociation)
	releaseIDsByUser := make(map[int64][]int64)
	seenReleaseByUser := make(map[dueKey]bool)
	for _, candidate := range candidates {
		key := dueKey{userID: candidate.userID, releaseID: candidate.releaseID}
		if seenReleaseByUser[key] {
			continue
		}
		seenReleaseByUser[key] = true
		releaseIDsByUser[candidate.userID] = append(releaseIDsByUser[candidate.userID], candidate.releaseID)
	}
	for userID, releaseIDs := range releaseIDsByUser {
		associations, err := s.followedReleaseAssociationsBatch(ctx, userID, releaseIDs)
		if err != nil {
			return err
		}
		for _, releaseID := range releaseIDs {
			associationByCandidate[dueKey{userID: userID, releaseID: releaseID}] = associations[releaseID]
		}
	}
	for _, d := range candidates {
		location, err := time.LoadLocation(d.timezone)
		if err != nil {
			continue
		}
		localNow := now.In(location)
		reminder, validReminder := reminderMinutes(d.reminder)
		if !validReminder || d.releaseDate != localNow.Format("2006-01-02") || localNow.Hour()*60+localNow.Minute() < reminder {
			continue
		}
		displayArtist, creditRole := releaseDayAssociationDisplay(d, associationByCandidate[dueKey{userID: d.userID, releaseID: d.releaseID}])
		body := fmt.Sprintf("%s's %q is out today.", displayArtist, d.title)
		title := "Released today: " + d.title
		switch creditRole {
		case "guest":
			body = fmt.Sprintf("%s is credited on %q, released today.", displayArtist, d.title)
			title = "Guest appearance released today: " + d.title
		case "featured":
			body = fmt.Sprintf("%s appears on %q, released today.", displayArtist, d.title)
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

// releaseDayAssociationDisplay picks the strongest followed association for
// explanatory release-day text. The canonical release artist remains the
// primary display identity unless the member follows only a guest/featured
// credit, in which case the alert explicitly describes that appearance.
func releaseDayAssociationDisplay(d releaseDayDue, associations []FollowedArtistAssociation) (string, string) {
	role := normalizedCreditRole(d.artistCreditRole)
	displayArtist := d.artist
	if len(associations) == 0 {
		return displayArtist, role
	}
	// A primary association is the canonical release identity for this owner.
	// If there is no primary association, the member follows only a credited
	// appearance and the strongest non-primary role should be shown instead.
	var best *FollowedArtistAssociation
	for index := range associations {
		association := &associations[index]
		if normalizedCreditRole(association.Role) == "primary" {
			return displayArtist, "primary"
		}
		if best == nil || creditRolePriority(association.Role) < creditRolePriority(best.Role) {
			best = association
		}
	}
	if best == nil {
		return displayArtist, role
	}
	if normalizedCreditRole(best.Role) == "guest" || normalizedCreditRole(best.Role) == "featured" {
		displayArtist = associationDisplayName(best.Label)
		role = normalizedCreditRole(best.Role)
	}
	return displayArtist, role
}

func creditRolePriority(role string) int {
	switch normalizedCreditRole(role) {
	case "primary":
		return 0
	case "featured":
		return 1
	case "guest":
		return 2
	default:
		return 3
	}
}

func associationDisplayName(label string) string {
	label = strings.TrimSpace(label)
	for _, role := range []string{"primary", "featured", "guest"} {
		suffix := " (" + role + ")"
		if strings.HasSuffix(label, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(label, suffix))
		}
	}
	return label
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
	upcoming, err = func() ([]Release, error) {
		rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
			WHERE `+followedReleasePredicate("?")+` AND `+preferredProvider+` AND `+definitelyFuture+`
			ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?`,
			userID, today, today, today, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		return scanReleases(rows)
	}()
	if err != nil {
		return nil, nil, err
	}
	recent, err = func() ([]Release, error) {
		rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id
			WHERE `+followedReleasePredicate("?")+` AND `+preferredProvider+` AND NOT COALESCE(`+definitelyFuture+`,0)
			ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC,rg.id DESC LIMIT ?`,
			userID, today, today, today, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		return scanReleases(rows)
	}()
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
func releaseTypeEnabled(p NotificationPreferences, primary string, secondaryTypes ...[]string) bool {
	// Catalog providers normalize compilations as Album with a Compilation
	// secondary type. Account preferences do not expose a separate compilation
	// switch; leave those releases to the per-follow Compilations rule instead
	// of accidentally treating them as ordinary albums.
	for _, types := range secondaryTypes {
		for _, secondary := range types {
			if strings.EqualFold(strings.TrimSpace(secondary), "compilation") {
				return true
			}
		}
	}
	if strings.EqualFold(strings.TrimSpace(primary), "compilation") {
		return true
	}
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
	items, err := func() ([]Release, error) {
		rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+` FROM release_groups rg JOIN artists a ON a.id=rg.artist_id WHERE `+followedReleasePredicate("?")+` AND rg.id=?`, userID, releaseID)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		return scanReleases(rows)
	}()
	if err != nil {
		return d, err
	}
	if len(items) == 0 {
		return d, sql.ErrNoRows
	}
	d.Release = items[0]
	d.FollowedArtists, err = s.followedReleaseArtists(ctx, userID, releaseID)
	if err != nil {
		return d, err
	}
	// Attribute the shared truth decision before any cursor is open: this runs
	// on the bounded reader pool, and nesting a query inside an open projection
	// is how this package has exhausted that pool before.
	var decidedBy sql.NullInt64
	switch err := s.readerDB().QueryRowContext(ctx,
		`SELECT decided_by_user_id FROM release_truth_decisions WHERE release_group_id=?`,
		releaseID).Scan(&decidedBy); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return d, err
	case decidedBy.Valid && decidedBy.Int64 == userID:
		d.TruthDecidedByYou = true
	case decidedBy.Valid:
		d.TruthDecidedByAnotherMember = true
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
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}
