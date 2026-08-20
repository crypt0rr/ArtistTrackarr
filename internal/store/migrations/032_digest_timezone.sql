-- Remember the timezone used to create a digest so changing a profile's
-- timezone cannot create a second run for the same local period.
ALTER TABLE release_digest_runs
  ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';

CREATE INDEX IF NOT EXISTS release_digest_runs_period_timezone
  ON release_digest_runs(user_id,frequency,timezone,created_at);
