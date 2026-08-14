-- Bounded, redacted operational history.  These rows contain counters and
-- timestamps only; they never include provider payloads or notification data.
CREATE TABLE operational_snapshots (
  id INTEGER PRIMARY KEY,
  captured_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('healthy','degraded','unavailable')),
  runner_status TEXT NOT NULL CHECK(runner_status IN ('running','stopped','unknown')),
  database_healthy INTEGER NOT NULL DEFAULT 0,
  schema_version INTEGER NOT NULL DEFAULT 0,
  followed_artists INTEGER NOT NULL DEFAULT 0,
  releases INTEGER NOT NULL DEFAULT 0,
  queued_syncs INTEGER NOT NULL DEFAULT 0,
  running_syncs INTEGER NOT NULL DEFAULT 0,
  pending_deliveries INTEGER NOT NULL DEFAULT 0,
  failed_deliveries INTEGER NOT NULL DEFAULT 0,
  recent_log_entries INTEGER NOT NULL DEFAULT 0,
  oldest_queue_at TEXT,
  stale_claims INTEGER NOT NULL DEFAULT 0,
  paused_destinations INTEGER NOT NULL DEFAULT 0,
  provider_failures INTEGER NOT NULL DEFAULT 0,
  digest_backlog INTEGER NOT NULL DEFAULT 0,
  database_bytes INTEGER NOT NULL DEFAULT 0,
  last_backup_at TEXT,
  last_restore_at TEXT,
  last_restore_result TEXT NOT NULL DEFAULT ''
);

CREATE INDEX operational_snapshots_captured
  ON operational_snapshots(captured_at DESC, id DESC);
