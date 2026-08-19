-- Repair follows created by import paths or older application versions that
-- do not yet have an owner-scoped notification policy row. Existing policies
-- are preserved, making this migration safe to rerun.
INSERT OR IGNORE INTO follow_notification_rules(
  user_id,artist_id,delivery_mode,include_primary,include_featured,
  albums,eps,singles,compilations,announcements,release_day,updated_at)
SELECT f.user_id,f.artist_id,'inherit',1,1,1,1,1,1,1,1,
  COALESCE(NULLIF(f.created_at,''),strftime('%Y-%m-%dT%H:%M:%fZ','now'))
FROM follows f;
