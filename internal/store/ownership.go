package store

import "context"

// followedReleasePredicate returns an owner-scoped predicate for a release
// group aliased as rg. A release is visible when the member follows either
// the canonical artist or an artist represented by release_credits. EXISTS is
// intentional: a member may follow both artists, but each release must still
// appear once in owner-scoped projections.
func followedReleasePredicate(userExpression string) string {
	return `EXISTS (
		SELECT 1 FROM follows owner_follow
		WHERE owner_follow.user_id=` + userExpression + `
		AND (owner_follow.artist_id=rg.artist_id OR EXISTS (
			SELECT 1 FROM release_credits owner_credit
			WHERE owner_credit.release_group_id=rg.id
			AND owner_credit.artist_id=owner_follow.artist_id
		))
	)`
}

// followedReleaseAssociationRole is a correlated expression that picks one
// deterministic role for a followed artist/release association. Provider
// evidence can contain several rows for the same artist; primary beats
// featured, which beats guest, and the release-level role is used when no
// credit row exists for the canonical artist.
const followedReleaseAssociationRole = `COALESCE(
	(SELECT CASE lower(COALESCE(rc.role,''))
		WHEN 'primary' THEN 'primary'
		WHEN 'featured' THEN 'featured'
		WHEN 'guest' THEN 'guest'
		ELSE '' END
		FROM release_credits rc
		WHERE rc.release_group_id=rg.id AND rc.artist_id=f.artist_id
		ORDER BY CASE lower(COALESCE(rc.role,'')) WHEN 'primary' THEN 0 WHEN 'featured' THEN 1 WHEN 'guest' THEN 2 ELSE 3 END,rc.id
		LIMIT 1),
	CASE WHEN f.artist_id=rg.artist_id THEN CASE lower(COALESCE(rg.artist_credit_role,''))
		WHEN 'primary' THEN 'primary'
		WHEN 'featured' THEN 'featured'
		WHEN 'guest' THEN 'guest'
		ELSE '' END ELSE '' END
)`

const followedReleaseAssociationLabel = `a.name || CASE WHEN ` + followedReleaseAssociationRole + `<>'' THEN ' (' || ` + followedReleaseAssociationRole + ` || ')' ELSE '' END`

// followedReleaseCreditNames returns deterministic labels for all followed
// artist associations. It is used only for explanatory UI/notification text;
// the canonical release artist remains the primary display identity.
const followedReleaseCreditNames = `(SELECT GROUP_CONCAT(label, ', ')
	FROM (
		SELECT ` + followedReleaseAssociationLabel + ` AS label
		FROM follows f
		JOIN artists a ON a.id=f.artist_id
		WHERE f.user_id=? AND (f.artist_id=rg.artist_id OR EXISTS (
			SELECT 1 FROM release_credits rc
			WHERE rc.release_group_id=rg.id AND rc.artist_id=f.artist_id
		))
		ORDER BY lower(a.name),f.artist_id
	))`

func (s *Store) followedReleaseArtists(ctx context.Context, userID, releaseID int64) ([]string, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT `+followedReleaseAssociationLabel+`
		FROM follows f JOIN artists a ON a.id=f.artist_id
		JOIN release_groups rg ON rg.id=?
		WHERE f.user_id=? AND (f.artist_id=rg.artist_id OR EXISTS (
			SELECT 1 FROM release_credits rc
			WHERE rc.release_group_id=rg.id AND rc.artist_id=f.artist_id
		)) ORDER BY lower(a.name),f.artist_id`, releaseID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
