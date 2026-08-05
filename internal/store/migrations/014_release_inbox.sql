-- Per-user review state for alertable release activity. Inbox entries are
-- derived from notification_events, so baseline and notification preference
-- rules remain unchanged while users can catch up after delivery failures.
CREATE TABLE IF NOT EXISTS user_release_states (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  state TEXT NOT NULL CHECK(state IN ('read','unread','snoozed','dismissed')),
  snoozed_until TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, release_group_id)
);
CREATE INDEX IF NOT EXISTS user_release_states_state
  ON user_release_states(user_id, state, snoozed_until, updated_at);
