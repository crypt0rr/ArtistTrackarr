#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# This driver is for CI and disposable local runs only. It creates a Compose
# project and volume owned by this process, then invokes the same backup and
# restore helpers operators use. It never points either helper at a production
# project or volume.
HELPER_IMAGE="${DR_HELPER_IMAGE:-alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b}"
IMAGE="${ARTIST_TRACKARR_IMAGE:-artist-trackarr:disaster-recovery}"
suffix="${CI_RUN_ID:-$$}"
project="${DR_COMPOSE_PROJECT_NAME:-artist-trackarr-dr-${suffix}}"
port="${DR_COMPOSE_PORT:-8080}"
restore_prefix="artist-trackarr-dr-restore-${suffix}"
base="http://127.0.0.1:${port}"
jar=$(mktemp)
archive=$(mktemp "${TMPDIR:-/tmp}/artist-trackarr-dr.XXXXXX.tgz")
signal_archive="${archive}.signal.tgz"
signal_marker="${signal_archive}.sha256"
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
export POLL_INTERVAL=1h
export SPOTIFY_POLL_INTERVAL=1h

cleanup() {
	status=$?
	trap - EXIT INT TERM HUP
	docker compose down --volumes --remove-orphans >/dev/null 2>&1 || true
	docker ps -aq --filter "name=^/${restore_prefix}-" | xargs -r docker rm -f >/dev/null 2>&1 || true
	docker volume ls -q --filter "name=^${restore_prefix}-" | xargs -r docker volume rm >/dev/null 2>&1 || true
	rm -f "$jar" "$archive" "$archive.sha256" "$signal_archive" "$signal_marker" "$legacy_archive"
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
grep -q 'CI encrypted destination' <<<"$settings_page"

# Exercise the backup trap on SIGHUP after the service has actually been
# stopped. The interrupted archive must not become an apparent backup, and the
# helper must restart the Compose service before returning.
COMPOSE_SERVICE=app BACKUP_HELPER_IMAGE="$HELPER_IMAGE" \
	./scripts/backup.sh "$signal_archive" &
backup_pid=$!
interrupt_when "$backup_pid" HUP backup_stopped
if wait "$backup_pid"; then
	echo 'disaster recovery: interrupted backup unexpectedly succeeded' >&2
	exit 1
fi
wait_ready
test ! -e "$signal_archive"
test ! -e "$signal_marker"
if compgen -G "${signal_archive}.*.tmp" >/dev/null; then
	echo 'disaster recovery: interrupted backup left temporary files' >&2
	exit 1
fi

BACKUP_HELPER_IMAGE="$HELPER_IMAGE" ./scripts/backup.sh "$archive"
test -s "$archive"
test -s "$archive.sha256"
test "$(stat -c '%a' "$archive")" = 600
test "$(stat -c '%a' "$archive.sha256")" = 600

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

echo 'disaster recovery: isolated backup, wrong-key rejection, restore persistence, and signal cleanup passed'
