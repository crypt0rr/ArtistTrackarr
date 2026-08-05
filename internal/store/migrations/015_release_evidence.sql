-- Normalized provider snapshots let the application explain disagreements
-- without retaining raw provider payloads or changing canonical release data.
CREATE TABLE IF NOT EXISTS release_provider_evidence (
  provider TEXT NOT NULL CHECK(provider IN ('spotify','itunes','musicbrainz')),
  provider_id TEXT NOT NULL,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  title TEXT NOT NULL DEFAULT '',
  primary_type TEXT NOT NULL DEFAULT '',
  first_release_date TEXT NOT NULL DEFAULT '',
  date_precision INTEGER NOT NULL DEFAULT 0,
  provider_url TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL,
  PRIMARY KEY(provider, provider_id)
);
CREATE INDEX IF NOT EXISTS release_provider_evidence_group
  ON release_provider_evidence(release_group_id, provider, observed_at DESC);

-- An issue is a durable, globally observed discrepancy. Individual household
-- members review it through release_evidence_reviews below.
CREATE TABLE IF NOT EXISTS release_evidence_issues (
  id INTEGER PRIMARY KEY,
  release_group_id INTEGER NOT NULL REFERENCES release_groups(id) ON DELETE CASCADE,
  issue_type TEXT NOT NULL CHECK(issue_type IN ('date_conflict','title_conflict','type_conflict','missing_canonical')),
  severity TEXT NOT NULL CHECK(severity IN ('info','warning','critical')),
  fingerprint TEXT NOT NULL,
  summary TEXT NOT NULL,
  evidence_json TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL CHECK(status IN ('open','resolved')),
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  resolved_at TEXT,
  UNIQUE(release_group_id, issue_type, fingerprint)
);
CREATE INDEX IF NOT EXISTS release_evidence_issues_status
  ON release_evidence_issues(status, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS release_evidence_issues_release
  ON release_evidence_issues(release_group_id, status, issue_type);

-- Review decisions are private to each followed user. A confirmation does not
-- overwrite provider data or change notification scheduling.
CREATE TABLE IF NOT EXISTS release_evidence_reviews (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issue_id INTEGER NOT NULL REFERENCES release_evidence_issues(id) ON DELETE CASCADE,
  state TEXT NOT NULL CHECK(state IN ('confirmed','snoozed','dismissed')),
  snoozed_until TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, issue_id)
);
CREATE INDEX IF NOT EXISTS release_evidence_reviews_state
  ON release_evidence_reviews(user_id, state, snoozed_until, updated_at);
