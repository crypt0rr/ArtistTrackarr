ALTER TABLE artists ADD COLUMN spotify_next_check_at TEXT;

CREATE INDEX IF NOT EXISTS artists_next_check ON artists(next_check_at);
CREATE INDEX IF NOT EXISTS artists_spotify_next_check ON artists(spotify_next_check_at);
