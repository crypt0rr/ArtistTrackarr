-- Review P0/P1 indexes. These are additive and safe for existing databases.
CREATE INDEX IF NOT EXISTS idx_provider_observations_release_observed
    ON provider_observations(release_group_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_follows_artist_user
    ON follows(artist_id, user_id);
CREATE INDEX IF NOT EXISTS idx_import_rows_job_id
    ON import_rows(job_id, id);
