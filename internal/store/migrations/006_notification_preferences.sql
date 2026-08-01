CREATE TABLE IF NOT EXISTS notification_preferences (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    albums INTEGER NOT NULL DEFAULT 1,
    eps INTEGER NOT NULL DEFAULT 1,
    singles INTEGER NOT NULL DEFAULT 1,
    announcements INTEGER NOT NULL DEFAULT 1,
    release_day INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO notification_preferences(user_id, updated_at)
SELECT id, created_at FROM users;
