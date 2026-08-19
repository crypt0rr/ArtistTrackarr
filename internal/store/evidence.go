package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// evaluateReleaseEvidenceTx compares the latest normalized snapshot from each
// provider. It deliberately runs inside the release transaction so an issue
// can never describe a provider observation that was not committed.
func evaluateReleaseEvidenceTx(ctx context.Context, tx *sql.Tx, releaseID int64, observed time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT provider,provider_id,title,primary_type,
		first_release_date,date_precision,provider_url,artist_credit_role,observed_at
		FROM release_provider_evidence WHERE release_group_id=?
		ORDER BY observed_at DESC,provider,provider_id`, releaseID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	latest := make(map[string]ReleaseEvidence)
	for rows.Next() {
		var evidence ReleaseEvidence
		var observedAt string
		if err := rows.Scan(&evidence.Provider, &evidence.ProviderID, &evidence.Title,
			&evidence.PrimaryType, &evidence.FirstReleaseDate, &evidence.DatePrecision,
			&evidence.ProviderURL, &evidence.ArtistCreditRole, &observedAt); err != nil {
			return err
		}
		evidence.ArtistCreditRole = normalizedArtistCreditRole(evidence.ArtistCreditRole)
		evidence.ObservedAt, err = parseStoredTime(observedAt, "release evidence observed_at")
		if err != nil {
			return err
		}
		if _, exists := latest[evidence.Provider]; !exists {
			latest[evidence.Provider] = evidence
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	evidence := make([]ReleaseEvidence, 0, len(latest))
	for _, item := range latest {
		evidence = append(evidence, item)
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Provider < evidence[j].Provider })
	issues := detectEvidenceIssues(evidence)
	if err := upsertEvidenceIssuesTx(ctx, tx, releaseID, issues, observed); err != nil {
		return err
	}
	return nil
}

type detectedEvidenceIssue struct {
	issueType   string
	severity    string
	summary     string
	evidence    []ReleaseEvidence
	fingerprint string
}

func detectEvidenceIssues(evidence []ReleaseEvidence) []detectedEvidenceIssue {
	if len(evidence) < 2 {
		return nil
	}
	issues := make([]detectedEvidenceIssue, 0, 4)
	if issue := evidenceDateIssue(evidence); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := evidenceTitleIssue(evidence); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := evidenceTypeIssue(evidence); issue != nil {
		issues = append(issues, *issue)
	}
	hasCanonical := false
	for _, item := range evidence {
		if item.Provider == "musicbrainz" {
			hasCanonical = true
			break
		}
	}
	if !hasCanonical {
		issues = append(issues, detectedEvidenceIssue{
			issueType: "missing_canonical", severity: "info",
			summary:  "Multiple provider observations have not yet been confirmed by MusicBrainz.",
			evidence: evidence,
		})
	}
	for i := range issues {
		issues[i].fingerprint = evidenceIssueFingerprint(issues[i].issueType, issues[i].evidence)
	}
	return issues
}

func evidenceDateIssue(evidence []ReleaseEvidence) *detectedEvidenceIssue {
	for i := range evidence {
		if !validEvidenceDate(evidence[i].FirstReleaseDate, evidence[i].DatePrecision) {
			continue
		}
		for j := i + 1; j < len(evidence); j++ {
			if validEvidenceDate(evidence[j].FirstReleaseDate, evidence[j].DatePrecision) &&
				!evidenceDatesCompatible(evidence[i], evidence[j]) {
				return &detectedEvidenceIssue{
					issueType: "date_conflict", severity: "warning",
					summary: evidenceSummary("Providers disagree on the release date", evidence, func(item ReleaseEvidence) string {
						return item.FirstReleaseDate
					}), evidence: evidence,
				}
			}
		}
	}
	return nil
}

func evidenceTitleIssue(evidence []ReleaseEvidence) *detectedEvidenceIssue {
	var first string
	for _, item := range evidence {
		title := normalizedReleaseTitle(item.Title)
		if title == "" {
			continue
		}
		if first == "" {
			first = title
			continue
		}
		if first != title {
			return &detectedEvidenceIssue{
				issueType: "title_conflict", severity: "warning",
				summary: evidenceSummary("Providers disagree on the release title", evidence, func(item ReleaseEvidence) string {
					return item.Title
				}), evidence: evidence,
			}
		}
	}
	return nil
}

func evidenceTypeIssue(evidence []ReleaseEvidence) *detectedEvidenceIssue {
	var first string
	for _, item := range evidence {
		typeName := strings.ToLower(strings.TrimSpace(item.PrimaryType))
		if typeName == "" {
			continue
		}
		if first == "" {
			first = typeName
			continue
		}
		if first != typeName {
			return &detectedEvidenceIssue{
				issueType: "type_conflict", severity: "warning",
				summary: evidenceSummary("Providers disagree on the release type", evidence, func(item ReleaseEvidence) string {
					return item.PrimaryType
				}), evidence: evidence,
			}
		}
	}
	return nil
}

func evidenceSummary(prefix string, evidence []ReleaseEvidence, value func(ReleaseEvidence) string) string {
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		parts = append(parts, fmt.Sprintf("%s: %s", providerLabel(item.Provider), strings.TrimSpace(value(item))))
	}
	return prefix + ": " + strings.Join(parts, "; ")
}

func providerLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "musicbrainz":
		return "MusicBrainz"
	case "spotify":
		return "Spotify"
	case "itunes":
		return "iTunes"
	default:
		return provider
	}
}

func evidenceIssueFingerprint(issueType string, evidence []ReleaseEvidence) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(issueType))
	for _, item := range evidence {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(item.Provider))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(item.ProviderID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(item.Title))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(item.PrimaryType))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(item.FirstReleaseDate))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(fmt.Sprint(item.DatePrecision)))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validEvidenceDate(value string, precision int) bool {
	value = strings.TrimSpace(value)
	length := map[int]int{1: 4, 2: 7, 3: 10}[precision]
	if length == 0 {
		length = len(value)
	}
	if len(value) != length || (length != 4 && length != 7 && length != 10) {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	if length == 4 {
		_, err := time.Parse("2006", value)
		return err == nil
	}
	if length == 7 {
		_, err := time.Parse("2006-01", value)
		return err == nil
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func evidenceDatesCompatible(a, b ReleaseEvidence) bool {
	precisionA, precisionB := evidencePrecision(a), evidencePrecision(b)
	precision := precisionA
	if precisionB < precision {
		precision = precisionB
	}
	length := map[int]int{1: 4, 2: 7, 3: 10}[precision]
	return length > 0 && a.FirstReleaseDate[:minInt(length, len(a.FirstReleaseDate))] ==
		b.FirstReleaseDate[:minInt(length, len(b.FirstReleaseDate))]
}

func evidencePrecision(item ReleaseEvidence) int {
	if item.DatePrecision >= 1 && item.DatePrecision <= 3 {
		return item.DatePrecision
	}
	switch len(strings.TrimSpace(item.FirstReleaseDate)) {
	case 4:
		return 1
	case 7:
		return 2
	case 10:
		return 3
	default:
		return 0
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func upsertEvidenceIssuesTx(ctx context.Context, tx *sql.Tx, releaseID int64, issues []detectedEvidenceIssue, observed time.Time) error {
	current := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		current[issue.issueType+"\x00"+issue.fingerprint] = struct{}{}
		payload, err := json.Marshal(issue.evidence)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO release_evidence_issues
			(release_group_id,issue_type,severity,fingerprint,summary,evidence_json,status,first_seen_at,last_seen_at)
			VALUES(?,?,?,?,?,?,'open',?,?)
			ON CONFLICT(release_group_id,issue_type,fingerprint) DO UPDATE SET
			 severity=excluded.severity,summary=excluded.summary,evidence_json=excluded.evidence_json,
			 status='open',last_seen_at=excluded.last_seen_at,resolved_at=NULL`,
			releaseID, issue.issueType, issue.severity, issue.fingerprint, issue.summary, string(payload),
			timeText(observed), timeText(observed))
		if err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,issue_type,fingerprint FROM release_evidence_issues
		WHERE release_group_id=? AND status='open'`, releaseID)
	if err != nil {
		return err
	}
	type existingIssue struct {
		id  int64
		key string
	}
	var stale []existingIssue
	for rows.Next() {
		var item existingIssue
		var issueType, fingerprint string
		if err := rows.Scan(&item.id, &issueType, &fingerprint); err != nil {
			_ = rows.Close()
			return err
		}
		item.key = issueType + "\x00" + fingerprint
		if _, ok := current[item.key]; !ok {
			stale = append(stale, item)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range stale {
		if _, err := tx.ExecContext(ctx, `UPDATE release_evidence_issues SET status='resolved',resolved_at=? WHERE id=?`, timeText(observed), item.id); err != nil {
			return err
		}
	}
	return nil
}

func evidenceIssueFilters(status, state, issueType, severity string, now time.Time) (string, []any, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "open"
	}
	if status != "open" && status != "resolved" {
		return "", nil, errors.New("invalid evidence issue status")
	}
	where := ` AND i.status=?`
	args := []any{status}
	switch state = strings.ToLower(strings.TrimSpace(state)); state {
	case "", "unread":
		where += ` AND (r.state IS NULL OR r.state='unread' OR (r.state='snoozed' AND (r.snoozed_until IS NULL OR r.snoozed_until<=?)))`
		args = append(args, timeText(now.UTC()))
	case "all":
	case "snoozed":
		where += ` AND r.state='snoozed' AND r.snoozed_until>?`
		args = append(args, timeText(now.UTC()))
	case "confirmed", "dismissed":
		where += ` AND r.state=?`
		args = append(args, state)
	default:
		return "", nil, errors.New("invalid evidence issue state")
	}
	if issueType = strings.ToLower(strings.TrimSpace(issueType)); issueType != "" {
		switch issueType {
		case "date_conflict", "title_conflict", "type_conflict", "missing_canonical":
			where += ` AND i.issue_type=?`
			args = append(args, issueType)
		default:
			return "", nil, errors.New("invalid evidence issue type")
		}
	}
	if severity = strings.ToLower(strings.TrimSpace(severity)); severity != "" {
		switch severity {
		case "info", "warning", "critical":
			where += ` AND i.severity=?`
			args = append(args, severity)
		default:
			return "", nil, errors.New("invalid evidence issue severity")
		}
	}
	return where, args, nil
}

var evidenceIssueSelect = `SELECT i.id,i.release_group_id,rg.artist_id,a.name,rg.title,
	i.issue_type,i.severity,i.fingerprint,i.summary,i.evidence_json,i.status,
	COALESCE(r.state,'unread'),r.snoozed_until,i.first_seen_at,i.last_seen_at,i.resolved_at
	FROM release_evidence_issues i
	JOIN release_groups rg ON rg.id=i.release_group_id
	JOIN artists a ON a.id=rg.artist_id
	LEFT JOIN release_evidence_reviews r ON r.issue_id=i.id AND r.user_id=?
	WHERE ` + followedReleasePredicate("?")

func (s *Store) EvidenceIssues(ctx context.Context, userID int64, status, state, issueType, severity string, limit, offset int, now time.Time) ([]EvidenceIssue, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, filterArgs, err := evidenceIssueFilters(status, state, issueType, severity, now)
	if err != nil {
		return nil, err
	}
	args := []any{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.readerDB().QueryContext(ctx, evidenceIssueSelect+where+` ORDER BY CASE i.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,i.last_seen_at DESC,i.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EvidenceIssue
	for rows.Next() {
		item, err := scanEvidenceIssue(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) EvidenceIssueCount(ctx context.Context, userID int64, status, state, issueType, severity string, now time.Time) (int, error) {
	where, filterArgs, err := evidenceIssueFilters(status, state, issueType, severity, now)
	if err != nil {
		return 0, err
	}
	args := []any{userID, userID}
	args = append(args, filterArgs...)
	var count int
	err = s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM release_evidence_issues i
		JOIN release_groups rg ON rg.id=i.release_group_id
		LEFT JOIN release_evidence_reviews r ON r.issue_id=i.id AND r.user_id=?
		WHERE `+followedReleasePredicate("?")+where, args...).Scan(&count)
	return count, err
}

func (s *Store) EvidenceIssueUnreadCount(ctx context.Context, userID int64, now time.Time) (int, error) {
	return s.EvidenceIssueCount(ctx, userID, "open", "unread", "", "", now)
}

func (s *Store) EvidenceIssuesForRelease(ctx context.Context, userID, releaseID int64, now time.Time) ([]EvidenceIssue, error) {
	where, filterArgs, err := evidenceIssueFilters("open", "all", "", "", now)
	if err != nil {
		return nil, err
	}
	args := []any{userID, userID, releaseID}
	args = append(args, filterArgs...)
	rows, err := s.readerDB().QueryContext(ctx, evidenceIssueSelect+` AND i.release_group_id=?`+where+` ORDER BY i.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EvidenceIssue
	for rows.Next() {
		item, err := scanEvidenceIssue(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func scanEvidenceIssue(row interface{ Scan(...any) error }) (EvidenceIssue, error) {
	var item EvidenceIssue
	var payload, snoozed, firstSeen, lastSeen, resolved sql.NullString
	if err := row.Scan(&item.ID, &item.ReleaseGroupID, &item.ArtistID, &item.ArtistName, &item.ReleaseTitle,
		&item.IssueType, &item.Severity, &item.Fingerprint, &item.Summary, &payload, &item.Status,
		&item.ReviewState, &snoozed, &firstSeen, &lastSeen, &resolved); err != nil {
		return item, err
	}
	if payload.Valid && payload.String != "" {
		if err := json.Unmarshal([]byte(payload.String), &item.Evidence); err != nil {
			return item, err
		}
	}
	var parseErr error
	item.SnoozedUntil, parseErr = parseStoredNullableTime(snoozed, "evidence issue snoozed_until")
	if parseErr != nil {
		return item, parseErr
	}
	item.FirstSeenAt, parseErr = parseStoredTime(firstSeen.String, "evidence issue first_seen_at")
	if parseErr != nil {
		return item, parseErr
	}
	item.LastSeenAt, parseErr = parseStoredTime(lastSeen.String, "evidence issue last_seen_at")
	if parseErr != nil {
		return item, parseErr
	}
	item.ResolvedAt, parseErr = parseStoredNullableTime(resolved, "evidence issue resolved_at")
	if parseErr != nil {
		return item, parseErr
	}
	if item.ReviewState == "snoozed" && item.SnoozedUntil != nil && !item.SnoozedUntil.After(time.Now().UTC()) {
		item.ReviewState = "unread"
	}
	return item, nil
}

func (s *Store) SetEvidenceIssueState(ctx context.Context, userID, issueID int64, state string, snoozedUntil *time.Time) error {
	state = strings.ToLower(strings.TrimSpace(state))
	if state != "confirmed" && state != "snoozed" && state != "dismissed" && state != "unread" {
		return errors.New("invalid evidence issue state")
	}
	if state == "snoozed" {
		if snoozedUntil == nil || !snoozedUntil.After(time.Now().UTC()) {
			return errors.New("snooze time must be in the future")
		}
	} else {
		snoozedUntil = nil
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var releaseID int64
		err := tx.QueryRowContext(ctx, `SELECT i.release_group_id FROM release_evidence_issues i
		JOIN release_groups rg ON rg.id=i.release_group_id
		WHERE i.id=? AND `+followedReleasePredicate("?")+` LIMIT 1`, issueID, userID).Scan(&releaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		if state == "unread" {
			_, err := tx.ExecContext(ctx, `DELETE FROM release_evidence_reviews WHERE user_id=? AND issue_id=?`, userID, issueID)
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO release_evidence_reviews(user_id,issue_id,state,snoozed_until,updated_at)
		VALUES(?,?,?,?,?) ON CONFLICT(user_id,issue_id) DO UPDATE SET state=excluded.state,
		snoozed_until=excluded.snoozed_until,updated_at=excluded.updated_at`, userID, issueID, state,
			nullableTime(snoozedUntil), nowText())
		if err != nil {
			return err
		}
		if state == "confirmed" {
			now := time.Now().UTC()
			if err := drainResolvedNotificationHoldsForUserTx(ctx, tx, userID, releaseID, now); err != nil {
				return err
			}
			if err := ensureApprovedReleaseNotificationTx(ctx, tx, userID, releaseID, now, false); err != nil {
				return err
			}
		}
		return nil
	})
}
