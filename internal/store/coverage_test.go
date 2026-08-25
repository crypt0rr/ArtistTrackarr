package store

import (
	"context"
	"testing"
	"time"
)

// TestDeferredObservationsDoNotEraseProviderEvidence pins which statuses may
// overwrite stored provider evidence. The protective branch was gated on
// `standby || skipped`, but the scheduler emits standby, deferred,
// not_configured and cooldown - and emits "skipped" nowhere at all. The three
// unlisted ones took the destructive branch and wrote last_error=” over a real
// failure on the artist's very next sync tick, so the Trust Center lost the
// message explaining why a provider had stopped working.
func TestDeferredObservationsDoNotEraseProviderEvidence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "evidence-artist", Name: "Evidence Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	for _, status := range []string{"deferred", "not_configured", "cooldown", "standby"} {
		t.Run(status, func(t *testing.T) {
			failedAt := now.Add(-time.Hour)
			if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
				ArtistID: artist.ID, Provider: "spotify", Status: "failed",
				LastAttemptAt: &failedAt, LastFailureAt: &failedAt,
				LastError: "spotify credentials rejected", UpdatedAt: failedAt,
			}); err != nil {
				t.Fatal(err)
			}
			// The next tick reports the provider was not contacted at all.
			if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
				ArtistID: artist.ID, Provider: "spotify", Status: status,
				LastAttemptAt: &now, LastError: "", UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			var stored string
			if err := s.DB.QueryRowContext(ctx,
				`SELECT COALESCE(last_error,'') FROM artist_provider_status WHERE artist_id=? AND provider='spotify'`,
				artist.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != "spotify credentials rejected" {
				t.Fatalf("a %q observation erased the stored failure: last_error=%q", status, stored)
			}
		})
	}
}

// TestRealProviderOutcomesStillOverwrite keeps the carve-out honest: a status
// that IS the result of a call must replace the previous evidence, or a fixed
// provider would keep reporting a stale error forever.
func TestRealProviderOutcomesStillOverwrite(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "overwrite-artist", Name: "Overwrite Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	failedAt := now.Add(-time.Hour)
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "spotify", Status: "failed",
		LastAttemptAt: &failedAt, LastFailureAt: &failedAt,
		LastError: "spotify credentials rejected", UpdatedAt: failedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "spotify", Status: "healthy",
		LastAttemptAt: &now, LastSuccessAt: &now, LastError: "", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(last_error,'') FROM artist_provider_status WHERE artist_id=? AND provider='spotify'`,
		artist.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Fatalf("a successful call left the old error in place: last_error=%q", stored)
	}
}

// TestProviderNotContactedCoversEveryEmittedStatus keeps the predicate and the
// scheduler from drifting apart again: every status the jobs layer emits must
// be classified deliberately, not by omission.
func TestProviderNotContactedCoversEveryEmittedStatus(t *testing.T) {
	contacted := map[string]bool{
		"healthy": true, "failed": true, "degraded": true, "not_found": true, "ambiguous": true,
		"standby": false, "deferred": false, "not_configured": false, "cooldown": false,
	}
	for status, isContact := range contacted {
		if providerNotContacted(status) == isContact {
			t.Errorf("status %q classified wrongly: providerNotContacted=%v", status, providerNotContacted(status))
		}
	}
}

// TestFirstProviderObservationNeverStoresTheSentinel is #268. The jobs layer
// passes ReleaseCount = -1 to mean "no batch this time, keep whatever was
// observed before". That sentinel is decoded only by the ON CONFLICT clauses, so
// on the FIRST row for an (artist, provider) pair it was written into the column
// verbatim - and every later upsert then took the preserve branch and kept -1
// forever, until that provider first returned a healthy batch. With no Spotify
// credentials configured that never happens, so the Trust Center rendered
// "-1 releases returned" permanently.
func TestFirstProviderObservationNeverStoresTheSentinel(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "sentinel-artist", Name: "Sentinel Artist"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	stored := func(provider string) int {
		t.Helper()
		var count int
		if err := s.DB.QueryRowContext(ctx,
			`SELECT release_count FROM artist_provider_status WHERE artist_id=? AND provider=?`,
			artist.ID, provider).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// A first observation that contacted nothing - the not_configured path a
	// deployment without Spotify credentials takes on every single sync.
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "spotify", Status: "not_configured",
		ReleaseCount: -1, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if got := stored("spotify"); got < 0 {
		t.Fatalf("first observation stored the sentinel verbatim: release_count=%d", got)
	}

	// A first observation on the contacted path must not store it either.
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "failed",
		ReleaseCount: -1, LastError: "upstream unavailable", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if got := stored("musicbrainz"); got < 0 {
		t.Fatalf("first contacted observation stored the sentinel: release_count=%d", got)
	}

	// The sentinel must still PRESERVE a real count once one exists.
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "healthy",
		ReleaseCount: 17, UpdatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stored("musicbrainz"); got != 17 {
		t.Fatalf("a healthy observation did not store its count: release_count=%d", got)
	}
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "failed",
		ReleaseCount: -1, LastError: "upstream unavailable", UpdatedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stored("musicbrainz"); got != 17 {
		t.Fatalf("the sentinel failed to preserve the previous count: release_count=%d, want 17", got)
	}
	// And a genuine zero must still overwrite, or a provider that legitimately
	// returns nothing would report a stale count forever.
	if err := s.RecordArtistProviderStatus(ctx, ArtistProviderStatus{
		ArtistID: artist.ID, Provider: "musicbrainz", Status: "healthy",
		ReleaseCount: 0, UpdatedAt: now.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stored("musicbrainz"); got != 0 {
		t.Fatalf("a genuine zero did not overwrite: release_count=%d", got)
	}
}
