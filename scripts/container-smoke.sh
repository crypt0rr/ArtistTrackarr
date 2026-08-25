#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Keep the smoke environment deterministic and use the same pinned helper as
# the backup/restore rehearsals. The helper is only used for the temporary
# SQLite fixture; no application data is copied out of the container.
HELPER_IMAGE="${SMOKE_HELPER_IMAGE:-alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b}"
IMAGE="${1:-artist-trackarr:ci}"
PORT="${SMOKE_PORT:-18080}"
EXPECTED_VERSION="${SMOKE_EXPECTED_VERSION:-}"
suffix="${CI_RUN_ID:-$$}"
volume="artist-trackarr-smoke-${suffix}"
container="artist-trackarr-smoke-${suffix}"
base="http://127.0.0.1:${PORT}"
jar=$(mktemp)
setup_token='ci-setup-token-123456789012345678901234567890'
encryption_key='ci-encryption-key-123456789012345678901234567890'
session_secret='ci-session-secret-123456789012345678901234567890'

cleanup() {
	rm -f "$jar"
	docker rm -f "$container" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
}
# HUP included: an untrapped fatal signal skips the EXIT trap in dash and busybox
# ash, which would leak the smoke-test container and its volume.
trap cleanup EXIT INT TERM HUP

docker volume create "$volume" >/dev/null
docker run -d --name "$container" --network host -v "$volume:/data" \
	-e LISTEN_ADDR=":$PORT" \
	-e PUBLIC_URL="$base" \
	-e ALLOW_INSECURE_HTTP=true \
	-e MUSICBRAINZ_CONTACT='ci@example.invalid' \
	-e SETUP_TOKEN="$setup_token" \
	-e APP_ENCRYPTION_KEY="$encryption_key" \
	-e SESSION_SECRET="$session_secret" \
	-e POLL_INTERVAL=1h -e SPOTIFY_POLL_INTERVAL=1h \
	"$IMAGE" >/dev/null

wait_ready() {
	attempt=1
	while [ "$attempt" -le 90 ]; do
		if curl --fail --silent "$base/readyz" >/dev/null; then
			return 0
		fi
		sleep 1
		attempt=$((attempt + 1))
	done
	docker logs "$container" >&2 || true
	echo 'container smoke: application did not become ready' >&2
	return 1
}

csrf_from() {
	sed -n 's/.*name="_csrf" value="\([^"]*\)".*/\1/p' | head -n 1
}

wait_ready
setup_page=$(curl --fail --silent --cookie-jar "$jar" "$base/setup")
setup_csrf=$(printf '%s' "$setup_page" | csrf_from)
test -n "$setup_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/setup" \
	--data-urlencode "_csrf=$setup_csrf" \
	--data-urlencode "setup_token=$setup_token" \
	--data-urlencode 'email=ci@example.invalid' \
	--data-urlencode 'username=ci-smoke' \
	--data-urlencode 'password=ci-smoke-password-long-enough-12345' \
	--data-urlencode 'timezone=UTC' >/dev/null

login_page=$(curl --fail --silent --cookie-jar "$jar" "$base/login")
if [[ -n "$EXPECTED_VERSION" ]]; then
	grep -q "v${EXPECTED_VERSION}" <<<"$login_page"
	if grep -qE 'vdev(-|</|<)' <<<"$login_page"; then
		echo "container smoke: development version leaked into the UI" >&2
		exit 1
	fi
fi
login_csrf=$(printf '%s' "$login_page" | csrf_from)
test -n "$login_csrf"
curl --fail --silent --show-error --cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/login" \
	--data-urlencode "_csrf=$login_csrf" \
	--data-urlencode 'email=ci@example.invalid' \
	--data-urlencode 'password=ci-smoke-password-long-enough-12345' >/dev/null

dashboard=$(curl --fail --silent --cookie "$jar" "$base/")
grep -q 'ArtistTrackarr' <<<"$dashboard"

# Stop the app before inserting a deterministic followed artist. This keeps
# the fixture transaction isolated from the application's SQLite writer.
docker stop --time 60 "$container" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
fixture_sql=$(cat <<'SQL'
INSERT OR IGNORE INTO artists (id, mbid, name, sort_name, artist_type, country, created_at, updated_at, next_check_at)
VALUES (1, '00000000-0000-4000-8000-000000000001', 'CI Smoke Artist', 'CI Smoke Artist', 'Group', 'US', datetime('now'), datetime('now'), datetime('now', '+1 day'));
INSERT OR IGNORE INTO follows (user_id, artist_id, created_at, baseline_synced_at)
VALUES (1, 1, datetime('now'), datetime('now'));
SQL
)
docker run --rm --env "FIXTURE_SQL=$fixture_sql" --volumes-from "$container" "$HELPER_IMAGE" \
	sh -ec 'apk add --no-cache sqlite >/dev/null; sqlite3 /data/artist-tracker.db "$FIXTURE_SQL"'
docker start "$container" >/dev/null
wait_ready

artists_page=$(curl --fail --silent --cookie "$jar" "$base/artists")
grep -q 'CI Smoke Artist' <<<"$artists_page"
artists_csrf=$(printf '%s' "$artists_page" | csrf_from)
test -n "$artists_csrf"
# Follow the redirect and read the flash message. syncArtist answers 303 on BOTH
# branches - queued and "could not be queued" - so `curl --fail` without -L
# treats a failed sync as a pass, and the closing line then claims manual sync
# passed regardless of what happened.
sync_result=$(curl --fail --silent --show-error --location \
	--cookie "$jar" --cookie-jar "$jar" \
	--request POST "$base/artists/1/sync" \
	--data-urlencode "_csrf=$artists_csrf")
if ! grep -q 'Synchronization queued' <<<"$sync_result"; then
	echo "container smoke: manual sync was not queued" >&2
	grep -o 'Synchronization[^<]*' <<<"$sync_result" >&2 || true
	exit 1
fi

# A second clean restart verifies persisted sessions and followed-artist state,
# not only unauthenticated readiness.
docker stop --time 60 "$container" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
docker start "$container" >/dev/null
wait_ready
artists_page=$(curl --fail --silent --cookie "$jar" "$base/artists")
grep -q 'CI Smoke Artist' <<<"$artists_page"
echo 'container smoke: setup, login, followed artist, manual sync, readiness, persistence, and graceful shutdown passed'
