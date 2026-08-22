package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	// ErrInvalidReleaseTruthProvider means the requested source is unsupported.
	ErrInvalidReleaseTruthProvider = errors.New("invalid release truth provider")
	// ErrReleaseTruthProviderUnavailable means the provider is valid but has no
	// persisted observation for this release.
	ErrReleaseTruthProviderUnavailable = errors.New("release truth provider is unavailable")

	// ErrReleaseTruthDecidedByAnotherMember reports an attempt to overwrite or
	// clear a decision another household member recorded. The decision is one
	// shared row per release, so without this guard any member who follows the
	// release - which a single guest credit is enough for - could silently
	// revert another member's review and replace its stated reason.
	ErrReleaseTruthDecidedByAnotherMember = errors.New("release truth decision belongs to another household member")
)

// SetReleaseTruthDecision records a household-scoped, reversible source choice
// after verifying that the caller follows the release's artist.
// The decision is deliberately separate from release_groups and provider
// observations so it cannot rewrite canonical data. An explicit confirmation
// may release or create this member's notification after normal admission
// rules are applied.
func (s *Store) SetReleaseTruthDecision(ctx context.Context, userID, releaseID int64, provider, reason string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "spotify" && provider != "itunes" && provider != "musicbrainz" {
		return ErrInvalidReleaseTruthProvider
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 240 {
		return errors.New("release truth reason is too long")
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var providerID sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT CASE ?
		WHEN 'spotify' THEN rg.spotify_id
		WHEN 'itunes' THEN rg.itunes_id
		WHEN 'musicbrainz' THEN rg.mbid
		END
		FROM release_groups rg
		WHERE rg.id=? AND `+followedReleasePredicate("?")+``, provider, releaseID, userID).Scan(&providerID)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		if !providerID.Valid || strings.TrimSpace(providerID.String) == "" {
			return ErrReleaseTruthProviderUnavailable
		}
		// The decision is one shared row per release. Only the member who
		// recorded it, or a household administrator, may change it; otherwise a
		// second member silently reverts the first member's review and replaces
		// its stated reason, which then renders on every member's release page.
		if err := releaseTruthDecisionWritableTx(ctx, tx, userID, releaseID); err != nil {
			return err
		}
		now := nowText()
		_, err = tx.ExecContext(ctx, `INSERT INTO release_truth_decisions
		(release_group_id,state,selected_provider,selected_provider_id,reason,decided_by_user_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(release_group_id) DO UPDATE SET state=excluded.state,
		selected_provider=excluded.selected_provider,selected_provider_id=excluded.selected_provider_id,
		reason=excluded.reason,decided_by_user_id=excluded.decided_by_user_id,updated_at=excluded.updated_at`,
			releaseID, "confirmed", provider, providerID.String, reason, userID, now, now)
		if err != nil {
			return err
		}
		// Confirming a provider is an explicit review decision, so release any
		// notification that was held for this member even if another observation
		// remains present for audit purposes.
		nowTime := time.Now().UTC()
		if err := drainNotificationHoldsTx(ctx, tx, userID, releaseID, nowTime); err != nil {
			return err
		}
		return ensureApprovedReleaseNotificationTx(ctx, tx, userID, releaseID, nowTime, true)
	})
}

// ClearReleaseTruthDecision removes a source choice for a followed release.
// Removing it restores the explainable automatic confidence state.
func (s *Store) ClearReleaseTruthDecision(ctx context.Context, userID, releaseID int64) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM release_groups rg
		WHERE rg.id=? AND `+followedReleasePredicate("?")+` LIMIT 1`, releaseID, userID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return err
		}
		if err := releaseTruthDecisionWritableTx(ctx, tx, userID, releaseID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM release_truth_decisions WHERE release_group_id=?`, releaseID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func releaseTruthState(explicitState, source string, sourceCount int, sources []string, issueCount int) string {
	if explicitState == "confirmed" {
		return "confirmed"
	}
	if issueCount > 0 {
		return "needs_review"
	}
	hasMusicBrainz := source == "musicbrainz"
	for _, provider := range sources {
		if provider == "musicbrainz" {
			hasMusicBrainz = true
			break
		}
	}
	if sourceCount >= 2 {
		if hasMusicBrainz {
			return "verified"
		}
		return "fallback_confirmed"
	}
	if hasMusicBrainz {
		return "canonical"
	}
	return "observed"
}

func parseTruthUpdatedAt(value sql.NullString) (*time.Time, error) {
	return parseStoredNullableTime(value, "release truth updated_at")
}

// releaseTruthDecisionWritableTx reports whether userID may change the shared
// truth decision for releaseID. An absent decision is writable by any follower;
// an existing one belongs to the member who recorded it, and a household
// administrator can always override.
func releaseTruthDecisionWritableTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64) error {
	var decidedBy sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT decided_by_user_id FROM release_truth_decisions
		WHERE release_group_id=?`, releaseID).Scan(&decidedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !decidedBy.Valid || decidedBy.Int64 == userID {
		return nil
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(role,'') FROM users WHERE id=?`, userID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReleaseTruthDecidedByAnotherMember
		}
		return err
	}
	if strings.EqualFold(strings.TrimSpace(role), "admin") {
		return nil
	}
	return ErrReleaseTruthDecidedByAnotherMember
}
