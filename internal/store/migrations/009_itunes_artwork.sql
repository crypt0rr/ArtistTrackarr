ALTER TABLE release_groups ADD COLUMN itunes_artwork_url TEXT NOT NULL DEFAULT '';
ALTER TABLE release_groups ADD COLUMN itunes_artwork_checked_at TEXT;
ALTER TABLE release_groups ADD COLUMN itunes_artwork_next_check_at TEXT;
ALTER TABLE release_groups ADD COLUMN itunes_artwork_attempts INTEGER NOT NULL DEFAULT 0;

UPDATE release_groups
SET itunes_artwork_next_check_at=datetime('now')
WHERE itunes_id IS NOT NULL AND itunes_artwork_url='';

CREATE INDEX IF NOT EXISTS release_groups_itunes_artwork_due
  ON release_groups(itunes_artwork_next_check_at)
  WHERE itunes_id IS NOT NULL AND itunes_artwork_url='';
