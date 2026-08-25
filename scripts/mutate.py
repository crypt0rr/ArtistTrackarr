#!/usr/bin/env python3
"""Reintroduce known defects and confirm the tests that guard them still fail.

This project's discipline is that every fix is proven by reverting it and
watching the new test fail. Done by hand that proof happens once, at the moment
of the fix, and then decays: a later refactor can quietly restore the defect, or
move the logic to a second site the guard never reached. Six of the forty-four
findings in the v0.58.0 review were exactly that - a fix applied to some sites
and not others.

Each row below records one already-fixed defect: where it lived, the exact edit
that reintroduces it, and the test that must fail when it does. The harness
copies the tree to a temporary directory, applies one mutation, and runs that
single test. A mutation nothing detects is reported as NOT DETECTED.

Adding a row costs one line at the moment the maintainer is already performing
the reintroduction by hand, and buys a permanent guard for that site.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class Mutation:
    name: str
    file: str
    # `find` must occur EXACTLY ONCE in the file. A pattern that matches zero or
    # several times is reported as drift rather than silently mutating the wrong
    # site - which is how a catalogue rots into false confidence.
    find: str
    replace: str
    package: str
    test: str
    guards: str


CATALOGUE: list[Mutation] = [
    Mutation(
        name="pause-realign-direction",
        file="internal/store/follow_notification_rules.go",
        find="case g.Deferred && item.attempts == 0:",
        replace="case g.Deferred && item.attempts == 0 && item.hasNextAt && item.nextAt.After(g.DeferTo):",
        package="./internal/store/",
        test="TestExtendingAPauseMovesItsDeliveriesLater",
        guards="#249 - realignment moved deliveries only earlier, so extending a "
               "pause left its alerts due at the old, earlier time.",
    ),
    Mutation(
        name="pause-realign-backoff",
        file="internal/store/follow_notification_rules.go",
        find="""		case !g.Deferred && item.attempts == 0:""",
        replace="""		case !g.Deferred:""",
        package="./internal/store/",
        test="TestRealignmentDoesNotResetAnOrdinaryRetryBackoff",
        guards="#248 - resuming an unrelated follow reset an ordinary retry "
               "backoff that no pause had created.",
    ),
    Mutation(
        name="retry-cancels-pause",
        file="internal/store/notifications.go",
        find="""			if g, ok := governing[item.eventID]; ok && g.Deferred {
				key := timeText(g.DeferTo)""",
        replace="""			if g, ok := governing[item.eventID]; ok && false && g.Deferred {
				key := timeText(g.DeferTo)""",
        package="./internal/store/",
        test="TestRetryDoesNotCancelADeliberatePause",
        guards="#227/#247 - recovering a destination force-delivered a "
               "deliberately paused notification.",
    ),
    Mutation(
        name="unfollow-keeps-deliveries",
        file="internal/store/artists.go",
        find="""		if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE id IN (""",
        replace="""		if false {
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 WHERE 0 AND ? IN (""",
        package="./internal/store/",
        test="TestUnfollowCancelsItsQueuedDeliveries",
        guards="#251 - removing an artist left its queued alerts to fire months later.",
    ),
    Mutation(
        name="digest-previous-period",
        file="internal/store/calendar.go",
        # Neuter the VALUE, not the placeholder. Removing a `?` would leave a
        # surplus argument, which modernc.org/sqlite silently ignores - the
        # remaining binds then shift and produce a nonsense window rather than
        # the defect, and the test passes for the wrong reason.
        find="""	previousKey := periodStart.Add(-periodEnd.Sub(periodStart)).Format("2006-01-02")""",
        replace='\tpreviousKey := ""',
        package="./internal/store/",
        test="TestTimezoneChangeDoesNotSuppressTheNextDailyDigest",
        guards="#252 - a timezone change matched the PREVIOUS period's run and "
               "silently skipped a whole day of digests.",
    ),
    Mutation(
        name="provider-evidence-erased",
        file="internal/store/coverage.go",
        find="""	case "standby", "skipped", "deferred", "not_configured", "cooldown":""",
        replace="""	case "standby", "skipped":""",
        package="./internal/store/",
        test="TestDeferredObservationsDoNotEraseProviderEvidence",
        guards="#270 - a not-contacted observation wiped the stored provider "
               "failure message on the next tick.",
    ),
    Mutation(
        name="release-count-sentinel",
        file="internal/store/coverage.go",
        find="""	storedCount := status.ReleaseCount
	if storedCount < 0 {
		storedCount = 0
	}""",
        replace="""	storedCount := status.ReleaseCount""",
        package="./internal/store/",
        test="TestFirstProviderObservationNeverStoresTheSentinel",
        guards="#268 - the retain-previous sentinel was stored verbatim on the "
               "first row and preserved forever, rendering -1 releases.",
    ),
    Mutation(
        name="release-link-ignores-truth",
        file="internal/store/release_helpers.go",
        find="""func ReleaseLink(release Release) string {
	switch release.TruthProvider {""",
        replace="""func ReleaseLink(release Release) string {
	switch "" {""",
        package="./internal/store/",
        test="TestReleaseLinkHonoursTheConfirmedProvider",
        guards="#272 - digests, notifications and the ICS export linked past a "
               "household's confirmed provider.",
    ),
    Mutation(
        name="log-loss-never-clears",
        file="internal/store/operational.go",
        find="""	if (snapshot.DroppedLogEntries > 0 || snapshot.LogWriteFailures > 0) &&
		operationalWithin(snapshot.LastLogLossAt, now, operationalLossWindow) {""",
        replace="""	if snapshot.DroppedLogEntries > 0 || snapshot.LogWriteFailures > 0 {""",
        package="./internal/store/",
        test="TestLogLossAndContentionReasonsClearOnceTheyStop",
        guards="#267 - one dropped record pinned the instance to degraded until "
               "it restarted.",
    ),
    Mutation(
        name="delivery-state-detached",
        file="internal/jobs/jobs.go",
        find="		base = context.WithoutCancel(ctx)",
        replace="		base = ctx",
        package="./internal/jobs/",
        test="TestDeliveryOutcomeIsRecordedAfterTheRunnerContextIsCancelled",
        guards="#199/#223/#254 - a notification the provider had accepted was "
               "recorded as failed during shutdown and re-sent on restart.",
    ),
    Mutation(
        name="shutdown-fails-manual-syncs",
        file="internal/jobs/jobs.go",
        find="""		if errors.Is(syncErr, context.Canceled) || errors.Is(syncErr, context.DeadlineExceeded) {""",
        replace="""		if false {""",
        package="./internal/jobs/",
        test="TestShutdownLeavesManualSyncRequestsRetryable",
        guards="#255 - a shutdown durably marked queued Sync now requests as "
               "failed, defeating RecoverExpiredWork.",
    ),
    Mutation(
        name="shutdown-fails-syncs",
        file="internal/jobs/jobs.go",
        find="""		if ctx.Err() != nil {
			r.logger.Info("artist synchronization stopped for shutdown",""",
        replace="""		if false {
			r.logger.Info("artist synchronization stopped for shutdown",""",
        package="./internal/jobs/",
        test="TestShutdownDoesNotCountUntouchedSyncsAsFailures",
        guards="#273 - a graceful shutdown reported untouched artists as "
               "synchronisation failures.",
    ),
    Mutation(
        name="listenbrainz-null-counts",
        file="internal/jobs/jobs.go",
        find="		if !ok || !stats.HasData() {",
        replace="		if !ok {",
        package="./internal/jobs/",
        test="TestListenBrainzNullCountsDoNotOverwriteKnownTotals",
        guards="#274 - a null count decoded to zero and overwrote a known "
               "listener total.",
    ),
    Mutation(
        name="send-budget-equals-transport",
        file="internal/jobs/jobs.go",
        find="const notificationSendTimeout = notify.DefaultSendTimeout * (maxDeliveryWorkers + 1)",
        replace="const notificationSendTimeout = notify.DefaultSendTimeout",
        package="./internal/jobs/",
        test="TestQueuedSendKeepsItsTransportBudgetUnderTheWorkerWatchdog",
        guards="#228/#253 - queue wait was charged against the transport budget, "
               "so a send behind a slow one failed on its own queueing.",
    ),
    Mutation(
        name="itunes-truncation-silent",
        file="internal/catalog/itunes.go",
        find="""	if collections >= lookupPageSize {
		return result, &ITunesCatalogTruncatedError{ArtistID: artistID, Limit: lookupPageSize}
	}""",
        replace="",
        package="./internal/catalog/",
        test="TestITunesAlbumLookupIssuesOneRequestBecauseOffsetIsIgnored",
        guards="#256 - a truncated catalogue reported a clean healthy check and "
               "suppressed the MusicBrainz fallback.",
    ),
    Mutation(
        name="itunes-credit-artist-only",
        file="internal/catalog/itunes.go",
        find="	return creditIncludesArtist(artistNameField, artist) || creditIncludesArtist(trackName, artist)",
        replace="	return creditIncludesArtist(artistNameField, artist)",
        package="./internal/catalog/",
        test="TestITunesGuestCreditsReadTheTrackTitle",
        guards="#257 - guest credits named in the track title were discarded; "
               "31 of 50 live rows for one artist.",
    ),
    Mutation(
        name="itunes-no-compilations",
        file="internal/catalog/itunes.go",
        find="""	kind := "album"
	if isVariousArtists(artistName) {
		kind = "compilation"
	}""",
        replace="""	kind := "album"
""",
        package="./internal/catalog/",
        test="TestITunesCompilationsCarryTheSecondaryType",
        guards="#258 - iTunes releases could never carry a Compilation secondary "
               "type, so the per-follow toggle did nothing.",
    ),
    Mutation(
        name="redaction-eats-diagnostics",
        file="internal/logging/logging.go",
        find="""	if _, safe := safeDiagnosticKeys[key]; safe {
		return false
	}""",
        replace="",
        package="./internal/logging/",
        test="TestDeliberateDiagnosticFieldsSurviveRedaction",
        guards="#264 - the invite-link fingerprint and two other deliberate "
               "operator signals were destroyed by redaction.",
    ),
    Mutation(
        name="presink-records-lost",
        file="internal/logging/logging.go",
        find="""			if sink == nil && !h.sink.attached && len(h.sink.pending) < pendingSinkLimit {
				h.sink.pending = append(h.sink.pending, entry)
			}""",
        replace="",
        package="./internal/logging/",
        test="TestRecordsLoggedBeforeTheSinkExistsArePersisted",
        guards="#275 - startup security warnings never reached the persisted log.",
    ),
    Mutation(
        name="csrf-cookie-strict",
        file="internal/web/core.go",
        find="""				HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
				MaxAge: int(sessionLifetime.Seconds()),
			})
		}
		if r.Method == http.MethodPost {""",
        replace="""				HttpOnly: true, Secure: a.cfg.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode,
				MaxAge: int(sessionLifetime.Seconds()),
			})
		}
		if r.Method == http.MethodPost {""",
        package="./internal/web/",
        test="TestCsrfCookieIsNoStricterThanTheSession",
        guards="#263 - a cross-site entry kept the session but dropped the CSRF "
               "token, 403ing every open form.",
    ),
    Mutation(
        name="artist-actions-replay-search",
        file="internal/web/artists.go",
        find="""	for _, key := range []string{"genre", "country", "type", "page"} {""",
        replace="""	for _, key := range []string{"q", "genre", "country", "type", "page"} {""",
        package="./internal/web/",
        test="TestArtistActionsDoNotReplayTheDiscoveryQuery",
        guards="#262 - every local bookkeeping action replayed up to three "
               "provider searches.",
    ),
    Mutation(
        name="lapsed-pause-still-paused",
        file="internal/web/web.go",
        find="	return rule.PausedUntil != nil && rule.PausedUntil.After(time.Now().UTC())",
        replace="	return rule.PausedUntil != nil",
        package="./internal/web/",
        test="TestLapsedPauseIsNotReportedAsPaused",
        guards="#260 - a lapsed pause reported as active forever, hiding the "
               "Pause control behind a Resume button.",
    ),
    Mutation(
        name="deep-link-return-path",
        file="internal/web/core.go",
        find="""			if r.Method == http.MethodGet {
				if next := localReturnPath(r.URL.RequestURI(), "", ""); next != "" && next != "/" {""",
        replace="""			if false {
				if next := localReturnPath(r.URL.RequestURI(), "", ""); next != "" && next != "/" {""",
        package="./internal/web/",
        test="TestUnauthenticatedDeepLinkReturnsAfterSignIn",
        guards="#290 - an ICS deep link dropped the reader on the dashboard "
               "after signing in.",
    ),
    Mutation(
        name="schema-downgrade-guard",
        file="internal/store/schema.go",
        find="""	if err := s.verifySchemaNotAhead(ctx, highestEmbedded); err != nil {
		return err
	}""",
        replace="""	_ = highestEmbedded""",
        package="./internal/store/",
        test="TestADatabaseFromANewerReleaseRefusesToStart",
        guards="An older binary opening a database a newer release migrated "
               "starts cleanly and reports ready, then fails on the first "
               "query touching what 035-037 dropped.",
    ),
    Mutation(
        name="reserved-network-policy",
        file="internal/netpolicy/netpolicy.go",
        # Drop one network from the shared policy. Both the webhook and the
        # artwork fetcher read this list, so a single entry going missing has
        # to be visible from the package that owns it.
        find="""	"100.64.0.0/10",   // RFC 6598 shared address space
""",
        replace="",
        package="./internal/netpolicy/",
        test="TestReservedAddressSpaceIsRejected",
        guards="The reserved-network list used to be written out twice with a "
               "comment on each copy asking the reader to keep them aligned.",
    ),
    Mutation(
        name="audit-event-reachability",
        file="internal/web/settings.go",
        # Leave the emitter in the source so the grep-based guard stays green.
        # Only a test that drives the route and reads the server's own log can
        # tell a live emitter from an unreachable one.
        find="""		a.logger.Info("calendar feed token issued", "event", "auth.feed_token_issued", "user_id", session.User.ID)""",
        replace="""		if session.User.ID < 0 {
			a.logger.Info("calendar feed token issued", "event", "auth.feed_token_issued", "user_id", session.User.ID)
		}""",
        package="./internal/web/",
        test="TestCredentialLifecycleEventsAreRecorded",
        guards="#265 - the audit trail had no behavioural test at all: the old "
               "one emitted the events itself and asserted they came back.",
    ),
    Mutation(
        name="sql-bind-arity",
        file="internal/store/notification_holds.go",
        # #239 verbatim: a third bind argument for a two-placeholder query.
        # modernc.org/sqlite ignores surplus arguments, so this executes and
        # returns rows - it just answers a different question.
        find="""// NotificationHoldsForRelease returns pending holds for a followed release.
func (s *Store) NotificationHoldsForRelease(ctx context.Context, userID, releaseID int64) ([]NotificationHold, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT h.id,h.user_id,h.release_group_id,a.name,rg.title,
		h.event_type,h.title,h.body,h.reason,h.issue_fingerprint,h.planned_at,h.status,h.created_at,h.released_at
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE h.user_id=? AND h.release_group_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status='held'
		ORDER BY h.created_at DESC,h.id DESC`, userID, releaseID)""",
        replace="""// NotificationHoldsForRelease returns pending holds for a followed release.
func (s *Store) NotificationHoldsForRelease(ctx context.Context, userID, releaseID int64) ([]NotificationHold, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT h.id,h.user_id,h.release_group_id,a.name,rg.title,
		h.event_type,h.title,h.body,h.reason,h.issue_fingerprint,h.planned_at,h.status,h.created_at,h.released_at
		FROM notification_holds h JOIN release_groups rg ON rg.id=h.release_group_id
		JOIN artists a ON a.id=rg.artist_id
		WHERE h.user_id=? AND h.release_group_id=? AND `+followedReleasePredicate("h.user_id")+` AND h.status='held'
		ORDER BY h.created_at DESC,h.id DESC`, userID, releaseID, userID)""",
        package="./internal/store/",
        test="TestEverySQLStatementBindsAsManyArgumentsAsItHasPlaceholders",
        guards="#239 - a surplus bind argument. Verified that this test is the "
               "only one in the package that fails when it is reintroduced.",
    ),
    Mutation(
        name="deferred-conflict-hold",
        file="internal/store/release_helpers.go",
        find="""	if p.HoldConflictingNotifications && (selected.rule.queuesImmediate(now) || deferredDelivery) && !bypassConflictHold {""",
        replace="""	if p.HoldConflictingNotifications && selected.rule.queuesImmediate(now) && !bypassConflictHold {""",
        package="./internal/store/",
        test="TestDeferredDeliveryStillTakesTheConflictHold",
        guards="A paused follow defers rather than discards, so it must still "
               "take the conflict hold - otherwise an unreviewed release is "
               "released at pause expiry with no hold row.",
    ),
    Mutation(
        name="raw-clock-time-in-go",
        file="internal/web/web.go",
        find="""		return "Paused until " + formatTime(*rule.PausedUntil, timezones...)""",
        replace="""		return "Paused until " + rule.PausedUntil.Format("2006-01-02 15:04")""",
        package="./internal/web/",
        test="TestNoTemplateRendersARawClockTime",
        guards="#261 - a raw UTC clock with no zone label. The original guard "
               "globbed templates only, so the Go instance was invisible to it.",
    ),
]


def run(mutation: Mutation, workdir: Path, verbose: bool) -> tuple[str, str]:
    """Return (status, detail). status is DETECTED, NOT DETECTED or DRIFT."""
    target = workdir / mutation.file
    if not target.exists():
        # The working tree is built from `git ls-files`, so a catalogue row
        # pointing at a file that is new and not yet staged lands here. Say so
        # instead of raising FileNotFoundError from three frames down.
        return "DRIFT", f"{mutation.file} is not tracked by git (stage it first)"
    original = target.read_text()
    occurrences = original.count(mutation.find)
    if occurrences != 1:
        return "DRIFT", f"pattern matches {occurrences} times in {mutation.file}, expected exactly 1"
    target.write_text(original.replace(mutation.find, mutation.replace))

    build = subprocess.run(
        ["go", "build", "./..."], cwd=workdir, capture_output=True, text=True
    )
    if build.returncode != 0:
        target.write_text(original)
        return "DRIFT", "mutated tree does not compile: " + build.stderr.strip().splitlines()[0][:160]

    result = subprocess.run(
        ["go", "test", mutation.package, "-run", "^" + mutation.test + "$", "-count=1"],
        cwd=workdir, capture_output=True, text=True,
    )
    target.write_text(original)
    if result.returncode != 0:
        return "DETECTED", ""
    detail = f"{mutation.test} still passes"
    if verbose:
        detail += "\n" + result.stdout.strip()[:400]
    return "NOT DETECTED", detail


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--only", help="run only mutations whose name contains this substring")
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()

    selected = [m for m in CATALOGUE if not args.only or args.only in m.name]
    if not selected:
        print(f"no mutation matches {args.only!r}", file=sys.stderr)
        return 2

    with tempfile.TemporaryDirectory(prefix="artisttrackarr-mutate-") as tmp:
        workdir = Path(tmp) / "repo"
        # Copy the tracked tree only; .git and build caches are large and unused.
        tracked = subprocess.run(
            ["git", "ls-files"], cwd=REPO, capture_output=True, text=True, check=True
        ).stdout.split()
        for relative in tracked:
            source, destination = REPO / relative, workdir / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)

        failures: list[tuple[Mutation, str, str]] = []
        for mutation in selected:
            status, detail = run(mutation, workdir, args.verbose)
            marker = {"DETECTED": "ok  ", "NOT DETECTED": "FAIL", "DRIFT": "DRIFT"}[status]
            print(f"{marker}  {mutation.name:32} {mutation.test}")
            if detail:
                print(f"      {detail}")
            if status != "DETECTED":
                failures.append((mutation, status, detail))

    print()
    print(f"{len(selected) - len(failures)}/{len(selected)} mutations detected")
    for mutation, status, detail in failures:
        print(f"  {status}: {mutation.name} -- {mutation.guards}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
