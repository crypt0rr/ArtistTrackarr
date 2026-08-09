-- Owner-scoped delivery preferences for individual follows. These rules are
-- intentionally separate from shared artist/release data and account-wide
-- notification preferences.
CREATE TABLE IF NOT EXISTS follow_notification_rules (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  delivery_mode TEXT NOT NULL DEFAULT 'inherit'
    CHECK(delivery_mode IN ('inherit','immediate','digest','off')),
  include_primary INTEGER NOT NULL DEFAULT 1,
  include_featured INTEGER NOT NULL DEFAULT 1,
  albums INTEGER NOT NULL DEFAULT 1,
  eps INTEGER NOT NULL DEFAULT 1,
  singles INTEGER NOT NULL DEFAULT 1,
  compilations INTEGER NOT NULL DEFAULT 1,
  announcements INTEGER NOT NULL DEFAULT 1,
  release_day INTEGER NOT NULL DEFAULT 1,
  paused_until TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, artist_id)
);

CREATE INDEX IF NOT EXISTS follow_notification_rules_artist
  ON follow_notification_rules(artist_id);
CREATE INDEX IF NOT EXISTS follow_notification_rules_pause
  ON follow_notification_rules(user_id, paused_until);

INSERT OR IGNORE INTO follow_notification_rules(
  user_id,artist_id,delivery_mode,include_primary,include_featured,
  albums,eps,singles,compilations,announcements,release_day,updated_at)
SELECT user_id,artist_id,'inherit',1,1,1,1,1,1,1,1,created_at
FROM follows;
