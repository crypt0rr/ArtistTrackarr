CREATE TABLE IF NOT EXISTS application_logs (
    id INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL,
    level TEXT NOT NULL CHECK (level IN ('INFO','WARN','ERROR')),
    message TEXT NOT NULL,
    attributes_json TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS application_logs_created ON application_logs(created_at DESC);

CREATE TABLE IF NOT EXISTS manual_sync_requests (
    id INTEGER PRIMARY KEY,
    requested_by INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('artist','retry')),
    artist_id INTEGER REFERENCES artists(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('queued','running','completed','failed')),
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS manual_sync_status ON manual_sync_requests(status, created_at);

CREATE TABLE IF NOT EXISTS provider_health (
    provider TEXT PRIMARY KEY,
    last_success_at TEXT,
    last_failure_at TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    next_check_at TEXT,
    rate_limited INTEGER NOT NULL DEFAULT 0,
    quota_exceeded INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
