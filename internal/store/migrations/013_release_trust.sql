-- Per-artist provider state complements provider_health's process-wide state.
-- Release provenance remains derived from provider_observations so this table
-- never changes notification or release identity semantics.
CREATE TABLE IF NOT EXISTS artist_provider_status (
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('spotify','itunes','musicbrainz')),
  status TEXT NOT NULL DEFAULT 'pending',
  last_attempt_at TEXT,
  last_success_at TEXT,
  last_failure_at TEXT,
  next_check_at TEXT,
  release_count INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(artist_id, provider)
);
CREATE INDEX IF NOT EXISTS artist_provider_status_next
  ON artist_provider_status(next_check_at, provider);
CREATE INDEX IF NOT EXISTS artist_provider_status_artist
  ON artist_provider_status(artist_id, provider);
