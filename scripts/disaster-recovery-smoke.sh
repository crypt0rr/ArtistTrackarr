#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# This driver is for CI and disposable local runs only. It creates a Compose
# project and volume owned by this process, then invokes the same backup and
# restore helpers operators use. It never points either helper at a production
# project or volume.
HELPER_IMAGE="${DR_HELPER_IMAGE:-alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6}"
IMAGE="${ARTIST_TRACKARR_IMAGE:-artist-trackarr:disaster-recovery}"
suffix="${CI_RUN_ID:-$$}"
project="${DR_COMPOSE_PROJECT_NAME:-artist-trackarr-dr-${suffix}}"
port="${DR_COMPOSE_PORT:-8080}"
restore_prefix="artist-trackarr-dr-restore-${suffix}"
recorder_port="${DR_RECORDER_PORT:-$((20000 + ($$ % 20000)))}"
recorder_pid=''
base="http://127.0.0.1:${port}"
jar=$(mktemp)
recorder_root=$(mktemp -d "${TMPDIR:-/tmp}/artist-trackarr-dr-recorder.XXXXXX")
archive=$(mktemp "${TMPDIR:-/tmp}/artist-trackarr-dr.XXXXXX.tgz")
legacy_archive="${archive}.legacy.tgz"
setup_token="${DR_SETUP_TOKEN:-ci-dr-setup-token-123456789012345678901234567890}"
encryption_key="${APP_ENCRYPTION_KEY:-ci-dr-encryption-key-123456789012345678901234567890}"
session_secret="${SESSION_SECRET:-ci-dr-session-secret-123456789012345678901234567890}"
password='ci-dr-password-long-enough-12345'

export COMPOSE_PROJECT_NAME="$project"
export ARTIST_TRACKARR_IMAGE="$IMAGE"
export ARTIST_TRACKARR_BIND=127.0.0.1
export PUBLIC_URL="$base"
export MUSICBRAINZ_CONTACT='ci-disaster-recovery@example.invalid'
export SETUP_TOKEN="$setup_token"
export APP_ENCRYPTION_KEY="$encryption_key"
export SESSION_SECRET="$session_secret"
export SPOTIFY_CLIENT_ID=''
export SPOTIFY_CLIENT_SECRET=''
export ALLOW_PRIVATE_NOTIFICATION_TARGETS=true
export RESTORE_SMOKE_SETTLE_SECONDS=12
export POLL_INTERVAL=1h
export SPOTIFY_POLL_INTERVAL=1h

cleanup() {
	status=$?
	trap - EXIT INT TERM HUP
	if [ -n "$recorder_pid" ]; then
		kill "$recorder_pid" >/dev/null 2>&1 || true
		wait "$recorder_pid" 2>/dev/null || true
	fi
	docker compose down --volumes --remove-orphans >/dev/null 2>&1 || true
	docker ps -aq --filter "name=^/${restore_prefix}-" | xargs -r docker rm -f >/dev/null 2>&1 || true
	docker volume ls -q --filter "name=^${restore_prefix}-" | xargs -r docker volume rm >/dev/null 2>&1 || true
	rm -f "$jar" "$archive" "$archive.sha256" "$legacy_archive" \
		"${archive}.INT.tgz" "${archive}.INT.tgz.sha256" \
		"${archive}.TERM.tgz" "${archive}.TERM.tgz.sha256" \
		"${archive}.HUP.tgz" "${archive}.HUP.tgz.sha256" \
		"${archive}.stopped.tgz" "${archive}.stopped.tgz.sha256" \
		"${archive}.INT.tgz."*.tmp "${archive}.TERM.tgz."*.tmp "${archive}.HUP.tgz."*.tmp
	rm -rf "$recorder_root"
	exit "$status"
}
trap cleanup EXIT INT TERM HUP

csrf_from() {
	sed -n 's/.*name="_csrf" value="\([^"]*\)".*/\1/p' | head -n 1
}

wait_ready() {
	for _ in {1..90}; do
		if curl --fail --silent "$base/readyz" >/dev/null; then
			return 0
		fi
		container_id=$(docker compose ps -aq app 2>/dev/null || true)
		if [ -n "$container_id" ] && [ "$(docker inspect -f '{{.State.Running}}' "$container_id" 2>/dev/null || true)" != "true" ]; then
			docker logs "$container_id" >&2 || true
			echo 'disaster recovery: Compose app exited before readiness' >&2
			return 1
		fi
		sleep 1
	done
	docker compose logs app >&2 || true
	echo 'disaster recovery: Compose app did not become ready' >&2
	return 1
}

assert_no_restore_resources() {
	if docker ps -aq --filter "name=^/${restore_prefix}-" | grep -q .; then
		echo 'disaster recovery: restore containers leaked' >&2
		docker ps -a --filter "name=^/${restore_prefix}-" >&2 || true
		return 1
	fi
	if docker volume ls -q --filter "name=^${restore_prefix}-" | grep -q .; then
		echo 'disaster recovery: restore volumes leaked' >&2
		docker volume ls --filter "name=^${restore_prefix}-" >&2 || true
		return 1
	fi
}

backup_stopped() {
	container_id=$(docker compose ps -aq app 2>/dev/null || true)
	[ -n "$container_id" ] && [ "$(docker inspect -f '{{.State.Running}}' "$container_id" 2>/dev/null || true)" = "false" ]
}

restore_volume_created() {
	[ -n "$(docker volume ls -q --filter "name=^${restore_prefix}-signal-" 2>/dev/null || true)" ]
}

recorder_request_count() {
	if [ -s "$recorder_root/requests" ]; then
		wc -l < "$recorder_root/requests" | tr -d ' '
	else
		printf '0\n'
	fi
}

# Freeze the helper after it has reached the resource-specific operation, then
# deliver a real signal. This makes the existing signal traps deterministic
# without adding test-only sleeps or branches to the operator-facing helpers.
interrupt_when() {
	pid=$1
	signal=$2
	condition=$3
	for _ in {1..300}; do
		if ! kill -0 "$pid" 2>/dev/null; then
			return 1
		fi
		if "$condition"; then
			kill -STOP "$pid"
			kill "-$signal" "$pid" || true
			kill -CONT "$pid" || true
			return 0
		fi
		sleep 0.05
	done
	echo "disaster recovery: timed out waiting to interrupt process $pid" >&2
	return 1
}

cat > "$recorder_root/server.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import sys

port = int(sys.argv[1])
request_log = sys.argv[2]

class Handler(BaseHTTPRequestHandler):
	def do_GET(self):
		self.send_response(200)
		self.end_headers()

	def do_POST(self):
		length = int(self.headers.get("Content-Length", "0"))
		self.rfile.read(length)
		with open(request_log, "a", encoding="utf-8") as output:
			output.write(f"{self.command} {self.path}\n")
		self.send_response(200)
		self.send_header("Content-Length", "8")
		self.end_headers()
		self.wfile.write(b"accepted")

ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
python3 "$recorder_root/server.py" "$recorder_port" "$recorder_root/requests" >/dev/null 2>&1 &
recorder_pid=$!
for _ in {1..30}; do
	if curl --fail --silent "http://127.0.0.1:$recorder_port/healthz" >/dev/null; then
		break
	fi
	sleep 0.1
done
if ! kill -0 "$recorder_pid" 2>/dev/null; then
	echo 'disaster recovery: notification request recorder failed to start' >&2
	exit 1
fi

docker compose up -d app >/dev/null
wait_ready

setup_page=$(curl --fail --silent --cookie-jar "$jar" "$base/setup")
setup_csrf=$(printf '%s' "$setup_page" | csrf_from)
test -n "$setup_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/setup" \
	--data-urlencode "_csrf=$setup_csrf" \
	--data-urlencode "setup_token=$setup_token" \
	--data-urlencode 'email=ci-disaster-recovery@example.invalid' \
	--data-urlencode 'username=ci-disaster-recovery' \
	--data-urlencode "password=$password" \
	--data-urlencode 'timezone=UTC' >/dev/null

login_page=$(curl --fail --silent --cookie-jar "$jar" "$base/login")
login_csrf=$(printf '%s' "$login_page" | csrf_from)
test -n "$login_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/login" \
	--data-urlencode "_csrf=$login_csrf" \
	--data-urlencode 'email=ci-disaster-recovery@example.invalid' \
	--data-urlencode "password=$password" >/dev/null

settings_page=$(curl --fail --silent --cookie "$jar" "$base/settings")
settings_csrf=$(printf '%s' "$settings_page" | csrf_from)
test -n "$settings_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/destinations" \
	--data-urlencode "_csrf=$settings_csrf" \
	--data-urlencode 'name=CI encrypted destination' \
	--data-urlencode 'service=ntfy' \
	--data-urlencode 'host=ntfy.sh' \
	--data-urlencode 'topic=artisttrackarr-ci-disaster-recovery' >/dev/null
settings_page=$(curl --fail --silent --cookie "$jar" "$base/settings")
settings_csrf=$(printf '%s' "$settings_page" | csrf_from)
test -n "$settings_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/destinations" \
	--data-urlencode "_csrf=$settings_csrf" \
	--data-urlencode 'name=CI restore request recorder' \
	--data-urlencode 'service=generic' \
	--data-urlencode "target=http://127.0.0.1:$recorder_port/cgi-bin/notify" >/dev/null
settings_page=$(curl --fail --silent --cookie "$jar" "$base/settings")
grep -q 'CI encrypted destination' <<<"$settings_page"
grep -q 'CI restore request recorder' <<<"$settings_page"

# Seed one due notification delivery while the application is stopped. The
# immutable backup captures it pending; the running source app may later retry
# it, but the restore rehearsal below must never reach the loopback recorder.
docker compose stop app >/dev/null
fixture_sql=$(cat <<SQL
INSERT OR IGNORE INTO artists (id, mbid, name, sort_name, artist_type, country, created_at, updated_at, next_check_at)
VALUES (1, '00000000-0000-4000-8000-000000000001', 'CI Recovery Artist', 'CI Recovery Artist', 'Group', 'US', datetime('now'), datetime('now'), datetime('now', '+1 day'));
INSERT OR IGNORE INTO release_groups (id, mbid, artist_id, title, primary_type, first_release_date, musicbrainz_url, first_observed_at, updated_at)
VALUES (1, '00000000-0000-4000-8000-000000000002', 1, 'CI Recovery Release', 'Album', '2026-01-01', 'https://musicbrainz.org/release-group/00000000-0000-4000-8000-000000000002', datetime('now'), datetime('now'));
INSERT OR IGNORE INTO notification_events (id, user_id, release_group_id, event_type, title, body, created_at)
VALUES (1, 1, 1, 'announcement', 'CI recovery pending message', 'This due notification must not leave the isolated restore container.', datetime('now'));
INSERT OR IGNORE INTO deliveries (event_id, destination_id, status, attempts, next_attempt_at, last_error)
SELECT 1, id, 'pending', 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 minute'), ''
FROM destinations WHERE user_id=1 AND name='CI restore request recorder';
SQL
)
docker run --rm --env "FIXTURE_SQL=$fixture_sql" --volumes-from "$(docker compose ps -aq app)" "$HELPER_IMAGE" \
	sh -ec 'apk add --no-cache sqlite >/dev/null; sqlite3 /data/artist-tracker.db "$FIXTURE_SQL"'
docker compose start app >/dev/null
wait_ready

# Exercise every backup signal trap after the running service has actually
# stopped. The interrupted archive must not become an apparent backup, and the
# helper must restore the service's original running state before returning.
for signal in INT TERM HUP; do
	signal_archive="${archive}.${signal}.tgz"
	signal_marker="${signal_archive}.sha256"
	# Bash marks asynchronous jobs as ignoring SIGINT unless job control is
	# enabled at launch. Enable it briefly so the shell helper can install and
	# exercise its real INT trap in this non-interactive rehearsal.
	set -m
	COMPOSE_SERVICE=app BACKUP_HELPER_IMAGE="$HELPER_IMAGE" \
		./scripts/backup.sh "$signal_archive" &
	backup_pid=$!
	set +m
	interrupt_when "$backup_pid" "$signal" backup_stopped
	if wait "$backup_pid"; then
		echo "disaster recovery: backup interrupted by $signal unexpectedly succeeded" >&2
		exit 1
	fi
	wait_ready
	test ! -e "$signal_archive"
	test ! -e "$signal_marker"
	if compgen -G "${signal_archive}.*.tmp" >/dev/null; then
		echo "disaster recovery: backup interrupted by $signal left temporary files" >&2
		exit 1
	fi
done

# The interrupted-backup rehearsal restarted the source app, which may have
# attempted the seeded webhook. Re-arm its row and health state while stopped
# so the final archive always contains due, deliverable work.
docker compose stop app >/dev/null
reset_sql=$(cat <<'SQL'
UPDATE deliveries SET status='pending', attempts=0,
  next_attempt_at=strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 minute'), last_error=''
WHERE event_id=1 AND destination_id=(SELECT id FROM destinations WHERE name='CI restore request recorder');
INSERT OR REPLACE INTO destination_health
  (destination_id,status,consecutive_failures,last_success_at,last_failure_at,next_retry_at,last_error,updated_at)
SELECT id,'healthy',0,NULL,NULL,NULL,'',datetime('now')
FROM destinations WHERE name='CI restore request recorder';
SQL
)
docker run --rm --env "FIXTURE_SQL=$reset_sql" --volumes-from "$(docker compose ps -aq app)" "$HELPER_IMAGE" \
	sh -ec 'apk add --no-cache sqlite >/dev/null; sqlite3 /data/artist-tracker.db "$FIXTURE_SQL"'
BACKUP_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/backup.sh "$archive"
test -s "$archive"
test -s "$archive.sha256"
test "$(stat -c '%a' "$archive")" = 600
test "$(stat -c '%a' "$archive.sha256")" = 600
docker run --rm \
	--env "PENDING_QUERY=SELECT COUNT(*) FROM deliveries d JOIN destinations n ON n.id=d.destination_id WHERE d.status='pending' AND d.next_attempt_at<=strftime('%Y-%m-%dT%H:%M:%SZ','now') AND n.name='CI restore request recorder';" \
	-v "$archive:/backup/restore.tgz:ro" "$HELPER_IMAGE" sh -ec '
work=$(mktemp -d)
trap "rm -rf \"$work\"" EXIT
tar xzf /backup/restore.tgz -C "$work"
apk add --no-cache sqlite >/dev/null
pending=$(sqlite3 "$work/artist-tracker.db" "$PENDING_QUERY")
printf 'disaster recovery: archived due notification deliveries: %s\n' "$pending"
if [ "$pending" -lt 1 ]; then
	echo 'disaster recovery: backup did not retain a due pending notification delivery' >&2
	exit 1
fi
'
# Prevent retries by the original container from affecting the recorder count
# while the isolated restore rehearsal runs.
docker compose stop app >/dev/null
recorder_requests_before=$(recorder_request_count)

# A backup of a service stopped by the operator must stay stopped after both
# the archive and checksum have been written successfully.
stopped_archive="${archive}.stopped.tgz"
docker compose stop app >/dev/null
if ! backup_stopped; then
	echo 'disaster recovery: could not establish stopped-service fixture' >&2
	exit 1
fi
BACKUP_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/backup.sh "$stopped_archive"
test -s "$stopped_archive"
test -s "$stopped_archive.sha256"
if ! backup_stopped; then
	echo 'disaster recovery: backup restarted a service that was already stopped' >&2
	exit 1
fi

image_id=$(docker image inspect --format '{{.Id}}' "$IMAGE")
case "$image_id" in
	sha256:*) ;;
	*) echo 'disaster recovery: runtime image did not provide a content-addressed ID' >&2; exit 1 ;;
esac

# A backup without its sidecar must not be accepted by default. This check is
# intentionally done before the key test so checksum loss is independently
# visible and the restore script's disposable volume cleanup is exercised.
cp -- "$archive" "$legacy_archive"
if APP_ENCRYPTION_KEY="$encryption_key" ARTIST_TRACKARR_IMAGE="$image_id" \
	RESTORE_SMOKE_NAME_PREFIX="$restore_prefix" RESTORE_SMOKE_PORT=18081 \
	RESTORE_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/restore-smoke.sh "$legacy_archive"; then
	echo 'disaster recovery: restore unexpectedly accepted an archive without its checksum sidecar' >&2
	exit 1
fi
assert_no_restore_resources

# A destination encrypted under the original key must make a wrong-key restore
# fail closed. The restore helper now notices an exited container immediately.
if APP_ENCRYPTION_KEY='ci-dr-wrong-encryption-key-123456789012345678901234567890' \
	ARTIST_TRACKARR_IMAGE="$image_id" RESTORE_SMOKE_NAME_PREFIX="$restore_prefix" \
	RESTORE_SMOKE_PORT=18082 RESTORE_HELPER_IMAGE="$HELPER_IMAGE" \
	./scripts/restore-smoke.sh "$archive"; then
	echo 'disaster recovery: restore unexpectedly accepted the wrong encryption key' >&2
	exit 1
fi
assert_no_restore_resources

APP_ENCRYPTION_KEY="$encryption_key" ARTIST_TRACKARR_IMAGE="$image_id" \
	RESTORE_SMOKE_NAME_PREFIX="$restore_prefix" RESTORE_SMOKE_PORT=18083 \
	RESTORE_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/restore-smoke.sh "$archive"
assert_no_restore_resources
recorder_requests_after=$(recorder_request_count)
if [ "$recorder_requests_after" -ne "$recorder_requests_before" ]; then
	echo "disaster recovery: isolated restore sent $((recorder_requests_after - recorder_requests_before)) outbound notification request(s)" >&2
	cat "$recorder_root/requests" >&2 || true
	exit 1
fi

# Exercise the restore trap on SIGTERM as soon as its disposable volume exists.
# The unique prefix lets this assertion coexist with an operator's unrelated
# local rehearsal if the driver is run outside CI.
before_restore_volumes=$(docker volume ls -q --filter "name=^${restore_prefix}-" || true)
APP_ENCRYPTION_KEY="$encryption_key" ARTIST_TRACKARR_IMAGE="$image_id" \
	RESTORE_SMOKE_NAME_PREFIX="${restore_prefix}-signal" RESTORE_SMOKE_PORT=18084 \
	RESTORE_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/restore-smoke.sh "$archive" &
restore_pid=$!
interrupt_when "$restore_pid" TERM restore_volume_created
if wait "$restore_pid"; then
	echo 'disaster recovery: interrupted restore unexpectedly succeeded' >&2
	exit 1
fi
test "$(docker volume ls -q --filter "name=^${restore_prefix}-" || true)" = "$before_restore_volumes"
assert_no_restore_resources

echo 'disaster recovery: isolated backup, wrong-key rejection, outbound-free restore persistence, and signal cleanup passed'
