-- Imported MusicBrainz identifiers are checked asynchronously before release
-- polling starts. Existing artists were created through an exact MusicBrainz
-- result, so they are trusted and backfilled as verified.
CREATE TABLE IF NOT EXISTS artist_identity_status (
  artist_id INTEGER PRIMARY KEY REFERENCES artists(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'verified'
    CHECK(status IN ('pending','verified','unresolvable')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_check_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO artist_identity_status
  (artist_id,status,attempts,next_check_at,last_error,updated_at)
SELECT id,'verified',0,NULL,'',updated_at
FROM artists;

CREATE INDEX IF NOT EXISTS artist_identity_status_due
  ON artist_identity_status(status,next_check_at);
