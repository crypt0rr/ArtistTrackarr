ALTER TABLE follows ADD COLUMN spotify_baseline_synced_at TEXT;

ALTER TABLE release_groups ADD COLUMN spotify_id TEXT;
ALTER TABLE release_groups ADD COLUMN spotify_image_url TEXT NOT NULL DEFAULT '';
ALTER TABLE release_groups ADD COLUMN source TEXT NOT NULL DEFAULT 'musicbrainz'
  CHECK(source IN ('musicbrainz','spotify','both'));

CREATE UNIQUE INDEX release_groups_spotify_id
  ON release_groups(spotify_id)
  WHERE spotify_id IS NOT NULL;
