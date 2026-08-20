-- Retain the bounded source upload so interrupted or failed imports can be
-- resumed without requiring the user to find the original backup again.
ALTER TABLE import_jobs ADD COLUMN payload BLOB NOT NULL DEFAULT X'';

CREATE INDEX IF NOT EXISTS import_jobs_resumable
  ON import_jobs(user_id,status,created_at)
  WHERE status IN ('interrupted','failed') AND length(payload)>0;
