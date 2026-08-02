ALTER TABLE artists ADD COLUMN spotify_unchanged_checks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN spotify_last_change_at TEXT;
