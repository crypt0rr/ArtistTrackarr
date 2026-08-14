-- Recovery leases and explicit blocked delivery state.  The queue remains
-- at-least-once: a worker claims a row for a short period, and an expired
-- claim becomes runnable again during the next maintenance pass.

ALTER TABLE destinations ADD COLUMN transport_status TEXT NOT NULL DEFAULT 'supported';
ALTER TABLE destinations ADD COLUMN transport_message TEXT NOT NULL DEFAULT '';
UPDATE destinations
   SET transport_status='unsupported',
       transport_message='This destination uses a transport that is no longer supported; replace it.'
 WHERE lower(service) NOT IN ('ntfy','discord','telegram','generic');

ALTER TABLE manual_sync_requests ADD COLUMN lease_owner TEXT;
ALTER TABLE manual_sync_requests ADD COLUMN lease_expires_at TEXT;
ALTER TABLE manual_sync_requests ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE delivery_attempts ADD COLUMN abandoned_at TEXT;

-- SQLite cannot alter a CHECK constraint in place.  Rebuild the two queue
-- tables once so blocked rows can be retained and replayed after recovery.
CREATE TABLE deliveries_recovery_new (
  id INTEGER PRIMARY KEY,
  event_id INTEGER NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('pending','sent','failed','blocked')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  claim_owner TEXT,
  claim_expires_at TEXT,
  UNIQUE(event_id, destination_id)
);
INSERT INTO deliveries_recovery_new
  (id,event_id,destination_id,status,attempts,next_attempt_at,last_error,sent_at)
SELECT id,event_id,destination_id,status,attempts,next_attempt_at,last_error,sent_at
  FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_recovery_new RENAME TO deliveries;
CREATE INDEX deliveries_due ON deliveries(status,next_attempt_at);
CREATE INDEX deliveries_status_due_destination
  ON deliveries(status,next_attempt_at,destination_id);

CREATE TABLE release_digest_deliveries_recovery_new (
  id INTEGER PRIMARY KEY,
  run_id INTEGER NOT NULL REFERENCES release_digest_runs(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('pending','sent','failed','blocked')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  claim_owner TEXT,
  claim_expires_at TEXT,
  UNIQUE(run_id, destination_id)
);
INSERT INTO release_digest_deliveries_recovery_new
  (id,run_id,destination_id,status,attempts,next_attempt_at,last_error,sent_at)
SELECT id,run_id,destination_id,status,attempts,next_attempt_at,last_error,sent_at
  FROM release_digest_deliveries;
DROP TABLE release_digest_deliveries;
ALTER TABLE release_digest_deliveries_recovery_new RENAME TO release_digest_deliveries;
CREATE INDEX release_digest_deliveries_due
  ON release_digest_deliveries(status,next_attempt_at);
CREATE INDEX release_digest_deliveries_status_due_destination
  ON release_digest_deliveries(status,next_attempt_at,destination_id);

CREATE INDEX IF NOT EXISTS destinations_transport_status
  ON destinations(transport_status,enabled);
CREATE INDEX IF NOT EXISTS manual_sync_leases
  ON manual_sync_requests(status,lease_expires_at);
CREATE INDEX IF NOT EXISTS deliveries_claim_expiry
  ON deliveries(claim_expires_at,status);
CREATE INDEX IF NOT EXISTS release_digest_deliveries_claim_expiry
  ON release_digest_deliveries(claim_expires_at,status);
CREATE INDEX IF NOT EXISTS delivery_attempts_started
  ON delivery_attempts(status,started_at);
