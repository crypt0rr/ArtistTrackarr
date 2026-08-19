-- Bind an iTunes artist identity to one canonical MusicBrainz artist. A
-- provider ID may not be claimed by two canonical artists; this prevents a
-- same-name lookup from silently sharing release data across homonyms.
CREATE TABLE IF NOT EXISTS artist_provider_identities (
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('itunes')),
  provider_id TEXT NOT NULL,
  provider_url TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(artist_id, provider),
  UNIQUE(provider, provider_id)
);

CREATE INDEX IF NOT EXISTS artist_provider_identities_provider_id
  ON artist_provider_identities(provider, provider_id);
