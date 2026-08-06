ALTER TABLE notification_preferences
  ADD COLUMN release_digest_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notification_preferences
  ADD COLUMN release_digest_frequency TEXT NOT NULL DEFAULT 'weekly';

INSERT OR IGNORE INTO notification_preferences(user_id, updated_at)
SELECT id, created_at FROM users;

CREATE TABLE IF NOT EXISTS release_digest_runs (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  frequency TEXT NOT NULL CHECK(frequency IN ('daily','weekly')),
  period_start TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  release_count INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL CHECK(status IN ('pending','sent','failed')),
  created_at TEXT NOT NULL,
  UNIQUE(user_id, frequency, period_start)
);

CREATE TABLE IF NOT EXISTS release_digest_deliveries (
  id INTEGER PRIMARY KEY,
  run_id INTEGER NOT NULL REFERENCES release_digest_runs(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('pending','sent','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  UNIQUE(run_id, destination_id)
);
CREATE INDEX IF NOT EXISTS release_digest_deliveries_due
  ON release_digest_deliveries(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS release_digest_runs_user
  ON release_digest_runs(user_id, created_at DESC);
