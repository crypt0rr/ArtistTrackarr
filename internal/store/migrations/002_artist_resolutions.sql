CREATE TABLE artist_resolutions (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('spotify')),
  provider_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  provider_url TEXT NOT NULL,
  image_url TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('pending','review')),
  candidate_json TEXT NOT NULL DEFAULT '[]',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, provider, provider_id)
);

CREATE INDEX artist_resolutions_due
  ON artist_resolutions(status, next_attempt_at);
