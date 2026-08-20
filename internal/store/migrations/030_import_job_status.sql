-- Track whether a CSV import finished. Existing jobs predate this state and
-- are treated as complete so the migration does not rewrite their history.
ALTER TABLE import_jobs ADD COLUMN status TEXT NOT NULL DEFAULT 'complete'
  CHECK(status IN ('processing','complete','interrupted','failed'));
ALTER TABLE import_jobs ADD COLUMN finished_at TEXT;
CREATE INDEX IF NOT EXISTS import_jobs_status_created ON import_jobs(status, created_at);
