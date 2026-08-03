CREATE TABLE IF NOT EXISTS artist_listenbrainz_stats (
  artist_id INTEGER PRIMARY KEY REFERENCES artists(id) ON DELETE CASCADE,
  total_listen_count INTEGER NOT NULL DEFAULT 0,
  total_user_count INTEGER NOT NULL DEFAULT 0,
  checked_at TEXT,
  next_check_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS artist_listenbrainz_due ON artist_listenbrainz_stats(next_check_at);

CREATE TABLE IF NOT EXISTS artist_genres (
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  genre TEXT NOT NULL,
  genre_key TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'musicbrainz',
  weight INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(artist_id, genre_key)
);
CREATE INDEX IF NOT EXISTS artist_genres_lookup ON artist_genres(genre_key, artist_id);
