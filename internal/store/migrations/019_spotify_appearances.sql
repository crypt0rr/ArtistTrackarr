ALTER TABLE follows ADD COLUMN spotify_appears_on_baseline_synced_at TEXT;

ALTER TABLE release_groups ADD COLUMN artist_credit_role TEXT NOT NULL DEFAULT 'primary'
  CHECK(artist_credit_role IN ('primary','featured'));

ALTER TABLE release_provider_evidence ADD COLUMN artist_credit_role TEXT NOT NULL DEFAULT 'primary'
  CHECK(artist_credit_role IN ('primary','featured'));

CREATE INDEX IF NOT EXISTS follows_spotify_appears_baseline
  ON follows(spotify_appears_on_baseline_synced_at);
