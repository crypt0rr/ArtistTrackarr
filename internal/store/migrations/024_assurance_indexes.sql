-- Indexes for credit-aware owner admission and paused-destination queue scans.
CREATE INDEX IF NOT EXISTS release_credits_release_artist
  ON release_credits(release_group_id,artist_id);
CREATE INDEX IF NOT EXISTS release_credits_artist_release
  ON release_credits(artist_id,release_group_id);
CREATE INDEX IF NOT EXISTS destinations_user_enabled
  ON destinations(user_id,enabled,id);
CREATE INDEX IF NOT EXISTS deliveries_status_due_destination
  ON deliveries(status,next_attempt_at,destination_id);
CREATE INDEX IF NOT EXISTS release_digest_deliveries_status_due_destination
  ON release_digest_deliveries(status,next_attempt_at,destination_id);
