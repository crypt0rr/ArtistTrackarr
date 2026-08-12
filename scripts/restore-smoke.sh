#!/bin/sh
set -eu
umask 077
CDPATH=''

HELPER_IMAGE=${RESTORE_HELPER_IMAGE:-alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b}

# Rehearse restoring a backup into an isolated Docker volume. The original
# encryption key is intentionally required: encrypted notification targets
# cannot be recovered with a newly generated key.
archive=${1:?usage: $0 BACKUP.tgz}
if [ ! -s "$archive" ]; then
	echo "restore: backup archive is missing or empty" >&2
	exit 1
fi
if [ -z "${APP_ENCRYPTION_KEY:-}" ] || [ "${#APP_ENCRYPTION_KEY}" -lt 32 ]; then
	echo "restore: set the original APP_ENCRYPTION_KEY (at least 32 characters)" >&2
	exit 1
fi

image=${ARTIST_TRACKARR_IMAGE:-ghcr.io/crypt0rr/artist-trackarr:latest}
port=${RESTORE_SMOKE_PORT:-18080}
volume="artist-trackarr-restore-$$"
container="artist-trackarr-restore-$$"
archive_path=$(cd -- "$(dirname -- "$archive")" && pwd)/$(basename -- "$archive")
checksum_path="$archive_path.sha256"

cleanup() {
	docker rm -f "$container" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker volume create "$volume" >/dev/null
if [ -s "$checksum_path" ]; then
	(
		cd "$(dirname -- "$archive_path")"
		sha256sum -c "$(basename -- "$checksum_path")"
	)
else
	echo "restore: checksum sidecar is absent; accepting a legacy archive" >&2
fi
docker run --rm -v "$volume:/data" -v "$archive_path:/backup/restore.tgz:ro" "$HELPER_IMAGE" \
	sh -ec 'tar xzf /backup/restore.tgz -C /data; test -s /data/artist-tracker.db; apk add --no-cache sqlite >/dev/null; test -z "$(sqlite3 /data/artist-tracker.db "PRAGMA foreign_key_check;")"'

state_fingerprint() {
	docker run --rm -v "$volume:/data" "$HELPER_IMAGE" sh -ec '
		apk add --no-cache sqlite >/dev/null
		sqlite3 -noheader -separator "|" /data/artist-tracker.db \
			"SELECT (SELECT count(*) FROM schema_migrations),(SELECT count(*) FROM users),(SELECT count(*) FROM artists),(SELECT count(*) FROM follows),(SELECT count(*) FROM release_groups),(SELECT count(*) FROM destinations);" \
			| sha256sum | cut -d" " -f1
	'
}

docker run -d --name "$container" --network host -v "$volume:/data" \
	-e LISTEN_ADDR=":$port" -e PUBLIC_URL="http://127.0.0.1:$port" \
	-e ALLOW_INSECURE_HTTP=true -e MUSICBRAINZ_CONTACT="restore-smoke@example.invalid" \
	-e APP_ENCRYPTION_KEY="$APP_ENCRYPTION_KEY" \
	-e SESSION_SECRET="${SESSION_SECRET:-restore-smoke-session-secret-change-me-1234567890}" \
	-e SETUP_TOKEN="${SETUP_TOKEN:-restore-smoke-setup-token-change-me-1234567890}" \
	"$image" >/dev/null

wait_ready() {
	i=0
	while [ "$i" -lt 60 ]; do
		if wget -q -O /dev/null "http://127.0.0.1:$port/readyz"; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "restore: application did not become ready" >&2
	docker logs "$container" >&2 || true
	return 1
}

wait_ready
docker stop --time 60 "$container" >/dev/null
if [ "$(docker inspect -f '{{.State.ExitCode}}' "$container")" != "0" ]; then
	echo "restore: application did not shut down cleanly" >&2
	exit 1
fi
docker start "$container" >/dev/null
wait_ready
before_fingerprint=$(state_fingerprint)
docker stop --time 60 "$container" >/dev/null
if [ "$(docker inspect -f '{{.State.ExitCode}}' "$container")" != "0" ]; then
	echo "restore: application did not shut down cleanly after verification" >&2
	exit 1
fi
docker start "$container" >/dev/null
wait_ready
after_fingerprint=$(state_fingerprint)
if [ "$before_fingerprint" != "$after_fingerprint" ]; then
	echo "restore: database state changed across restart" >&2
	exit 1
fi
echo "restore: readiness, foreign-key check, key-preserving startup, and restart persistence passed (state $after_fingerprint)"
