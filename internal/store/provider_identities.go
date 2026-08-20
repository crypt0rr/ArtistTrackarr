package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ArtistProviderIdentity binds a provider-specific artist ID to the
// canonical MusicBrainz artist that was reviewed or resolved for it.
type ArtistProviderIdentity struct {
	ArtistID    int64
	Provider    string
	ProviderID  string
	ProviderURL string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ArtistProviderIdentity returns the owner-scoped provider mapping, if one
// exists. Missing mappings are expected for artists that have not yet had an
// unambiguous provider lookup.
func (s *Store) ArtistProviderIdentity(ctx context.Context, artistID int64, provider string) (ArtistProviderIdentity, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "itunes" {
		return ArtistProviderIdentity{}, false, errors.New("unsupported artist provider identity")
	}
	var identity ArtistProviderIdentity
	var created, updated string
	err := s.readerDB().QueryRowContext(ctx, `SELECT artist_id,provider,provider_id,provider_url,created_at,updated_at
		FROM artist_provider_identities WHERE artist_id=? AND provider=?`, artistID, provider).Scan(
		&identity.ArtistID, &identity.Provider, &identity.ProviderID, &identity.ProviderURL, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtistProviderIdentity{}, false, nil
	}
	if err != nil {
		return ArtistProviderIdentity{}, false, err
	}
	identity.CreatedAt, err = parseStoredTime(created, "artist provider identity created_at")
	if err != nil {
		return ArtistProviderIdentity{}, false, err
	}
	identity.UpdatedAt, err = parseStoredTime(updated, "artist provider identity updated_at")
	if err != nil {
		return ArtistProviderIdentity{}, false, err
	}
	return identity, true, nil
}

// SaveArtistProviderIdentity persists an unambiguous provider mapping. The
// UNIQUE(provider, provider_id) constraint intentionally rejects attempts to
// assign one iTunes identity to two canonical artists.
func (s *Store) SaveArtistProviderIdentity(ctx context.Context, artistID int64, provider, providerID, providerURL string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	providerID, providerURL = strings.TrimSpace(providerID), strings.TrimSpace(providerURL)
	if provider != "itunes" {
		return errors.New("unsupported artist provider identity")
	}
	if artistID <= 0 || !validProviderIdentityID(providerID) {
		return errors.New("invalid iTunes artist identity")
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return saveArtistProviderIdentityTx(ctx, tx, artistID, provider, providerID, providerURL)
	})
}

func saveArtistProviderIdentityTx(ctx context.Context, tx *sql.Tx, artistID int64, provider, providerID, providerURL string) error {
	now := nowText()
	_, err := tx.ExecContext(ctx, `INSERT INTO artist_provider_identities
		(artist_id,provider,provider_id,provider_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(artist_id,provider) DO UPDATE SET
		provider_id=excluded.provider_id,
		provider_url=CASE WHEN excluded.provider_url<>'' THEN excluded.provider_url ELSE artist_provider_identities.provider_url END,
		updated_at=excluded.updated_at`,
		artistID, provider, providerID, providerURL, now, now)
	return err
}

func validProviderIdentityID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
