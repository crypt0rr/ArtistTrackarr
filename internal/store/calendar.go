package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const calendarPreferredProvider = `((a.spotify_id IS NULL AND NOT EXISTS (
	SELECT 1 FROM release_groups external_release
	WHERE external_release.artist_id=rg.artist_id
	AND external_release.source IN ('spotify','itunes','both')
)) OR rg.source IN ('spotify','itunes','both') OR NOT EXISTS (
	SELECT 1 FROM release_groups newer
	WHERE newer.artist_id=rg.artist_id
	AND newer.source IN ('spotify','itunes','both')
))`

// CalendarReleases returns precise, dated releases for one followed artist
// watchlist within an inclusive ISO date range. Partial dates are intentionally
// excluded from ICS output because assigning them to a day would be misleading.
func (s *Store) CalendarReleases(ctx context.Context, userID int64, from, to string, limit int) ([]CalendarRelease, error) {
	if limit < 1 {
		limit = 200
	}
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+releaseSelectColumns+`,
		EXISTS(SELECT 1 FROM notification_holds nh
			WHERE nh.user_id=? AND nh.release_group_id=rg.id AND nh.status='held')
		FROM release_groups rg
		JOIN artists a ON a.id=rg.artist_id
		JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? AND `+calendarPreferredProvider+`
		AND rg.date_precision=3 AND length(rg.first_release_date)=10
		AND rg.first_release_date BETWEEN ? AND ?
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?`,
		userID, userID, strings.TrimSpace(from), strings.TrimSpace(to), limit)
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
	return result, rows.Err()
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

// QueueDueReleaseDigests creates at most one aggregate digest run per user
// and local period. It deliberately reuses the normal destination encryption
// and retry path, while keeping aggregate content separate from per-release
// notification event uniqueness.
func (s *Store) QueueDueReleaseDigests(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT u.id,u.timezone,u.reminder_time,
		p.release_digest_frequency,p.albums,p.eps,p.singles
		FROM users u JOIN notification_preferences p ON p.user_id=u.id
		WHERE p.release_digest_enabled=1`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var users []digestUser
	for rows.Next() {
		var user digestUser
		var frequency string
		var albums, eps, singles int
		if err := rows.Scan(&user.ID, &user.Timezone, &user.Reminder, &frequency, &albums, &eps, &singles); err != nil {
			return 0, err
		}
		user.Frequency = normalizeDigestFrequency(frequency)
		user.Albums, user.EPs, user.Singles = albums != 0, eps != 0, singles != 0
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	queued := 0
	for _, user := range users {
		location, err := time.LoadLocation(user.Timezone)
		if err != nil {
			continue
		}
		localNow := now.In(location)
		if reminder, parseErr := time.Parse("15:04", user.Reminder); parseErr != nil ||
			localNow.Hour()*60+localNow.Minute() < reminder.Hour()*60+reminder.Minute() {
			continue
		}

		periodStart := localNow
		// The upper bound is converted to an inclusive date below. Include
		// today and tomorrow in a daily digest.
		windowEnd := localNow.AddDate(0, 0, 2)
		if user.Frequency == "weekly" {
			daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
			periodStart = localNow.AddDate(0, 0, -daysSinceMonday)
			// Deduplicate by the local week, but show the next seven days from
			// the time the digest is generated so a late-week setup is useful.
			windowEnd = localNow.AddDate(0, 0, 8)
		}
		periodKey := periodStart.Format("2006-01-02")
		from := localNow.Format("2006-01-02")
		to := windowEnd.AddDate(0, 0, -1).Format("2006-01-02")
		items, err := s.CalendarReleases(ctx, user.ID, from, to, 50)
		if err != nil {
			return queued, err
		}
		rules, err := s.FollowNotificationRules(ctx, user.ID, nil)
		if err != nil {
			return queued, err
		}
		var releases []CalendarRelease
		for _, item := range items {
			rule, ok := rules[item.ArtistID]
			if !ok {
				rule = defaultFollowNotificationRule(user.ID, item.ArtistID, now.UTC())
			}
			if !releaseTypeEnabled(NotificationPreferences{Albums: user.Albums, EPs: user.EPs, Singles: user.Singles}, item.PrimaryType) ||
				!rule.AllowsContent(item.PrimaryType, item.ArtistCreditRole, "", now) || !rule.belongsInDigest(now) {
				continue
			}
			releases = append(releases, item)
		}
		if len(releases) == 0 {
			continue
		}
		body := buildDigestBody(releases, user.Frequency)
		title := fmt.Sprintf("Upcoming releases · %s", localNow.Format("2006-01-02"))
		tx, err := s.beginWriteTx(ctx)
		if err != nil {
			return queued, err
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO release_digest_runs
			(user_id,frequency,period_start,title,body,release_count,status,created_at)
			VALUES(?,?,?,?,?,?, 'pending',?)`, user.ID, user.Frequency, periodKey, title, body, len(releases), timeText(now))
		if err != nil {
			_ = tx.Rollback()
			return queued, err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return queued, err
		}
		if inserted == 0 {
			_ = tx.Rollback()
			continue
		}
		runID, err := result.LastInsertId()
		if err != nil {
			_ = tx.Rollback()
			return queued, err
		}
		deliveryResult, err := tx.ExecContext(ctx, `INSERT INTO release_digest_deliveries
			(run_id,destination_id,status,next_attempt_at)
			SELECT ?,id,'pending',? FROM destinations WHERE user_id=? AND enabled=1`, runID, timeText(now), user.ID)
		if err != nil {
			_ = tx.Rollback()
			return queued, err
		}
		deliveryCount, err := deliveryResult.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return queued, err
		}
		if deliveryCount == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE release_digest_runs SET status='sent' WHERE id=?`, runID); err != nil {
				_ = tx.Rollback()
				return queued, err
			}
		}
		if err := tx.Commit(); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

func buildDigestBody(releases []CalendarRelease, frequency string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Your %s ArtistTrackarr release digest:\n", frequency)
	for _, item := range releases {
		status := calendarConfidenceLabel(item.Release, item.Held)
		if item.ArtistCreditRole == "featured" && item.GuestCreditCount > 0 {
			fmt.Fprintf(&builder, "- %s — %s — %s (%s; Guest appearance; %s)", item.CalendarDate, item.ArtistName, item.Title, item.PrimaryType, status)
		} else if item.ArtistCreditRole == "featured" {
			fmt.Fprintf(&builder, "- %s — %s — %s (%s; Featured appearance; %s)", item.CalendarDate, item.ArtistName, item.Title, item.PrimaryType, status)
		} else {
			fmt.Fprintf(&builder, "- %s — %s — %s (%s; %s)", item.CalendarDate, item.ArtistName, item.Title, item.PrimaryType, status)
		}
		if link := releaseExternalURL(item.Release); link != "" {
			builder.WriteString("\n  ")
			builder.WriteString(link)
		}
		builder.WriteByte('\n')
	}
	return strings.TrimSpace(builder.String())
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
