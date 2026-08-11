-- Provider-backed artist credit evidence. Release groups remain the alertable
-- unit; track_title is context for guest credits, not a separate notification
-- entity.
CREATE TABLE IF NOT EXISTS release_credits (
  id INTEGER PRIMARY KEY,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('spotify','itunes','musicbrainz')),
  provider_id TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('primary','featured','guest')),
  track_title TEXT NOT NULL DEFAULT '',
  credit_name TEXT NOT NULL DEFAULT '',
  provider_url TEXT NOT NULL DEFAULT '',
  confidence TEXT NOT NULL DEFAULT 'confirmed'
    CHECK(confidence IN ('confirmed','probable','inferred')),
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  UNIQUE(release_group_id,artist_id,provider,provider_id,role,track_title)
);
CREATE INDEX IF NOT EXISTS release_credits_artist ON release_credits(artist_id,role,last_seen_at);
CREATE INDEX IF NOT EXISTS release_credits_release ON release_credits(release_group_id,last_seen_at);

-- Each provider/role receives one owner-scoped baseline. This prevents the
-- first post-upgrade guest-credit sync from flooding existing followers while
-- allowing new evidence to notify normally afterwards.
CREATE TABLE IF NOT EXISTS follow_credit_baselines (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('spotify','itunes','musicbrainz')),
  role TEXT NOT NULL CHECK(role IN ('featured','guest')),
  baseline_synced_at TEXT NOT NULL,
  PRIMARY KEY(user_id,artist_id,provider,role)
);
CREATE INDEX IF NOT EXISTS follow_credit_baselines_artist
  ON follow_credit_baselines(artist_id,provider,role);

-- Preserve the existing Spotify appearance baseline semantics when upgrading.
INSERT OR IGNORE INTO follow_credit_baselines(user_id,artist_id,provider,role,baseline_synced_at)
SELECT user_id,artist_id,'spotify','featured',spotify_appears_on_baseline_synced_at
FROM follows
WHERE spotify_appears_on_baseline_synced_at IS NOT NULL
  AND spotify_appears_on_baseline_synced_at<>'';

-- Seed the graph with already-observed release-level evidence. No guest rows
-- existed before this migration, so this is safe and prevents re-alerting
-- existing primary/featured observations.
INSERT OR IGNORE INTO release_credits(
  release_group_id,artist_id,provider,provider_id,role,provider_url,
  confidence,first_seen_at,last_seen_at)
SELECT rg.id,rg.artist_id,po.provider,po.provider_id,
  CASE WHEN COALESCE(rpe.artist_credit_role,rg.artist_credit_role)='featured'
       THEN 'featured' ELSE 'primary' END,
  COALESCE(rpe.provider_url,''),'confirmed',po.observed_at,po.observed_at
FROM provider_observations po
JOIN release_groups rg ON rg.id=po.release_group_id
LEFT JOIN release_provider_evidence rpe
  ON rpe.provider=po.provider AND rpe.provider_id=po.provider_id;
