-- Calendar feed tokens are opaque, revocable credentials for unattended ICS
-- subscriptions. Only a SHA-256 digest is persisted; the raw token is shown
-- once when the owner generates or rotates it.
CREATE TABLE IF NOT EXISTS calendar_feed_tokens (
  user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  token_hash BLOB NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT
);

CREATE INDEX IF NOT EXISTS calendar_feed_tokens_active
  ON calendar_feed_tokens(token_hash, revoked_at, expires_at);
