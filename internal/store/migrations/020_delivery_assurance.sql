-- Durable delivery health and per-attempt audit records.  These tables are
-- deliberately additive: existing delivery state remains the source of
-- truth for queueing and notification deduplication.
CREATE TABLE IF NOT EXISTS destination_health (
  destination_id INTEGER PRIMARY KEY REFERENCES destinations(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'healthy' CHECK(status IN ('healthy','degraded','paused')),
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  last_success_at TEXT,
  last_failure_at TEXT,
  next_retry_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS destination_health_retry
  ON destination_health(status, next_retry_at);

CREATE TABLE IF NOT EXISTS delivery_attempts (
  id INTEGER PRIMARY KEY,
  -- IDs are retained as audit references even after the queue row is removed
  -- by destination/user cleanup; the snapshot fields below remain usable.
  delivery_id INTEGER,
  digest_delivery_id INTEGER,
  destination_id INTEGER REFERENCES destinations(id) ON DELETE SET NULL,
  destination_name TEXT NOT NULL,
  service TEXT NOT NULL,
  attempt_number INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('started','sent','failed')),
  started_at TEXT NOT NULL,
  finished_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  CHECK ((delivery_id IS NOT NULL) != (digest_delivery_id IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS delivery_attempts_destination
  ON delivery_attempts(destination_id, started_at DESC);
CREATE INDEX IF NOT EXISTS delivery_attempts_delivery
  ON delivery_attempts(delivery_id, started_at DESC);
CREATE INDEX IF NOT EXISTS delivery_attempts_digest
  ON delivery_attempts(digest_delivery_id, started_at DESC);
