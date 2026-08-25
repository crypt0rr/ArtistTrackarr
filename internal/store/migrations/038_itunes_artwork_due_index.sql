-- The artwork backfill's due predicate is a disjunction,
--   (itunes_artwork_next_check_at IS NULL OR itunes_artwork_next_check_at<=?)
-- which gives an index keyed on that bare column no usable range constraint, so
-- SQLite full-scanned release_groups instead. Verified with EXPLAIN QUERY PLAN
-- against a populated database: the plan was "SCAN rg".
--
-- Re-key the partial index on the same COALESCE expression the query and its
-- ORDER BY already use, so a single range constraint replaces the disjunction.
-- NULL means "never checked", which sorts first as '' and is due immediately -
-- the behaviour the disjunction expressed.
DROP INDEX IF EXISTS release_groups_itunes_artwork_due;
CREATE INDEX IF NOT EXISTS release_groups_itunes_artwork_due
  ON release_groups(COALESCE(itunes_artwork_next_check_at,''))
  WHERE itunes_id IS NOT NULL AND itunes_artwork_url='';
