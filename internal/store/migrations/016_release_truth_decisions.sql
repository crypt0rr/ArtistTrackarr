-- Explicit source choices make provider disagreement actionable without
-- mutating immutable observations or canonical release metadata.
CREATE TABLE IF NOT EXISTS release_truth_decisions (
  release_group_id INTEGER PRIMARY KEY REFERENCES release_groups(id) ON DELETE CASCADE,
  state TEXT NOT NULL CHECK(state IN ('confirmed')),
  selected_provider TEXT NOT NULL CHECK(selected_provider IN ('spotify','itunes','musicbrainz')),
  selected_provider_id TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  decided_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS release_truth_decisions_provider
  ON release_truth_decisions(selected_provider, updated_at DESC);
