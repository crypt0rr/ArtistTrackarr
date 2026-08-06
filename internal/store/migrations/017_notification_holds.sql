-- Optional, owner-scoped notification holds keep provider conflicts from being
-- delivered before a household member has reviewed the evidence. The default
-- remains immediate notification for backwards compatibility.
ALTER TABLE notification_preferences
  ADD COLUMN hold_conflicting_notifications INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS notification_holds (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL CHECK(event_type IN ('announcement','release_day')),
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  issue_fingerprint TEXT NOT NULL DEFAULT '',
  planned_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('held','released','discarded')),
  created_at TEXT NOT NULL,
  released_at TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS notification_holds_pending_unique
  ON notification_holds(user_id, release_group_id, event_type)
  WHERE status='held';
CREATE INDEX IF NOT EXISTS notification_holds_user_status
  ON notification_holds(user_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS notification_holds_release
  ON notification_holds(release_group_id, status);
