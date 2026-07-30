CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL COLLATE NOCASE UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('admin','member')),
  timezone TEXT NOT NULL DEFAULT 'UTC',
  reminder_time TEXT NOT NULL DEFAULT '09:00',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  token_hash BLOB PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS auth_tokens (
  token_hash BLOB PRIMARY KEY,
  kind TEXT NOT NULL CHECK(kind IN ('invite','reset')),
  email TEXT NOT NULL COLLATE NOCASE,
  user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  used_at TEXT,
  created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS login_attempts (
  key_hash BLOB PRIMARY KEY,
  failures INTEGER NOT NULL,
  first_at TEXT NOT NULL,
  blocked_until TEXT
);

CREATE TABLE IF NOT EXISTS artists (
  id INTEGER PRIMARY KEY,
  mbid TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  sort_name TEXT NOT NULL DEFAULT '',
  artist_type TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  disambiguation TEXT NOT NULL DEFAULT '',
  spotify_id TEXT,
  spotify_url TEXT,
  spotify_image_url TEXT,
  last_checked_at TEXT,
  next_check_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS follows (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  baseline_synced_at TEXT,
  PRIMARY KEY(user_id, artist_id)
);

CREATE TABLE IF NOT EXISTS release_groups (
  id INTEGER PRIMARY KEY,
  mbid TEXT NOT NULL UNIQUE,
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  primary_type TEXT NOT NULL,
  secondary_types TEXT NOT NULL DEFAULT '[]',
  first_release_date TEXT NOT NULL DEFAULT '',
  date_precision INTEGER NOT NULL DEFAULT 0,
  musicbrainz_url TEXT NOT NULL,
  spotify_url TEXT,
  first_observed_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS releases_artist ON release_groups(artist_id);
CREATE INDEX IF NOT EXISTS releases_date ON release_groups(first_release_date);

CREATE TABLE IF NOT EXISTS provider_observations (
  provider TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  payload_hash TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  PRIMARY KEY(provider, provider_id)
);

CREATE TABLE IF NOT EXISTS import_jobs (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS import_rows (
  id INTEGER PRIMARY KEY,
  job_id INTEGER NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
  source_value TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('added','already_followed','ambiguous','invalid')),
  artist_id INTEGER REFERENCES artists(id) ON DELETE SET NULL,
  reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS destinations (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  service TEXT NOT NULL,
  encrypted_url BLOB NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS notification_events (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL CHECK(event_type IN ('announcement','release_day')),
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, release_group_id, event_type)
);

CREATE TABLE IF NOT EXISTS deliveries (
  id INTEGER PRIMARY KEY,
  event_id INTEGER NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('pending','sent','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  UNIQUE(event_id, destination_id)
);
CREATE INDEX IF NOT EXISTS deliveries_due ON deliveries(status, next_attempt_at);
