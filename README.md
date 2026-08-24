# ArtistTrackarr

A self-hosted household dashboard that watches Spotify for new albums, EPs, singles, and compilations,
using MusicBrainz for stable artist identity with Apple/iTunes fallback observations,
public ListenBrainz artist popularity, and sends announcement and release-day notifications through
[Shoutrrr](https://containrrr.dev/shoutrrr/).

## Example dashboard

The following screenshot shows how ArtistTrackarr can look in a production
deployment:

![ArtistTrackarr production dashboard](docs/images/artisttrackarr-production.png)

## Quick start

1. Copy `.env.example` to `.env` and set `SETUP_TOKEN`,
   `APP_ENCRYPTION_KEY`, and `SESSION_SECRET`. Each secret should be a random
   value of at least 32 characters.
2. Set `MUSICBRAINZ_CONTACT` to a real email address or project URL.
3. Start the application:

   ```console
   docker compose pull
   docker compose up -d
   ```

4. Open `http://localhost:8080/setup`, enter `SETUP_TOKEN`, and create the
   first administrator with a unique username.

Members can change their password from Settings after confirming their current
password. The change revokes every active session, including the session used
to submit it, and revokes any calendar feed token, because that token grants
unauthenticated read access to the member's upcoming releases for up to a year.
Generate a new feed URL from Settings afterwards if you use one. Administrators
can also issue the existing one-hour reset link, which revokes both in the same
way. Email remains the recovery and login identity.

Application data remains on the existing Compose volume mapping so upgrades do
not move user data. Docker Compose names the container `artist-trackarr` for
predictable logs and administration commands. Use the backup helper below to
resolve the project-prefixed volume mounted at `/data`; do not guess its Docker
volume name.

Release-group artwork follows a validated cascade: Spotify artwork first,
then direct Apple/iTunes artwork, then Cover Art Archive artwork for real
MusicBrainz IDs, and finally a local placeholder. Apple artwork is loaded
directly by the browser and is never stored, cached, or proxied by the app;
the dashboard labels it with Apple attribution and an Apple Music link. A
bounded background backfill gradually fills artwork on existing iTunes
releases without creating releases or notifications.

Use the moon/sun button in the header to switch between light and dark mode;
your choice is remembered in the browser.
The running application version and project repository are available in the
footer. The current release is `v0.59.0`; local and published images use the
same source-controlled semantic version. Operational timestamps are stored in
UTC and rendered in the signed-in administrator's configured timezone in the
web UI and downloaded assurance report; machine-readable JSON and CSV exports
remain RFC3339 UTC. Existing databases are normalized automatically during the
v0.20.0 migration.

Background synchronization and application-log persistence shut down in an
orderly fashion before SQLite is closed. Routine page loads return a generic
error when a data lookup fails, while the detailed cause remains in structured
logs. Static assets use immutable, semantic-version and content-hash stamped
URLs, so an asset change invalidates browser caches even before the next
release version bump; unversioned paths remain available for compatibility.

The v0.43.0 retention-governance release adds an administrator dry-run and
explicit cleanup action for bounded operational state. Notification events,
delivery rows, inbox state, blocked work, and delivery-attempt audit records
are retained indefinitely; only expired sessions/tokens, old login-attempt
records, completed transient work, and application logs inside the documented
windows are eligible for cleanup. The v0.42.0 reliability release routes
production SQLite writes through a bounded busy/locked retry path, rejects
malformed operational timestamps instead of silently showing zero values, and
replays complete multi-statement write transactions after an intermediate or
commit-time lock, and preflights database paths and MusicBrainz contact input
before startup. The v0.40.0 operations
release adds strict boolean configuration validation,
polling-cadence-aware provider freshness, and bounded scheduler, provider, and
delivery metrics in the administrator diagnostics report. The v0.39.0
operational-confidence release adds checksum-protected backups,
restore state verification, pinned Docker helper images, an authenticated
container smoke rehearsal, and freshness-aware provider health reporting. The
v0.38.0 hardening release adds paused-destination admission, credit-aware
owner visibility, atomic migration recovery, connection-time notification
target checks, and a volume-resolving backup workflow. These controls keep
manual work from starving scheduled synchronization or the SQLite writer while
preserving the existing routes and notification behavior.

The v0.36.0 hardening release tightened startup validation, request handling,
build identity, and operational safety without changing release semantics. The
previous reliability work also recovers from panics in scheduled and delivery
work, applies persisted MusicBrainz cooldowns with bounded provider retries,
and bounds provider caches and catalog pagination so unusually large catalogs
cannot exhaust memory or silently apply incomplete results. ListenBrainz
retries transient responses with the ArtistTrackarr User-Agent and keeps prior
aggregate values when a response omits an artist; Cover Art Archive failures
retain stale artwork or the local placeholder without negative-caching
transient outages.

The v0.23.0 hardening defaults keep setup and login attempts bounded, use both
the trusted peer and normalized account identity for durable login throttling,
accept forwarded client addresses only from explicitly trusted proxy networks,
and redact notification destination credentials from delivery errors. Notification
targets that resolve to loopback, private, link-local, metadata, shared, or
reserved networks are blocked by default; enable
`ALLOW_PRIVATE_NOTIFICATION_TARGETS` only for a trusted household that needs a
local notification service.

The Artists page uses 50-item, page-number navigation for followed artists.
Genre, country, artist-type, and search filters remain in the page URL while
browsing, and the watchlist total stays separate from the filtered result count.

The **Release calendar** gives each member a timezone-aware view of precise,
day-dated releases from their followed artists. Calendar entries retain source
confidence, review/hold state, and links to the internal release details page;
the authenticated ICS export contains the next year of releases. Settings can
also issue a revocable, one-year private feed URL for unattended calendar
subscriptions; the raw token is shown only once and is never stored. Partial
and unknown dates are kept out of the export rather than being assigned a
misleading day.

Settings can optionally queue a daily or weekly upcoming-release digest at the
member's existing reminder time. Digest runs are deduplicated per local period,
use the same encrypted destinations and bounded retry policy as normal
notifications, and are disabled by default. They are informational only and
never create release events or alter provider polling.

The **Release Trust Center** summarizes per-artist provider coverage. It shows
when Spotify, Apple/iTunes, or MusicBrainz last returned data, whether releases
are confirmed by multiple sources or currently rely on a fallback, provider
cooldowns, and the next scheduled check. Use **Sync now** for a followed artist
to queue the normal provider strategy; it does not bypass rate limits or alter
notification deduplication.

Provider health freshness follows the configured polling cadence and becomes
stale after two missed checks. The administrator diagnostics report also shows
bounded process-local scheduler, provider-cooldown, and delivery counters;
these counters never include credentials, URLs, notification bodies, or
provider payloads.

The dashboard and Trust Center also include **Watchlist assurance**. Each
followed artist is classified as healthy, delayed, degraded, or pending from
recent provider outcomes and release history; the dashboard surfaces the most
important gaps first without exposing provider credentials or payloads. Admins
can open **System diagnostics** or download a redacted support report with
database, scheduler, queue, and provider-status counters. The report is safe
to share because it excludes destination URLs, credentials, notification
bodies, and provider error text.

The v0.44.0 operations release also persists a redacted hourly health snapshot
for up to 30 days. Snapshots contain only scheduler state, queue/provider
counters, database size, and backup/restore timestamps, so administrators can
see whether a problem is recurring after a restart without retaining provider
payloads or notification content. `/readyz` is a bounded,
unauthenticated database/schema/write readiness probe; detailed queue, provider,
runner, and backup state remains available only through authenticated admin
diagnostics. A provider cooldown or overdue backup does not cause a restart
loop.

Operational health treats a missing backup marker on a fresh installation as an
informational “backup not yet established” note rather than a service failure.
Transient provider failures are shown in diagnostics but affect the overall
status only when the latest failure remains unresolved for at least one hour;
digest backlog affects the status after fifteen minutes. These thresholds keep
the readiness/administration signal actionable without hiding persistent
failures, and the diagnostics page still shows the underlying counters and
oldest timestamps.

The v0.46.0 approval catch-up release ensures an explicit provider or evidence
approval creates one owner-scoped inbox event and delivery when no earlier
notification hold existed. It preserves discarded holds, waits for all blocking
evidence issues to resolve, and orders the default inbox with unread releases
first; marking an item read returns to the first page so the next unread item
is immediately visible. The v0.45.0 operations release adds an
administrator-only machine-readable
diagnostics document at `/admin/diagnostics.json`. It contains bounded queue,
provider, runner, database, and retention counters without credentials,
destination URLs, provider error text, or notification bodies. The admin page
also offers a paginated, CSV-safe delivery-audit export before any future
history policy is considered. The export contains notification text and should
be handled as confidential household data; a complete encrypted database
backup remains the authoritative recovery archive.

The v0.47.0 reliability release keeps fresh-install backup state informational
until a backup is established, applies age thresholds to provider failures and
digest backlog, exposes the true due-artist backlog and oldest due time, and
sends Telegram notifications through the bounded, sender-owned HTTP client
instead of Shoutrrr's process-global JSON client. Startup failures drain
persisted application logs before closing SQLite, and database paths are
escaped as SQLite file URIs so unusual filenames remain unambiguous. Admin
diagnostics now distinguish database file size from reusable SQLite freelist
space; retention cleanup is bounded and does not run an automatic `VACUUM`, so
operators can compact a backup or maintenance window deliberately. Member-owned
work is bounded at 25 notification destinations and 100 active provider
identifications per account. CSV imports report `processing`, `complete`,
`failed`, or `interrupted` state; hourly maintenance marks stale uploads as
interrupted instead of presenting a partial upload as complete. Diagnostics
also flag pending delivery retries parked more than 24 hours into the future,
which is a useful clock-skew or bad-schedule signal rather than a delivery
failure by itself.

The v0.48.0 catalog release uses one Album/EP/Single heuristic for Spotify and
Apple/iTunes observations. Explicit standalone title labels take precedence,
then one track is a Single, two through six tracks are an EP, and seven or
more tracks are an Album; compilations remain Albums. A unique normalized
title/date match can merge provider records even when those derived types
differ, while ambiguous matches are kept separate for review.

The v0.48.2 notification safety patch bounds payloads per supported transport.
Telegram checks the final rendered Unicode message, Discord rejects payloads
beyond Shoutrrr's total chunk budget instead of silently omitting content, and
ntfy/generic webhooks have a conservative 64 KiB cap. Oversized messages remain
visible as failed deliveries with an actionable transport-limit error; normal
release and digest notifications are unchanged.

The v0.48.1 consistency patch keeps invitation failures generic while retaining
actionable username and timezone guidance, and emits RFC 5545-safe calendar
`URL:` properties by normalizing whitespace, Unicode query values, and malformed
percent escapes.

The v0.49.0 calendar subscription patch adds a revocable, owner-scoped ICS feed
token. Generate or rotate it in Settings and paste the private URL into a
calendar app; tokens expire after one year and can be revoked immediately.
Exports paginate the next year of precise releases and fail clearly instead of
silently truncating an unusually large watchlist.

The v0.50.0 reliability patch keeps an empty-destination digest pending and
replayable without creating duplicate periods, suppresses duplicate digest
periods after a timezone change, and keeps replayable pending runs separate
from completed history. Calendar exports now fold ASCII and Unicode lines
within the RFC 5545 75-octet limit, and iTunes artist validation reuses a
bounded cache to avoid duplicate concurrent lookups. The v0.51.0 reliability
patch widens release discovery after a prolonged provider outage to the last
successful catalog check, validates timezones before persistence, invalidates
Spotify release caches for scheduled observations, and keeps a direct artist
management action visible when the dashboard has no insight data.
The v0.52.0 hardening patch keeps readiness reads off the serialized writer,
prevents authenticated pages from being cached after logout, resumes provider
health refreshes after back-forward-cache restores, adds a plain-HTTP clipboard
fallback, and mounts the optional Spotify client secret through a Docker secret.
The v0.54.0 reliability patch verifies imported MusicBrainz identities before
release polling, preserves artist metadata in CSV exports and imports,
including sort names, types, countries, disambiguation, and Spotify artwork
URLs, while remaining compatible with older six-column exports. Static assets
disable byte ranges so compressed responses cannot advertise offsets for an
uncompressed representation.
The v0.54.1 patch closes a reader-pool exhaustion path in digest scheduling by
materializing eligible users before nested calendar and notification-rule
queries begin.
The v0.54.2 patch bounds manual synchronization per household member and
interleaves claims across members so one large manual refresh cannot starve
other users' requests.
The v0.54.3 patch prevents destinations added after an event or digest run
from being backfilled into that historical work. New destinations receive
future events, while existing delivery rows remain replayable after recovery.
The v0.54.4 patch renders pending artist-resolution retry times in each
member's configured timezone, matching the rest of the authenticated UI.
The v0.54.5 patch adds a testable HTTP bind seam and regression coverage so
startup bind failures remain observable before readiness is announced. The
v0.54.6 patch makes every lifecycle drain/serve failure a non-zero process
result, so supervisors cannot treat an unsafe shutdown as successful.
The v0.54.7 patch includes digest deliveries in the household admin audit,
pagination, export, and detail views so normal notifications and scheduled
release digests have one consistent operational history.
The v0.54.8 patch retains bounded CSV source payloads for interrupted or
failed imports and adds an owner-scoped Resume import action. Resuming creates
a new idempotent import job while preserving the original incomplete audit.
The v0.54.10 patch makes `/readyz` fail closed when the migrated database is
readable but cannot complete a bounded write probe, so read-only/full SQLite
failures are distinguishable before work is admitted. The v0.54.9 patch clears
retained CSV payloads as soon as an import completes; only interrupted or
failed jobs retain their bounded source for recovery.
The v0.54.11 patch keeps compilation releases independent from the account
Albums switch, so the per-follow Compilations rule remains authoritative for
Spotify, iTunes, and MusicBrainz observations.
The v0.54.12 security patch blocks Teredo transition addresses during
notification-target resolution, alongside the existing private, 6to4, and
NAT64 ranges, so transition mechanisms cannot bypass outbound target policy.
The v0.54.13 security patch applies the same reserved, transition, and
IPv4-mapped address protections to Cover Art Archive DNS resolution, keeping
approved artwork hosts from reaching non-public endpoints after a rebinding.
The v0.54.14 patch verifies that SQLite foreign-key enforcement remains enabled
after the complete migration sequence; startup now fails closed if a migration
leaves ownership cascades disabled.
The v0.54.15 patch keeps release-day notifications clear when a release is
visible through a followed guest or featured artist: the event title and body
identify the credited appearance while preserving one owner-scoped event and
delivery for the shared release.

The v0.54.17 patch restores MusicBrainz guest-credit discovery, which had never
returned a result: the recording search response was decoded with browse-style
field names, so every credit was discarded before it could be projected onto a
release group. The search is deliberately bounded to one page, matching the
Apple/iTunes credit search, because credit discovery runs for every artist on
every scheduled check and MusicBrainz permits one request a second. A release
group discovered this way is dated from the search document; the v0.54.18 note
below corrects what this paragraph originally claimed. The same patch makes the
calendar month grid say when a month holds more dated releases than the grid
shows, instead of dropping the remainder silently, and adds the timezone
abbreviation to rendered timestamps so a displayed time is unambiguous.

The v0.54.18 patch corrects the release dates on MusicBrainz guest credits.
v0.54.17 read a release date only from the release group embedded in a
recording search result, which never carries one, so every newly discovered
credit was stored without a date and stayed invisible to notifications, the
release calendar, the ICS feed, and the release inbox. Each release in a search
result carries its own date, and a release group's first release date is the
earliest of its releases, so that earliest date is now applied at its true
precision: a year-only upstream date stays a year and is not promoted to a
release day. A credit whose search result has no usable date at all is skipped
rather than stored undated, matching the Apple/iTunes credit path, while a
credit on a release group already known from the release-group browse keeps
that catalogue date.

The v0.55.0 release closes a full review backlog. Three faults were reachable
through ordinary documented use. Compose declared the optional Spotify secret
unconditionally, so the documented quick start could not produce a running
container without Spotify credentials; an absent optional secret file now means
"not configured" while the three required secrets still fail closed. Application
logs were written before redaction ran, so every value the redactor hides from
the diagnostics panel was still printed in full to standard output and shipped
to whatever collects container logs; redaction now happens once, before the
record reaches the log handler, and covers persistent and grouped attributes.
An artist carrying a Spotify ID on a deployment without Spotify credentials was
never rescheduled and held a due slot forever, starving every other followed
artist.

Alongside those: the calendar feed token no longer reaches any log through the
request path; a release truth decision now belongs to the member who recorded
it, with administrator override; following an artist can no longer repoint the
household-shared Spotify identity, artwork, or genres another member already
relies on; guest credits follow the featured-appearance switch as documented
rather than the primary one; a follow set to "Digest only" now receives a digest
run even when the account digest is off, instead of recording an event that is
delivered nowhere and can never be re-queued; and the account-wide failure
counter still counts every attempt but no longer refuses one before the password
is checked, so a known email address cannot by itself keep an account locked
out. That last property depends on the deployment identifying real client
addresses: see the reverse proxy section, and the v0.57.0 note below.

Three provider integrations were reading fields their upstream never sends. The
MusicBrainz artist search decoded a genres key that only the lookup endpoint
returns, which also made every genre-less artist cost an extra lookup on every
scheduled sync; the iTunes wrong-artist guard compared a field Apple sends only
on track rows, so a stale provider identity is now surfaced for review instead
of importing another artist's discography; and the iTunes release cache is now
invalidated before a due check, so a scheduled poll reaches the provider rather
than replaying a day-old response, suppressing the MusicBrainz fallback, and
recording health for a request that never happened. Their fixtures previously
repeated the same wrong field names, so the tests validated each struct against
itself; they now use real payload shapes.

The v0.56.0 release closes the second review batch. Apple's lookup endpoint
ignores the paging offset, so an artist with a full page of albums never
satisfied the short-page terminator: the catalog fetch issued a hundred
identical requests and then discarded the result, cooling the provider down on
every sync. It now issues one bounded request and keeps what it returns.
Pausing a follow defers its notifications to the moment the pause expires
instead of discarding them permanently, and the release digest filters while
paging so an eligible release is no longer lost behind a page of ineligible
ones. Retention keeps interrupted and failed imports that still carry a
resumable payload. The shutdown stage budgets now sum to less than the
container stop grace period, so a slow shutdown is no longer killed part-way
through the log drain.

Smaller corrections in the same release: the calendar truncation notice no
longer points at an export that cannot cover the month being viewed; the
unauthenticated calendar feed route is throttled by caller rather than by the
token the caller supplies; MusicBrainz aliases are used instead of being
fetched and discarded; Spotify artist artwork uses a rendition suitable for
display rather than the smallest thumbnail; and eighteen test assertions no
longer call the testing package's fatal helpers from inside HTTP handler
goroutines, where they terminate the handler instead of reporting the failure.

The v0.56.1 patch adds provider contract testing. Several defects in this
project shared one shape: a struct decoded a field name the provider never
sends, and the fixture beside it was hand-written with the same wrong name, so
the test proved the struct matched itself rather than the API while the feature
silently returned nothing. Compilation, vet, the linters, and the race detector
cannot see that. Responses captured verbatim from MusicBrainz, Apple/iTunes, and
ListenBrainz are now checked in and run through the real parsing code, and two
structural checks assert that every JSON field the code declares actually
appears in a captured payload - one across all payloads, and one narrowed to the
endpoint each response struct belongs to, because a field can be real on one
endpoint and dead on another. Spotify cannot be captured without client
credentials, so its fields are listed explicitly as unverified. No runtime
behaviour changes.

The v0.56.2 patch finishes the Truth Loop ownership work. A release truth
decision is one shared row per household, so a decision another member recorded
previously rendered on the release page as though the viewer had made it,
including its free-text reason. The page now says whether a decision is yours or
another member's, the Clear action appears only for the member who recorded it
or for an administrator - matching what the store already enforced - and the
shared-versus-private distinction is documented alongside the private evidence
review actions it is easily confused with.

The v0.57.0 release fixes six defects found by a third review, four of which
were introduced by earlier fixes in this series.

A notification that took longer than five seconds to send could never record
that it had been sent: the durable-state budget was started before the send
rather than after it, and a send is allowed ten seconds. The delivery stayed
pending with its attempt count unchanged and was re-sent on every later tick,
never reaching a terminal state. The budget now starts once the send returns.

Retention's unattended sweep deleted interrupted and failed imports along with
the payload that makes them resumable. The guard added in v0.56.0 was applied
to the manual cleanup path only, while the path that runs every tick kept
deleting them - and the admin dry-run used the guarded query, so it reported
nothing to delete while the scheduler deleted it.

Pausing a follow, which began deferring rather than discarding notifications in
v0.56.0, interacted badly with two things. The administrator's clock-skew repair
requeued every deferred delivery, releasing every member's paused notifications
at once while the interface still showed the pause; and a deferred delivery on
its own marked the instance degraded, which is what surfaced that button. A
paused follow also ranked identically to a disabled one when choosing which
follow rule governs a release, so a disabled follow could win and the deferral
was dropped while the event was still recorded and could never be re-queued. A
follow that will deliver at pause expiry now also takes the Trust Guard hold it
previously skipped.

MusicBrainz-only releases were absent from the release calendar, the ICS
export, the calendar feed and the digest for any artist who also had a Spotify
or Apple release, while still generating an announcement. The provider
preference now suppresses only an unmerged duplicate of the same release rather
than every catalogue-only release by that artist, and the calendar and upcoming
lists share one definition of it.

Finally, login throttling behind a reverse proxy: with TRUST_PROXY unset every
request appears to come from the proxy, so members share one failure counter per
account and anyone who knows an address can lock its owner out. That was
documented as a refinement rather than a requirement, and the v0.55.0 note
claimed the lockout was impossible without saying it depends on this. The
requirement is now stated plainly, the claim is qualified, and the application
logs a warning the first time it sees a forwarded header without TRUST_PROXY.

The v0.57.1 patch fixes two ways a release or an alert could be lost silently.

"Notify anyway" on a held release marked the hold released whenever a
notification event existed. That event row is written before the member's own
delivery rules are consulted, so for a paused or disabled follow the hold
disappeared while nothing was delivered - and because an event is unique per
member, release, and type, it could never be raised again. The override now asks
whether the event was actually admitted for delivery, and leaves the hold
pending when it was not so the owner can change the rule and retry. The same
test was wrong on the drain path and is corrected there too. A digest-only
follow still counts as admitted, because the digest collects it.

Release titles made entirely of symbols - "+", "=", "-", "÷" are all real names
- normalized to the empty string, so every one of them compared equal to every
other and the identity match fell through to the release date alone. Two such
releases by one artist in the same year were folded into a single release group,
permanently losing one along with its provider observations, calendar entry and
notification. Symbol titles now normalize to themselves, so they stay
distinguishable from each other while still matching the same title from another
provider.

The v0.57.2 patch revokes the calendar feed token when a member changes or
resets their password. The token is a year-long bearer credential in a plain
URL, usable without a session, and it exposes the member's upcoming releases;
until now it outlived every credential change, so a member acting on a lost
device or a leaked feed URL had no way to end access short of finding a separate
Settings action. Generate a new feed URL after changing a password if you use
one.

The same patch gives the release digest its own share of each delivery batch.
The digest previously received whatever the normal delivery queue left over, so
a saturated queue - several members at the per-member claim cap, a release-day
burst, or a backlog re-armed after an outage - left nothing and digest
deliveries were never attempted, while the pending digest run stopped a
replacement from being created.

An artist whose MusicBrainz identity check exhausted its retry budget is marked
unresolvable and excluded from scheduled synchronization. The refusal told the
member to run an explicit sync, but that path did not clear the state, so the
artist was stranded: never synchronized, absent from the backlog figure, and
unreachable from the interface. An explicit sync now clears it, which is what
the message always promised.

The v0.59.0 release starts on the fifth review's backlog, and rebuilds the part
of the notification engine that backlog kept pointing at.

Five separate defects all came from one place. Three different pieces of code
decided which of your follows governs a given alert, and only one of them
actually chose: the other two matched *any* follow connected to the release.
Pausing one artist could therefore move an alert that a different, still-active
follow was in charge of; extending a pause left its alerts due at the old,
earlier time, so they arrived while the follow still read "Paused until" a later
date; and an alert held back by a pause, on a destination that then failed
often enough to be switched off, had no automatic way back once the destination
recovered - you had to know to press "Retry failed deliveries" a second time
after the pause expired, which nothing told you. Removing an artist from your
watchlist left its already-queued alerts in place, so a notification could still
arrive months later for an artist you had removed.

There is now one definition of "which follow governs this alert", and the code
that admits a notification calls the same function as everything that reschedules
one. Recovering a destination now reports separately how many alerts it made
runnable and how many it unblocked but is holding until a pause ends, instead of
saying "0 queued for retry" beside a destination still showing failures.

Elsewhere: pausing an artist and letting the pause lapse no longer leaves the
Artists page insisting it is still paused with the Pause button replaced by
"Resume now"; that page also shows the pause expiry in your own timezone. Arriving
from a dashboard tile, a webmail tab, or one of this application's own calendar
links no longer silently invalidates open forms and loses what you had typed.

For operators: a `TRUST_PROXY` misconfiguration, application-log loss, and SQLite
write contention are all reported rather than silent; provider failure messages
survive a tick where the provider was deliberately not contacted, instead of
being erased; startup security warnings now reach the stored log rather than only
stdout; and three diagnostic fields that exist purely as operator signal -
including the invite-link fingerprint added so a failing link can be traced -
are no longer destroyed by the log redaction.

Twenty-seven findings from that review remain open and will follow.

The v0.58.0 release closes the fourth review's backlog: twenty-five findings,
each proven by reintroducing the defect and confirming the new test fails.

Three notification defects shared one cause. A pause set on an artist who is
merely credited on a release - not its canonical artist - was not recognised as
a pause at all, so the deferred delivery looked like a clock-skewed one and was
forced out to a member who had explicitly asked for quiet. Lifting a pause early
left everything it had deferred sitting at its original future time, so silence
continued until the pause would have expired anyway. Both came from a
correlation rule that existed as three hand-written copies which had already
drifted; there is now one definition, and all three sites use it.

Shutdown no longer turns delivered notifications into failures. A send that the
transport had accepted, but which had not yet reported back when the process
began stopping, was recorded as a durable failure - so it was retried, and the
member received it twice. Sends now get a brief grace period to report, and a
worker cancelled before it starts is skipped rather than counted as a failure.

A slow destination no longer degrades a healthy one. Every HTTP notification
serialises through one process-wide gate, and the per-send timeout used to start
when a send entered the queue rather than when it reached the transport. With
the gate held for a full send timeout, the next send in line arrived with no
budget left, returned a deadline error without issuing a request, and its
destination was backed off for a failure that never happened.

Several things the application already knew are now actually shown. The delivery
log on your dashboard displays why a notification failed, how many attempts it
took, and when a sent one went out - all of it fetched and redacted already, and
previously discarded in favour of a bare red badge. The release-truth panel has
the reason field its stored decision always had. Recent imports are listed
beside the import form, so an import interrupted by a restart can be resumed
instead of being unreachable. Artist actions return you to the page and filters
you were working on. The calendar feed URL, which is shown exactly once, has a
copy button. Timestamps on the artists page follow your timezone like every
other page.

Operators gain the four signals the product was missing. A `TRUST_PROXY` setting
whose CIDRs do not match the proxy actually in front of the application now
reports itself, instead of silently collapsing login throttling exactly as an
unset `TRUST_PROXY` does. Application-log drops and write failures are visible
while the process runs, rather than only on a clean shutdown that an incident
never reaches. SQLite write retries, retry exhaustion and connection-pool waits
are counted and reported, so a hanging interface can be told apart from a slow
provider. And a restore rehearsal can record its result where the application
reads it, with `RESTORE_RECORD_RESULT=true`, so `Last restore rehearsal` stops
reading `not recorded` forever.

Underneath, the digest scheduler no longer rebuilds and discards every member's
entire digest on each sixty-second tick once that period's digest is settled;
the two duplicated delivery state machines are one; a panic in a request handler
is logged through the application's own logger with its route rather than a path
that can carry a credential; and the notification URL builder can no longer
produce a transport that the transport policy rejects.

The v0.57.3 patch finishes the third review's backlog.

A shared artwork fetch ran on the first requester's connection, so one browser
navigating away handed every other waiter a placeholder image - which their
browser then cached - even though the artwork service had answered. The artists
grid requests many covers at once, so a single reload during loading could spoil
the rest of the page. The shared fetch is now independent of any one requester
and stays bounded by its own deadline.

A large CSV import could outlive the request timeout, because every row is its
own database write. The job was then left in a state that is not resumable, with
the failed rows unrecorded because the write that would have recorded them
reused the same expired request. An import that runs out of time now stops
cleanly, records what happened, and is offered for resume straight away.

Finally, release merging tolerates a few days of disagreement between regional
storefronts, while evidence review demanded identical dates - so the store
merged two provider records and review immediately raised a conflict on the
result, creating a permanent review item, and a withheld notification for
members who hold conflicting releases. Both now use the same tolerance.

The **Release inbox** keeps one owner-scoped entry for each alertable release.
It shows the latest announcement or release-day event, provider confidence,
observation history, and source links even when a notification destination was
offline. Members can mark entries read, snooze them for one or seven days, or
dismiss and restore them. Historical releases silently baselined during an
initial sync do not appear, and inbox state never changes notification
delivery or provider polling.

The **Release Truth Desk** highlights disagreements between the latest Spotify,
Apple/iTunes, and MusicBrainz observations for a release, as well as releases
that have multiple fallback observations without canonical confirmation. Open
issues are visible from the Trust Center, dashboard, and release details. Each
household member can confirm, snooze, dismiss, or restore an issue privately;
review actions never change canonical release metadata, notifications, or
provider polling. Evidence is normalized to provider, title, type, date, and
link fields; raw provider payloads and credentials are never stored. Issues are
created or refreshed during normal synchronization, so existing records appear
after their next provider check. Release details also expose a reversible Truth
Loop decision: members can explicitly confirm the provider that best represents
a release for their household without rewriting provider observations.

A Truth Loop decision is **shared across the household**, unlike the evidence
review actions above, which are private to each member. There is one decision
per release, and the release page shows whether it was recorded by you or by
another member so a shared choice is never mistaken for your own. Only the
member who recorded a decision, or a household administrator, can change or
clear it; anyone else is told to ask them. A release with no decision yet can be
confirmed by any member who follows it.

The optional **Release Trust Guard** builds on the Truth Desk. Enable “Hold
notifications when provider evidence conflicts” under Settings to keep alerts
with warning or critical date, title, or type disagreements out of the delivery
queue. Held alerts appear on the dashboard and release details, where a member
can confirm a provider, notify anyway, or discard the alert. The default remains
immediate delivery, and informational gaps such as a missing canonical
observation do not block notifications.

Release details include a **Release Assurance Timeline**. This owner-scoped,
redacted view explains when each provider observed a release, which primary,
featured, or guest credits were recorded, how evidence reviews and household
truth decisions changed confidence, and whether notifications were held,
queued, sent, or failed. It is derived from existing observations and audit
projections; provider payloads, credentials, and notification bodies are never
shown or stored in the timeline. The inbox links directly to this explanation
for each alert.

The scheduler checks due synchronization and release-day work once per minute,
delivers notifications every ten seconds, and runs transient-state maintenance
hourly. Hourly maintenance also bounds the artwork cache to 1 GiB or 25,000
files, removing stale and oldest entries first. Notification delivery is
scheduled through a bounded four-worker queue, while the Shoutrrr 0.8
compatibility adapter serializes only transports that still require its
process-global client and restores the process default after each operation;
Telegram uses the sender-owned bounded Bot API client directly. SQLite keeps
one serialized writer and a small read-only pool so dashboard queries do not
queue behind provider work.

The four delivery workers bound queue management and durable state updates, but
production sends that use Shoutrrr's compatibility adapter are serialized by
its process-global client lock. Measure the effective transport wait and lock
hold time with `make benchmark-notify`; the benchmark reports
`queue-wait-ns/op` and `client-mutex-ns/op` without contacting an external
provider. The Telegram adapter uses the sender-owned client directly and is
not subject to that compatibility lock.

Delivery assurance records every normal and digest attempt, keeps a durable
health state for each destination, and pauses destinations after five
consecutive failures. Settings shows the latest failure and provides an
owner-scoped retry action; administrators can see household-wide destination
health and pending/failed queue counts. HTTP notification transports use a
bounded timeout and re-check every redirect against the outbound-target safety
policy. Message bodies and encrypted destination URLs are never stored in the
health projection.

Each followed artist can also have an owner-scoped notification rule. Use the
artist list to keep an artist on the account defaults, deliver only through
the immediate queue, include them only in the configured digest, or turn their
notifications off while retaining the release in the inbox. Rules can narrow
alerts to primary or featured credits and to albums, EPs, singles, or
compilations, and a follow can be paused for seven days. Existing account-wide
notification preferences remain the defaults for follows using **Account
defaults**; provider polling and release history are never changed by a rule.
The artist page also supports applying a delivery mode to up to 50 selected
follows at once.

When `PUBLIC_URL` uses HTTPS, the application sends HSTS and a restrictive
Permissions-Policy header. With `LOG_LEVEL=debug`, sanitized request-completion
records include the request ID, route pattern, status, duration, and response
size; request paths, query strings, bodies, credentials, and destination URLs
are never logged.

## Container images

GitHub Actions builds and publishes the Docker image to
`ghcr.io/crypt0rr/artist-trackarr` for `linux/amd64` and `linux/arm64`.

- `latest` and `main` follow the current `main` branch.
- `sha-<commit>` identifies an exact source revision.
- Pushing a tag such as `v0.59.0` publishes `0.59.0`, `0.59`, and `latest`.

The application version is kept in `internal/version/version.go` and is bumped
with each release. Local, branch, and release images show that same semantic
version in the interface and User-Agent. Build provenance remains available
through image labels and immutable SHA tags.

The module targets Go 1.27; CI and the Docker build use the pinned Go 1.27.0
toolchain so local builds and release images share the same supported
language/runtime line.

`make lint` runs the pinned `golangci-lint` v2.13.0 configuration. The focused
quality gate covers unchecked errors, Go vet, static analysis, ineffective
assignments, unused code, row and SQL resource handling, HTTP body closure,
wrapped errors, and nil-error paths. `make test` builds the fast Docker test
stage; `make docker-quality` builds the full Docker quality stage, which runs
serialized tests plus the pinned lint and vulnerability checks. Both Docker
stages serialize package tests (`-p 1`) so constrained Docker/CI filesystems
do not report spurious SQLite disk-full failures; the quality stage additionally
runs the race detector and pinned lint/vulnerability tools.

Pin a deployment to a release by setting the Compose image before starting:

```console
ARTIST_TRACKARR_IMAGE=ghcr.io/crypt0rr/artist-trackarr:0.59.0 docker compose up -d
```

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `PUBLIC_URL` | yes | `http://localhost:8080` | External base URL; use HTTPS behind a reverse proxy. |
| `ARTIST_TRACKARR_BIND` | no | `127.0.0.1` | Host address for the Compose port; expose wider only behind a trusted TLS proxy/firewall. |
| `SETUP_TOKEN` | yes | — | Protects initial administrator creation and is validated at every startup; must be at least 32 characters. Keep it available if the database ever needs to be initialized again. |
| `APP_ENCRYPTION_KEY` | yes | — | Encrypts notification credentials at rest. |
| `SESSION_SECRET` | yes | — | Adds server-side protection to session cookies. |
| `MUSICBRAINZ_CONTACT` | yes | — | Single-line contact included in the required MusicBrainz User-Agent; values over 200 characters are rejected. |
| `POLL_INTERVAL` | no | `6h` | Catalog polling interval; values below one hour are rejected. |
| `SPOTIFY_POLL_INTERVAL` | no | `24h` | Independent Spotify observation interval; values below one hour are rejected. |
| `SPOTIFY_CLIENT_ID` | no | — | Enables Spotify-first artist discovery. |
| `SPOTIFY_CLIENT_SECRET` or `SPOTIFY_CLIENT_SECRET_FILE` | no | — | Spotify application secret; Compose mounts the optional value as a Docker secret when configured. |
| `SPOTIFY_MARKET` | no | `US` | Two-letter market used when retrieving Spotify releases. |
| `ITUNES_MARKET` | no | `US` | Two-letter Apple/iTunes storefront used for fallback searches and release lookups. |
| `DATABASE_PATH` | no | `/data/artist-tracker.db` | SQLite database location; startup rejects directory paths and missing parent directories. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |
| `TRUST_PROXY` | no | `false` | Strict boolean (`true`/`false`); trust `X-Forwarded-For` only when the connecting proxy matches `TRUSTED_PROXY_CIDRS`. |
| `TRUSTED_PROXY_CIDRS` | no | — | Comma-separated proxy networks, for example `127.0.0.1/32,10.0.0.0/8`; required when `TRUST_PROXY=true`. |
| `ALLOW_INSECURE_HTTP` | no | `false` | Strict boolean (`true`/`false`); explicitly permits a non-local HTTP `PUBLIC_URL`. |
| `ALLOW_PRIVATE_NOTIFICATION_TARGETS` | no | `false` | Strict boolean (`true`/`false`); explicitly permits destinations resolving to private networks. |
| `LOG_LEVEL` | no | `info` | JSON log threshold: `debug`, `info`, `warn`, or `error`. |
| `TZ` | no | `UTC` | Container/system timezone for runtime logs and local process time, e.g. `Europe/Amsterdam`. |

Every secret also supports Docker's `*_FILE` convention, for example
`APP_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key`.

## Spotify, Apple/iTunes, and MusicBrainz discovery and release observation

Create an application in the [Spotify developer dashboard](https://developer.spotify.com/dashboard),
then set `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET`. When configured,
Spotify supplies the preferred artist search, images, links, and release observation feed. When Spotify is unavailable or returns no results, the application falls back to the public Apple iTunes Search API and then MusicBrainz. MusicBrainz remains the stable artist identity source. Spotify- and iTunes-only releases are stored under their stable provider identity and can generate notifications immediately; they are promoted to a MusicBrainz release group later when a conservative title, type, and date match is found.

Set `SPOTIFY_MARKET` to the country whose catalogue should be checked, for
example `NL`. Existing followed artists are silently baselined the first time
Spotify release polling runs after an upgrade, preventing back-catalogue
notification floods. New releases observed after that baseline can notify
independently of MusicBrainz. Albums, EPs, singles, and compilations are all
eligible release types; two through six track Spotify releases are treated as
EPs. Spotify also requests the `appears_on` relationship, so a followed artist
is notified when they are featured on another artist's album, EP, single, or
compilation. Existing follows receive a one-time appearance baseline during
their first successful post-upgrade Spotify sync; new followers retain the
normal single-release onboarding notification. Featured alerts represent the
containing release rather than individual tracks.

ArtistTrackarr also keeps a source-agnostic credit graph for followed artists.
Spotify's `appears_on` results, iTunes multi-artist song credits, and
MusicBrainz recording artist credits are normalized into primary, featured, and
guest evidence for the containing release. A guest credit includes its track
context where the provider supplies one, while notifications remain one
release-level event and continue to use the existing deduplication and
baseline rules. Existing follows are baselined once per provider and credit
role so an upgrade cannot flood the inbox with historical collaborations;
newly observed future or recent guest releases are eligible for the normal
announcement and release-day reminders. Follow rules that include featured
appearances also include guest credits.

To keep Spotify Development Mode usage low, release observation normally reads
the newest Spotify artist-albums page and only walks older pages when the stored
Spotify release history has not yet been reached. Known release IDs, dates, and
provider observations are retained locally, and successful release responses
are cached for 24 hours. Artist searches are cached briefly (with identical
in-flight searches coalesced), and artist metadata is cached for 24 hours; this
also means selecting an artist directly from a recent search does not trigger a
second lookup request. Batch follow actions use Spotify's multiple-artist
endpoint when available. Artists are assigned stable polling offsets so a large
watch list is spread across the day instead of queried in one burst.
Apple/iTunes release observations are best-effort and are matched by canonical artist name. Spotify and Apple/iTunes use the same release-type heuristic: an explicit standalone “Single” or “EP” title wins, followed by one track as Single, two through six tracks as EP, and seven or more tracks as Album; compilations remain Albums. Word-boundary matching avoids treating titles such as “episode”, “epic”, or “epilogue” as EPs. When provider-derived types disagree, a unique normalized title/date match is preferred over creating a duplicate. Apple artwork URLs are accepted only from Apple hosts, loaded directly with attribution, and never downloaded or retained as image bytes. Existing artwork gaps are backfilled one artist at a time using the same conservative limiter. MusicBrainz release polling remains the final fallback and does not override successful Spotify or iTunes observations.

iTunes requests are serialized to approximately one request every three seconds and successful responses are cached. The storefront follows `ITUNES_MARKET` (default `US`) independently of Spotify, and no Apple credentials are required. The [iTunes Search API](https://developer.apple.com/library/archive/documentation/AudioVideo/Conceptual/iTuneSearchAPI/Searching.html) recommends keeping usage around 20 requests per minute, so iTunes remains a conservative fallback rather than a high-volume source.

Successful Spotify checks also adapt per artist. A catalog change returns the
artist to the configured `SPOTIFY_POLL_INTERVAL`; unchanged artists back off
progressively up to seven days, while artists with upcoming releases stay on the
baseline interval. The backoff state is stored in SQLite and survives restarts.
Spotify rate-limit cooldowns are stored at provider level as well, so a quota
response suppresses background and search requests until the safe retry time
even if the container is restarted.

Selections that cannot be identified while MusicBrainz is unavailable remain
pending and retry automatically.

Spotify Development Mode currently requires the application owner to have an
active Premium subscription and limits new applications to five authorized
users. No Spotify user login is required for this application's
client-credentials search and release-observation flow.

## Artist management

The **Artists** page combines individual search, multi-select following,
watchlist export, and ArtistTrackarr CSV import. An export can be uploaded
unchanged for a round trip: the six exported columns are required (in any
order), while unknown future columns are ignored. Imports are limited to 1 MiB
and 500 data rows, validate canonical MusicBrainz and optional Spotify
identities locally, and process rows independently. Added rows are followed
and scheduled for the normal baseline sync; invalid rows remain visible in the
owner-only import results page and do not prevent valid rows from being
applied. Exports use stable keyset pages and are fully assembled before the
download begins so watchlist changes or lookup failures cannot produce a
misleading truncated file. At most two uploads are processed concurrently and manual sync work
is admitted through a bounded queue, so large imports cannot starve scheduled
work or the SQLite writer. Provider calls are never made during the upload
request.

The account menu shows each member's unique username and links to personal
Settings, where they can update their username, password, timezone, release-day
reminder time, notification preferences, and all notification destinations. The old
`/destinations` address redirects to Settings for compatibility, while
household account administration remains restricted to the Admin page.

Usernames are case-insensitive, 3–32 characters, and may contain letters,
numbers, dots, underscores, and hyphens. Existing accounts receive a
deterministic username during the v0.16.0 migration and can change it later.

Public [ListenBrainz popularity](https://listenbrainz.readthedocs.io/en/latest/users/api/popularity.html) is refreshed once per day for followed canonical
artists. Artist pages show aggregate listen and listener counts when available;
these statistics are informational only and never create release observations
or notifications. The dashboard and Artists page also provide compact
breakdowns by genre, country, and artist type with owner-scoped drill-down
filters. Genres come from MusicBrainz tags and are normalized locally.

## Notification destinations

Users can add Discord, Telegram, ntfy, and generic HTTP(S) webhook
destinations. Advanced Shoutrrr URLs are limited to those same audited
transports; SMTP, Gotify, and unknown schemes are rejected because the
application cannot apply its connection-time SSRF policy to them. Existing
legacy destinations remain visible as **Unsupported**, are never contacted,
and must be replaced explicitly. Credentials are encrypted in SQLite and
redacted from the UI and logs. Use the **Send test** action after adding a
destination.

Delivery is at-least-once. A process crash after an external provider accepts a
message can result in a duplicate, but durable claims and recovery avoid
silently losing queued work. Paused or unsupported destinations receive a
blocked queue row instead of disappearing from an event; an administrator or
owner can retry after replacing/recovering the destination. A newly added
destination receives future events only and is not backfilled with historical
notifications.

### Retention and cleanup

The administrator page includes a retention dry-run with the effective policy
before any cleanup is possible. Application logs are kept for seven days;
expired sessions and tokens, old login-attempt records, completed manual-sync
requests, and import jobs are transient operational state and use a 30-day
window (login attempts use a 24-hour safety window). Cleanup is never run from
the web request automatically: an administrator must explicitly confirm it.
Notification events, deliveries, inbox state, blocked deliveries, and
delivery-attempt audit records have no automatic expiry and are not removed by
this action while the account exists (account deletion still removes that
account's private data). Backups should therefore be treated as confidential and retained
according to the household's own recovery policy.
Explicit cleanup checkpoints the SQLite WAL when possible and reports if a
reader delayed truncation. It does not run `VACUUM` or promise a smaller
database file: deleted pages are reported separately as reusable freelist
space. Run `VACUUM` only during a planned maintenance window with a recent
backup and enough temporary disk space.

The administrator page marks a retention review when the oldest notification or
delivery history reaches 365 days. This is a review recommendation only: no
user-facing history is deleted automatically, and any future cleanup policy
must be approved against the household's recovery and audit requirements first.
Administrators can download the delivery-audit CSV from the same page to support
an export-before-delete workflow. The export neutralizes formula-leading cells,
uses stable keyset pages while it is generated, and is assembled before the
response starts so a lookup failure cannot masquerade as a truncated 200 file.
It never includes encrypted destination URLs.

Users can choose whether albums, EPs, singles, announcements, and release-day
reminders should be delivered. Followed artists show their last and next
synchronization times, and **Sync now** queues a rate-limited refresh. Release
details expose the stored provider observations and source history.

Expired sessions and authentication tokens are removed during periodic state
maintenance. Login-attempt records older than 24 hours and completed or failed
manual sync requests older than 30 days are removed. Import jobs older than 30
days (including their rows) are removed; notification and delivery history is
retained, application logs keep their existing seven-day window, and recent
queued work is not deleted.

## Household administration

Administrators can review every household account, its role, reminder settings,
follow count, and notification-destination count. They can permanently delete
another user and all of that user's private data. Administrators cannot delete
their own account or leave the household without an administrator.

## Reverse proxy and backups

Terminate TLS at Caddy, Traefik, nginx, or another reverse proxy and set
`PUBLIC_URL` to its HTTPS address. Non-local HTTP is rejected unless
`ALLOW_INSECURE_HTTP=true` is explicitly set.

**Set `TRUST_PROXY=true` together with the proxy's exact `TRUSTED_PROXY_CIDRS`.**
This is not a refinement. Without it every request appears to originate from the
proxy, so login throttling cannot tell members apart: all of them share one
failure counter per account, anyone who knows an address can lock its owner out
by failing five logins, and the per-address request limiter becomes a single
household-wide cap on signing in. ArtistTrackarr logs a warning on the first
login request that carries `X-Forwarded-For` while `TRUST_PROXY` is unset.
Forwarding headers from an untrusted connection are still ignored.

Notification destinations are server-side outbound requests. By default,
ArtistTrackarr blocks loopback, private, link-local, multicast, and metadata
network addresses to prevent an invited user from using notifications as an
SSRF proxy. Set `ALLOW_PRIVATE_NOTIFICATION_TARGETS=true` only when all
household members are trusted and local notification services are required.

For a consistent backup, use the repository helper. It stops the app, resolves
the volume actually mounted at `/data`, refuses missing or empty databases, and
always attempts to restart the service. The archive contains the complete
persistent data directory and is accompanied by a restrictive-permission
`.sha256` sidecar. Keep the archive and sidecar together:

```console
./scripts/backup.sh artist-trackarr-backup.tgz
```

Successful backups write a non-sensitive timestamp marker into the persistent
volume so administrator diagnostics can show an approximate backup age. The
marker is archived with the next backup and is not a substitute for an
off-host backup inventory. The archive and checksum are streamed through the
invoking shell, so they keep the operator's UID/GID even with rootless Docker;
the helper never needs to chown a host-side file. The marker is written as the
application user (UID 10001), so the operator only needs Docker access and
write permission to the output directory.

Restore into an empty Compose volume while the app is stopped, keep the
original `APP_ENCRYPTION_KEY` available, and run the temporary restore
rehearsal before replacing production data. The key is required to decrypt
existing notification destinations. Embedded migrations run automatically
during upgrades; the rehearsal must pass SQLite foreign-key checks and
`/readyz` before the restored instance is considered usable.

The rehearsal requires an immutable image digest (`@sha256:`), verifies the
checksum sidecar, runs SQLite `integrity_check` and `foreign_key_check`,
validates that encrypted destinations can be opened with the original key,
fingerprints the durable logical database state, and compares that fingerprint
after a clean restart. A mismatch fails the rehearsal rather than declaring the
restore usable. Legacy archives without a sidecar or mutable images are
accepted only when explicitly opted in with `RESTORE_ALLOW_LEGACY_ARCHIVE=true`
or `RESTORE_ALLOW_MUTABLE_IMAGE=true`; new backups should always use the
immutable path. Backup archives and encryption keys are confidential operator
artifacts.

The rehearsal uses an isolated Docker volume, starts the selected image,
stops it with the configured grace period, and starts it again to verify that
the restored data remains usable:

```console
APP_ENCRYPTION_KEY="$APP_ENCRYPTION_KEY" \
  ARTIST_TRACKARR_IMAGE=ghcr.io/crypt0rr/artist-trackarr@sha256:<release-digest> \
  ./scripts/restore-smoke.sh artist-trackarr-backup.tgz
```

The rehearsal records a non-sensitive restore result marker in its temporary
volume; that volume is removed when the rehearsal exits, so by default nothing
about the rehearsal reaches the running instance and `Last restore rehearsal`
keeps reading `not recorded`.

To have the running instance report the rehearsal, set `RESTORE_RECORD_RESULT=true`.
The script then also stamps the marker on the live `/data` volume, resolved from
the Compose service (`COMPOSE_SERVICE`, default `app`) exactly as `scripts/backup.sh`
resolves it, writing as UID 10001 through an atomic rename. The timestamp and
result then appear as `Last restore rehearsal` on the administration page, in the
support report, in `/admin/diagnostics.json`, and in the hourly operational
snapshots. It is opt-in because the rehearsal is otherwise fully isolated from
production:

```console
APP_ENCRYPTION_KEY="$APP_ENCRYPTION_KEY" \
  ARTIST_TRACKARR_IMAGE=ghcr.io/crypt0rr/artist-trackarr@sha256:<release-digest> \
  RESTORE_RECORD_RESULT=true \
  ./scripts/restore-smoke.sh artist-trackarr-backup.tgz
```

## Development

The test suite runs in the pinned Go toolchain from the build image:

```console
docker build --target test .
```

To measure statement coverage for the internal packages locally:

```console
make coverage
```

The command writes the temporary `coverage.out` profile (ignored by Git) and
prints the combined percentage from `go tool cover`. It enforces an 80% minimum
by default; use `make coverage COVERAGE_MIN=85` to test a stricter local target.

To build and run the current checkout instead of the published image:

```console
docker build -t artist-trackarr:local .
ARTIST_TRACKARR_IMAGE=artist-trackarr:local docker compose up -d
```

The CI-equivalent lifecycle rehearsal can be run against a locally built image.
It creates a temporary volume, completes setup and login, verifies a persisted
follow and manual sync, checks readiness, and confirms clean shutdown/restart:

```console
docker build -t artist-trackarr:ci .
./scripts/container-smoke.sh artist-trackarr:ci
```

No Node.js toolchain or external asset CDN is required.

## License

- Licensed under the [MIT License](LICENSE).
