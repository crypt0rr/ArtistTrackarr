-- follow_credit_baselines was created by migration 022 to "prevent the first
-- post-upgrade guest-credit sync from flooding existing followers", but nothing
-- ever implemented that: the table had exactly one writer, an INSERT OR IGNORE,
-- and no SELECT anywhere in the codebase or in any later migration. Its index
-- could therefore never be chosen by a query plan, and every write paid to
-- maintain a B-tree nothing read.
--
-- The guarantee it documented is genuinely provided elsewhere, by the
-- per-follow baseline gating in the announcement path (see the initial-sync and
-- provider-baseline branches in releases.go), which is why removing this changed
-- no behaviour. The stored rows have no consumer by construction, so there is
-- nothing to migrate forward.
DROP INDEX IF EXISTS follow_credit_baselines_artist;
DROP TABLE IF EXISTS follow_credit_baselines;
