package store

import (
	"context"
	"database/sql"
	"time"
)

// RetentionReport is a safe, read-only dry run for the administrator. The
// history counters are informational; only the two operational candidate
// counts can be affected by CleanupRetention.
type RetentionReport struct {
	CheckedAt                 time.Time
	Policy                    RetentionPolicy
	NotificationEvents        int64
	Deliveries                int64
	DeliveryAttempts          int64
	ApplicationLogs           int64
	OldestNotificationEvent   *time.Time
	OldestDelivery            *time.Time
	OldestDeliveryAttempt     *time.Time
	OldestApplicationLog      *time.Time
	OldestHistory             *time.Time
	HistoryAgeDays            int
	HistoryReviewDue          bool
	PrunableApplicationLogs   int64
	PrunableTransientSessions int64
	PrunableAuthTokens        int64
	PrunableLoginAttempts     int64
	PrunableManualSyncs       int64
	PrunableImportJobs        int64
}

// RetentionCleanupStats contains only rows removed from transient state. It
// deliberately has no notification or delivery fields so callers cannot
// accidentally imply that user-facing history was purged.
type RetentionCleanupStats struct {
	ApplicationLogs int64
	Sessions        int64
	AuthTokens      int64
	LoginAttempts   int64
	ManualSyncs     int64
	ImportJobs      int64
	// WALCheckpointed indicates that SQLite truncated the write-ahead log
	// after cleanup. A successful checkpoint does not compact freelist pages;
	// operators must schedule an explicit VACUUM during a maintenance window
	// if shrinking the database file is required.
	WALCheckpointed    bool
	WALCheckpointBusy  bool
	WALCheckpointError bool
}

func (s *Store) RetentionReport(ctx context.Context, now time.Time) (RetentionReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	policy := s.retention()
	report := RetentionReport{CheckedAt: now.UTC(), Policy: policy}

	queries := []struct {
		query  string
		count  *int64
		oldest **time.Time
		name   string
	}{
		{`SELECT COUNT(*), MIN(created_at) FROM notification_events`, &report.NotificationEvents, &report.OldestNotificationEvent, "notification event"},
		{`SELECT COUNT(*), MIN(COALESCE(sent_at,next_attempt_at)) FROM deliveries`, &report.Deliveries, &report.OldestDelivery, "delivery"},
		{`SELECT COUNT(*), MIN(started_at) FROM delivery_attempts`, &report.DeliveryAttempts, &report.OldestDeliveryAttempt, "delivery attempt"},
		{`SELECT COUNT(*), MIN(created_at) FROM application_logs`, &report.ApplicationLogs, &report.OldestApplicationLog, "application log"},
	}
	for _, item := range queries {
		var oldest sql.NullString
		if err := s.readerDB().QueryRowContext(ctx, item.query).Scan(item.count, &oldest); err != nil {
			return RetentionReport{}, err
		}
		parsed, err := parseStoredNullableTime(oldest, item.name+" oldest timestamp")
		if err != nil {
			return RetentionReport{}, err
		}
		*item.oldest = parsed
	}

	logCutoff := timeText(report.CheckedAt.Add(-time.Duration(policy.ApplicationLogsDays) * 24 * time.Hour))
	transientCutoff := timeText(report.CheckedAt.Add(-time.Duration(policy.TransientStateDays) * 24 * time.Hour))
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM application_logs WHERE created_at < ?`, logCutoff).Scan(&report.PrunableApplicationLogs); err != nil {
		return RetentionReport{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE expires_at < ?`, timeText(report.CheckedAt)).Scan(&report.PrunableTransientSessions); err != nil {
		return RetentionReport{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, timeText(report.CheckedAt)).Scan(&report.PrunableAuthTokens); err != nil {
		return RetentionReport{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM login_attempts WHERE first_at < ?`, timeText(report.CheckedAt.Add(-24*time.Hour))).Scan(&report.PrunableLoginAttempts); err != nil {
		return RetentionReport{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM manual_sync_requests WHERE status IN ('completed','failed') AND finished_at IS NOT NULL AND finished_at < ?`, transientCutoff).Scan(&report.PrunableManualSyncs); err != nil {
		return RetentionReport{}, err
	}
	if err := s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM import_jobs WHERE created_at < ?
		AND NOT `+resumableImportJob, transientCutoff).Scan(&report.PrunableImportJobs); err != nil {
		return RetentionReport{}, err
	}
	report.OldestHistory = oldestTime(report.OldestNotificationEvent, report.OldestDelivery, report.OldestDeliveryAttempt)
	if report.OldestHistory != nil {
		age := report.CheckedAt.Sub(report.OldestHistory.UTC())
		if age > 0 {
			report.HistoryAgeDays = int(age / (24 * time.Hour))
		}
		report.HistoryReviewDue = report.HistoryAgeDays >= policy.HistoryReviewDays
	}
	return report, nil
}

func oldestTime(values ...*time.Time) *time.Time {
	var oldest *time.Time
	for _, value := range values {
		if value == nil {
			continue
		}
		candidate := value.UTC()
		if oldest == nil || candidate.Before(oldest.UTC()) {
			copy := candidate
			oldest = &copy
		}
	}
	return oldest
}

// CleanupRetention performs the same bounded cleanup as scheduled
// maintenance, but only when an administrator explicitly requests it. It
// never deletes notification events, deliveries, inbox state, blocked rows,
// or delivery-attempt audit records.
func (s *Store) CleanupRetention(ctx context.Context, now time.Time) (RetentionCleanupStats, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	policy := s.retention()
	stats, err := withWriteTxResult(s, ctx, func(tx *sql.Tx) (RetentionCleanupStats, error) {
		var resultStats RetentionCleanupStats
		statements := []struct {
			query string
			args  []any
			out   *int64
		}{
			{`DELETE FROM application_logs WHERE created_at < ?`, []any{timeText(now.Add(-time.Duration(policy.ApplicationLogsDays) * 24 * time.Hour))}, &resultStats.ApplicationLogs},
			{`DELETE FROM sessions WHERE expires_at < ?`, []any{timeText(now)}, &resultStats.Sessions},
			{`DELETE FROM auth_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, []any{timeText(now)}, &resultStats.AuthTokens},
			{`DELETE FROM login_attempts WHERE first_at < ?`, []any{timeText(now.Add(-24 * time.Hour))}, &resultStats.LoginAttempts},
			{`DELETE FROM manual_sync_requests WHERE status IN ('completed','failed') AND finished_at IS NOT NULL AND finished_at < ?`, []any{timeText(now.Add(-time.Duration(policy.TransientStateDays) * 24 * time.Hour))}, &resultStats.ManualSyncs},
			// Keep interrupted and failed jobs that still carry a payload: that
			// payload is the whole reason migration 034 retains it, and deleting
			// it silently removes the Resume import action for that job.
			{`DELETE FROM import_jobs WHERE created_at < ?
				AND NOT ` + resumableImportJob,
				[]any{timeText(now.Add(-time.Duration(policy.TransientStateDays) * 24 * time.Hour))}, &resultStats.ImportJobs},
		}
		for _, statement := range statements {
			result, err := tx.ExecContext(ctx, statement.query, statement.args...)
			if err != nil {
				return RetentionCleanupStats{}, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return RetentionCleanupStats{}, err
			}
			*statement.out = count
		}
		return resultStats, nil
	})
	if err != nil {
		return RetentionCleanupStats{}, err
	}
	// Cleanup has already committed at this point. Do not turn a successful
	// deletion into a misleading failure if a concurrent reader prevents WAL
	// truncation; report the outcome explicitly instead.
	var busy, logPages, checkpointedPages int64
	checkpointErr := s.DB.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointedPages)
	stats.WALCheckpointError = checkpointErr != nil
	// Treat an inability to checkpoint like a busy reader for operator-facing
	// messaging. The committed row cleanup remains successful either way.
	stats.WALCheckpointBusy = checkpointErr != nil || busy != 0
	stats.WALCheckpointed = checkpointErr == nil && busy == 0
	return stats, nil
}
