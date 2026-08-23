package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// calendarPreferredProvider suppresses a MusicBrainz row that is an unmerged
// near-duplicate of a provider row for the same release.
//
// It used to test whether the ARTIST had any provider release at all, which
// removed every genuinely MusicBrainz-only release from the calendar, the ICS
// export, the token feed and the digest as soon as that artist gained a single
// Spotify or iTunes release - while the announcement path, which applies no such
// filter, still notified the member about it.
//
// The duplicate window mirrors releaseIdentityMatches: same artist, same date
// precision, and within three days at day precision or an exact match otherwise.
// calendarPreferredProvider suppresses a MusicBrainz row that is an unmerged
// near-duplicate of a provider row for the same release.
//
// The title comparison is deliberately narrower than releaseIdentityMatches.
// That function normalises titles in Go - stripping provider type suffixes and
// edition markers - and SQL cannot call it. Reimplementing that normalisation
// here would create a second definition of "same title" that drifts from the
// first, which is the exact failure mode that has produced several defects in
// this project. Comparing case-folded, trimmed titles instead means this clause
// suppresses strictly less than the merge comparator would: a near-duplicate
// whose titles differ only by an edition suffix stays visible.
//
// That asymmetry is the safe direction. Showing one redundant row costs a
// member almost nothing; hiding a real release costs them the release. The
// previous version compared artist and date but no title at all, so any
// provider release within three days hid an unrelated MusicBrainz-only release
// from the calendar, the ICS feed, the token feed, the digest and the dashboard
// at once - while the announcement path still told the member it existed.
const calendarPreferredProvider = `(rg.source IN ('spotify','itunes','both') OR NOT EXISTS (
	SELECT 1 FROM release_groups duplicate
	WHERE duplicate.artist_id=rg.artist_id
	  AND duplicate.id<>rg.id
	  AND duplicate.source IN ('spotify','itunes','both')
	  AND duplicate.date_precision=rg.date_precision
	  AND lower(trim(duplicate.title))=lower(trim(rg.title))
	  AND (
		(rg.date_precision=3
		  AND date(duplicate.first_release_date) BETWEEN date(rg.first_release_date,'-3 day')
		                                            AND date(rg.first_release_date,'+3 day'))
		OR (rg.date_precision<>3 AND duplicate.first_release_date=rg.first_release_date)
	  )
))`

// CalendarReleases returns precise, dated releases for one followed artist
// watchlist within an inclusive ISO date range. Partial dates are intentionally
// excluded from ICS output because assigning them to a day would be misleading.
func (s *Store) CalendarReleases(ctx context.Context, userID int64, from, to string, limit int) ([]CalendarRelease, error) {
	return s.CalendarReleasesPage(ctx, userID, from, to, limit, 0)
}

// CalendarReleasesPage returns one deterministic page of precise, dated
// releases for a followed artist watchlist. The offset form is used by the
// authenticated and tokenized ICS exporters so large calendars are never
// silently truncated.
func (s *Store) CalendarReleasesPage(ctx context.Context, userID int64, from, to string, limit, offset int) ([]CalendarRelease, error) {
	if limit < 1 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+`,
		EXISTS(SELECT 1 FROM notification_holds nh
			WHERE nh.user_id=? AND nh.release_group_id=rg.id AND nh.status='held')
		FROM release_groups rg
		JOIN artists a ON a.id=rg.artist_id
		WHERE `+followedReleasePredicate("?")+` AND `+calendarPreferredProvider+`
		AND rg.date_precision=3 AND length(rg.first_release_date)=10
		AND rg.first_release_date BETWEEN ? AND ?
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ? OFFSET ?`,
		userID, userID, strings.TrimSpace(from), strings.TrimSpace(to), limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []CalendarRelease
	for rows.Next() {
		var item CalendarRelease
		var held int
		item.Release, err = scanReleaseWithExtra(rows, &held)
		if err != nil {
			return nil, err
		}
		item.CalendarDate = item.FirstReleaseDate
		item.Held = held != 0
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	// Materialize and close the release projection before resolving followed
	// associations. The reader pool is finite; querying associations while the
	// cursor is open can otherwise deadlock when all readers are occupied.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return result, nil
	}
	releaseIDs := make([]int64, len(result))
	for i := range result {
		releaseIDs[i] = result[i].ID
	}
	followedAssociations, err := s.followedReleaseAssociationsBatch(ctx, userID, releaseIDs)
	if err != nil {
		return nil, err
	}
	for i := range result {
		associations := followedAssociations[result[i].ID]
		result[i].FollowedAssociations = associations
		result[i].FollowedArtists = make([]string, 0, len(associations))
		for _, association := range associations {
			result[i].FollowedArtists = append(result[i].FollowedArtists, association.Label)
		}
	}
	return result, nil
}

type digestUser struct {
	ID        int64
	Timezone  string
	Reminder  string
	Frequency string
	Albums    bool
	EPs       bool
	Singles   bool
}

// Digest assembly bounds. Rows are scanned in pages and filtered by the
// member's preferences and per-follow rules as they are read, so an eligible
// release is not lost behind a page full of ineligible ones.
const (
	digestScanPageSize = 200
	digestMaxScanRows  = 1000
	digestMaxItems     = 50
)

// QueueDueReleaseDigests creates at most one aggregate digest run per user
// and local period. It deliberately reuses the normal destination encryption
// and retry path, while keeping aggregate content separate from per-release
// notification event uniqueness.
//
// A member is included when the account-level digest is on, or when any follow
// is set to "Digest only". The account digest defaults to off, so without the
// second condition a digest-only follow produced a notification event with no
// delivery rows and no digest run: the alert went nowhere, and because
// notification_events is unique per (user, release, event type) it could never
// be re-queued by enabling the digest or reverting the follow later.
func (s *Store) QueueDueReleaseDigests(ctx context.Context, now time.Time) (int, error) {
	var users []digestUser
	if err := func() error {
		rows, err := s.readerDB().QueryContext(ctx, `SELECT u.id,u.timezone,u.reminder_time,
			p.release_digest_frequency,p.albums,p.eps,p.singles
			FROM users u JOIN notification_preferences p ON p.user_id=u.id
			WHERE p.release_digest_enabled=1
			   OR EXISTS (SELECT 1 FROM follow_notification_rules r
				WHERE r.user_id=u.id AND lower(trim(COALESCE(r.delivery_mode,'')))='digest')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var user digestUser
			var frequency string
			var albums, eps, singles int
			if err := rows.Scan(&user.ID, &user.Timezone, &user.Reminder, &frequency, &albums, &eps, &singles); err != nil {
				return err
			}
			user.Frequency = normalizeDigestFrequency(frequency)
			user.Albums, user.EPs, user.Singles = albums != 0, eps != 0, singles != 0
			users = append(users, user)
		}
		return rows.Err()
	}(); err != nil {
		return 0, err
	}

	queued := 0
	for _, user := range users {
		location, err := time.LoadLocation(user.Timezone)
		if err != nil {
			continue
		}
		localNow := now.In(location)
		if reminder, ok := reminderMinutes(user.Reminder); !ok ||
			localNow.Hour()*60+localNow.Minute() < reminder {
			continue
		}

		// Keep the de-duplication period anchored to local midnight. The prior
		// implementation used the current instant as the period start and only
		// stored its date, which made a timezone change on the same UTC day look
		// like a brand-new digest period. The explicit bounds below let us find a
		// run created in the same logical local period even when the profile's
		// timezone has since changed.
		periodStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
		periodEnd := periodStart.AddDate(0, 0, 1)
		// The upper bound is converted to an inclusive date below. Include
		// today and tomorrow in a daily digest.
		windowEnd := localNow.AddDate(0, 0, 2)
		if user.Frequency == "weekly" {
			daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
			periodStart = periodStart.AddDate(0, 0, -daysSinceMonday)
			periodEnd = periodStart.AddDate(0, 0, 7)
			// Deduplicate by the local week, but show the next seven days from
			// the time the digest is generated so a late-week setup is useful.
			windowEnd = localNow.AddDate(0, 0, 8)
		}
		periodKey := periodStart.Format("2006-01-02")
		// Everything the de-duplication check needs is known here, so ask before
		// doing the expensive part. The loop below runs FollowNotificationRules
		// and up to five CalendarReleasesPage scans - the heaviest read in the
		// store - builds the digest body, and only then opened a write
		// transaction whose first statement discovered the run already existed.
		// On the sixty-second tick plus every UI-triggered wake, that rebuilt and
		// threw away each member's whole digest for the rest of their local
		// period, contending for the four-connection reader pool and the single
		// writer that user-facing writes also need.
		settled, err := s.digestPeriodSettled(ctx, user, periodKey, periodStart, periodEnd)
		if err != nil {
			return queued, err
		}
		if settled {
			continue
		}
		from := localNow.Format("2006-01-02")
		to := windowEnd.AddDate(0, 0, -1).Format("2006-01-02")
		rules, err := s.FollowNotificationRules(ctx, user.ID, nil)
		if err != nil {
			return queued, err
		}
		// Scan in pages and filter as we go. Fetching a single fixed page and
		// only then applying the member's type preferences and per-follow digest
		// rules meant a busy window could fill that page with ineligible rows and
		// produce a short or empty digest while eligible releases sat just past
		// the cap. Both bounds stay modest so digest generation remains cheap.
		var items []CalendarRelease
		for offset := 0; offset < digestMaxScanRows; offset += digestScanPageSize {
			page, pageErr := s.CalendarReleasesPage(ctx, user.ID, from, to, digestScanPageSize, offset)
			if pageErr != nil {
				return queued, pageErr
			}
			items = append(items, page...)
			if len(page) < digestScanPageSize {
				break
			}
		}
		var releases []CalendarRelease
		for _, item := range items {
			if len(releases) >= digestMaxItems {
				break
			}
			if !releaseTypeEnabled(NotificationPreferences{Albums: user.Albums, EPs: user.EPs, Singles: user.Singles}, item.PrimaryType, item.SecondaryTypes) {
				continue
			}
			// Calendar visibility can come from a credited followed artist rather
			// than the canonical release artist. Evaluate every owner-scoped
			// association and admit the release when at least one follow is
			// configured for digest delivery.
			associations := item.FollowedAssociations
			if len(associations) == 0 {
				associations = []FollowedArtistAssociation{{ArtistID: item.ArtistID, Role: item.ArtistCreditRole}}
			}
			eligible := false
			for _, association := range associations {
				rule, ok := rules[association.ArtistID]
				if !ok {
					rule = defaultFollowNotificationRule(user.ID, association.ArtistID, now.UTC())
				}
				if rule.AllowsRelease(item.PrimaryType, item.SecondaryTypes, association.Role, "", now) && rule.belongsInDigest(now) {
					eligible = true
					break
				}
			}
			if !eligible {
				continue
			}
			releases = append(releases, item)
		}
		if len(releases) == 0 {
			continue
		}
		body := buildDigestBody(releases, user.Frequency)
		title := fmt.Sprintf("Upcoming releases · %s", localNow.Format("2006-01-02"))
		queuedRun := false
		err = s.withWriteTx(ctx, func(tx *sql.Tx) error {
			var runID int64
			var runStatus string
			lookupErr := tx.QueryRowContext(ctx, digestRunLookup,
				digestRunLookupArgs(user, periodKey, periodStart, periodEnd)...).Scan(&runID, &runStatus)
			insertedRun := errors.Is(lookupErr, sql.ErrNoRows)
			if lookupErr != nil && !insertedRun {
				return lookupErr
			}
			if insertedRun {
				result, err := tx.ExecContext(ctx, `INSERT INTO release_digest_runs
					(user_id,frequency,period_start,timezone,title,body,release_count,status,created_at)
					VALUES(?,?,?,?,?,?,?, 'pending',?)`, user.ID, user.Frequency, periodKey, user.Timezone, title, body, len(releases), timeText(now))
				if err != nil {
					return err
				}
				runID, err = result.LastInsertId()
				if err != nil {
					return err
				}
			} else if runStatus != "pending" {
				// A completed or failed run already has a durable outcome for this
				// logical period. Never attach a newly-added destination to it.
				return nil
			} else {
				var deliveries int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_digest_deliveries WHERE run_id=?`, runID).Scan(&deliveries); err != nil {
					return err
				}
				if deliveries > 0 {
					return nil
				}
			}
			deliveryResult, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO release_digest_deliveries
				(run_id,destination_id,status,next_attempt_at)
				SELECT ?,d.id,`+destinationQueueStatus("d")+`,? FROM destinations d
				LEFT JOIN destination_health dh ON dh.destination_id=d.id
				WHERE d.user_id=? AND d.enabled=1
				AND d.created_at <= (SELECT created_at FROM release_digest_runs WHERE id=?)`, runID, timeText(now), user.ID, runID)
			if err != nil {
				return err
			}
			deliveryCount, err := deliveryResult.RowsAffected()
			if err != nil {
				return err
			}
			if deliveryCount == 0 {
				// Keep an orphan run pending for auditability. Destinations added after
				// this run was created are intentionally not backfilled into historical
				// work; only a newly created future digest can admit them.
				return nil
			}
			queuedRun = true
			return nil
		})
		if err != nil {
			return queued, err
		}
		if queuedRun {
			queued++
		}
	}
	return queued, nil
}

// periodLookback covers how far a digest period boundary can move when a
// profile changes timezone, which is bounded by the span of real UTC offsets
// (UTC-12 to UTC+14) and never by the length of the period. Expanding it by a
// whole period instead meant the previous period's run - which necessarily
// carries the old timezone - fell inside the window and suppressed the new
// period: a full week of digests for a weekly subscriber who changed timezone.
const periodLookback = 26 * time.Hour

// digestRunLookup finds the run covering a member's logical period. A profile
// can change timezone between scheduler runs, so the wider time window applies
// only when the stored run was created under another timezone; same-timezone
// periods continue to use the exact key and cannot suppress a legitimate
// next-day or next-week digest.
//
// One definition because two call sites need it: the cheap pre-check that
// decides whether the digest scan is worth doing at all, and the authoritative
// re-check inside the write transaction. Hand-synchronised copies of a
// timezone-tolerant period predicate are exactly the sort of thing that drifts.
const digestRunLookup = `SELECT id,status FROM release_digest_runs
	WHERE user_id=? AND frequency=? AND (period_start=? OR
		(COALESCE(timezone,'UTC')<>? AND created_at>=? AND created_at<?))
	ORDER BY created_at DESC,id DESC LIMIT 1`

func digestRunLookupArgs(user digestUser, periodKey string, periodStart, periodEnd time.Time) []any {
	return []any{user.ID, user.Frequency, periodKey, user.Timezone,
		timeText(periodStart.Add(-periodLookback).UTC()), timeText(periodEnd.Add(periodLookback).UTC())}
}

// digestPeriodSettled reports whether this member's digest for this period is
// already decided, so the caller can skip building it. It mirrors the two
// branches inside the write transaction that return without queueing: a run
// that is no longer pending has a durable outcome, and a pending run that
// already has deliveries is fully queued.
//
// It deliberately does not cover the third branch - a pending run with no
// deliveries - because that one still attaches destinations, and only when the
// current scan is non-empty. Skipping on it would queue digests the current
// code does not.
//
// This is an optimisation, not the authority: the transaction repeats the
// lookup. A state that changes between the two is re-evaluated on the next
// tick, which is a minute away.
func (s *Store) digestPeriodSettled(ctx context.Context, user digestUser, periodKey string, periodStart, periodEnd time.Time) (bool, error) {
	var runID int64
	var runStatus string
	err := s.readerDB().QueryRowContext(ctx, digestRunLookup,
		digestRunLookupArgs(user, periodKey, periodStart, periodEnd)...).Scan(&runID, &runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if runStatus != "pending" {
		return true, nil
	}
	var deliveries int
	if err := s.readerDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM release_digest_deliveries WHERE run_id=?`, runID).Scan(&deliveries); err != nil {
		return false, err
	}
	return deliveries > 0, nil
}

// reminderMinutes accepts the persisted HH:MM form and harmless legacy
// values such as H:MM, normalizing both before local-time comparisons. New
// profile writes remain strict; this keeps older rows from being skipped just
// because their hour was not zero-padded.
func reminderMinutes(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// normalizeReminderTime returns the canonical zero-padded form used for new
// profile values. Older rows are still accepted by reminderMinutes, but
// normalizing writes keeps comparisons, exports, and future queries
// deterministic.
func normalizeReminderTime(value string) (string, bool) {
	minutes, ok := reminderMinutes(value)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60), true
}

func buildDigestBody(releases []CalendarRelease, frequency string) string {
	const maxDigestBodyBytes = 3500
	var builder strings.Builder
	fmt.Fprintf(&builder, "Your %s ArtistTrackarr release digest:\n", frequency)
	for _, item := range releases {
		var entry strings.Builder
		status := calendarConfidenceLabel(item.Release, item.Held)
		artistName := item.ArtistName
		association := ""
		if len(item.FollowedArtists) > 0 {
			association = "; followed association(s): " + strings.Join(item.FollowedArtists, ", ")
		}
		if item.ArtistCreditRole == "featured" && item.GuestCreditCount > 0 {
			fmt.Fprintf(&entry, "- %s — %s — %s (%s; Guest appearance; %s%s)", item.CalendarDate, artistName, item.Title, item.PrimaryType, status, association)
		} else if item.ArtistCreditRole == "featured" {
			fmt.Fprintf(&entry, "- %s — %s — %s (%s; Featured appearance; %s%s)", item.CalendarDate, artistName, item.Title, item.PrimaryType, status, association)
		} else {
			fmt.Fprintf(&entry, "- %s — %s — %s (%s; %s%s)", item.CalendarDate, artistName, item.Title, item.PrimaryType, status, association)
		}
		if link := releaseExternalURL(item.Release); link != "" {
			entry.WriteString("\n  ")
			entry.WriteString(link)
		}
		entryText := entry.String()
		if builder.Len()+1+len(entryText) > maxDigestBodyBytes {
			marker := "\n… additional releases omitted"
			if builder.Len()+len(marker) > maxDigestBodyBytes {
				return strings.TrimSpace(truncateUTF8(builder.String(), maxDigestBodyBytes))
			}
			builder.WriteString(marker)
			break
		}
		builder.WriteByte('\n')
		builder.WriteString(entryText)
	}
	return strings.TrimSpace(builder.String())
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	data := []byte(value[:limit])
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}

func calendarConfidenceLabel(release Release, held bool) string {
	if held {
		return "held for review"
	}
	if release.TruthIssueCount > 0 {
		return "review required"
	}
	if release.Confidence == "confirmed" || release.SourceCount > 1 {
		return "confirmed"
	}
	return "single source"
}
