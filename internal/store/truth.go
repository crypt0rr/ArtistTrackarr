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
