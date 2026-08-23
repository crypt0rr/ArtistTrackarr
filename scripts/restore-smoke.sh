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

image=${ARTIST_TRACKARR_IMAGE:-}
if [ -z "$image" ]; then
	echo "restore: set ARTIST_TRACKARR_IMAGE to an immutable image digest (for example ghcr.io/crypt0rr/artist-trackarr@sha256:...)" >&2
	exit 1
fi
case "$image" in
	*@sha256:*) ;;
	*)
		if [ "${RESTORE_ALLOW_MUTABLE_IMAGE:-false}" != "true" ]; then
			echo "restore: image must use an immutable @sha256 digest (set RESTORE_ALLOW_MUTABLE_IMAGE=true only for local rehearsal)" >&2
			exit 1
		fi
		echo "restore: warning: mutable image explicitly allowed" >&2
		;;
esac
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
	if [ "${RESTORE_ALLOW_LEGACY_ARCHIVE:-false}" != "true" ]; then
		echo "restore: checksum sidecar is required (set RESTORE_ALLOW_LEGACY_ARCHIVE=true only for a legacy archive)" >&2
		exit 1
	fi
	echo "restore: warning: checksum sidecar is absent; accepting an explicitly allowed legacy archive" >&2
fi
docker run --rm -v "$volume:/data" -v "$archive_path:/backup/restore.tgz:ro" "$HELPER_IMAGE" \
	sh -ec 'tar xzf /backup/restore.tgz -C /data; test -s /data/artist-tracker.db; apk add --no-cache sqlite >/dev/null; test "$(sqlite3 /data/artist-tracker.db "PRAGMA integrity_check;")" = ok; test -z "$(sqlite3 /data/artist-tracker.db "PRAGMA foreign_key_check;")"'

state_fingerprint() {
	docker run --rm -v "$volume:/data" "$HELPER_IMAGE" sh -ec "
set -eu
apk add --no-cache sqlite >/dev/null
fingerprint_file=\$(mktemp)
sqlite3 -noheader -separator '|' /data/artist-tracker.db >\"\$fingerprint_file\" <<'SQL'
-- Fingerprint durable logical state, not runner-owned timestamps, claims,
-- observations, sessions, or queue status that may legitimately change
-- while the restored process starts and performs maintenance.
SELECT 'schema',version FROM schema_migrations ORDER BY version;
SELECT 'users',id,email,username,password_hash,role,timezone,reminder_time FROM users ORDER BY id;
SELECT 'artists',id,mbid,name,sort_name,artist_type,country,disambiguation,spotify_id,spotify_url,spotify_image_url FROM artists ORDER BY id;
SELECT 'follows',user_id,artist_id FROM follows ORDER BY user_id,artist_id;
SELECT 'releases',id,mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,musicbrainz_url,spotify_url,itunes_url,spotify_id,itunes_id,source,artist_credit_role,itunes_artwork_url FROM release_groups ORDER BY id;
SELECT 'destinations',id,user_id,name,service,hex(encrypted_url),enabled,transport_status,transport_message FROM destinations ORDER BY id;
SELECT 'events',id,user_id,release_group_id,event_type,title,body FROM notification_events ORDER BY id;
SELECT 'prefs',user_id,albums,eps,singles,announcements,release_day,release_digest_enabled,release_digest_frequency,hold_conflicting_notifications FROM notification_preferences ORDER BY user_id;
SELECT 'genres',artist_id,genre,source FROM artist_genres ORDER BY artist_id,genre,source;
SELECT 'credits',release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,provider_url,confidence FROM release_credits ORDER BY release_group_id,artist_id,provider,provider_id,role,track_title;
SQL
sha256sum \"\$fingerprint_file\" > \"\$fingerprint_file.sha256\"
fingerprint_hash=\$(cut -d' ' -f1 \"\$fingerprint_file.sha256\")
printf '%s\n' \"\$fingerprint_hash\"
"
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
# The rehearsal volume is destroyed on exit, so a marker written into it is
# only ever a record of this script's own success and reaches nothing that
# outlives the run. Keep writing it there - it costs nothing and documents the
# rehearsal for anyone inspecting the volume before the trap fires - but the
# line an operator actually reads is on the live instance.
docker run --rm -v "$volume:/data" "$HELPER_IMAGE" sh -ec \
	'printf "%s\\n%s\\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "ok" > /data/.artist-trackarr-last-restore && chown 10001:10001 /data/.artist-trackarr-last-restore && chmod 640 /data/.artist-trackarr-last-restore'

# Opt-in: also stamp the result on the live instance, which is where the
# application reads it from. "When did we last prove we can restore, and did it
# pass?" is the most valuable line in a backup-confidence report, and without
# this it is the one line that can never be populated: the admin page, the
# support report, /admin/diagnostics.json and thirty days of hourly snapshots
# all read "not recorded" forever, which trains an operator to ignore the
# field. It stays opt-in because the rehearsal is otherwise strictly isolated
# from production, and writing to a live volume is not something this script
# should start doing unannounced.
if [ "${RESTORE_RECORD_RESULT:-false}" = "true" ]; then
	service=${COMPOSE_SERVICE:-app}
	# Resolve the live container the way scripts/backup.sh does. A rehearsal
	# result must never be attributed to an arbitrary replica.
	live_ids=$(docker compose ps -aq "$service" 2>/dev/null || true)
	live_count=$(printf '%s\n' "$live_ids" | awk 'NF {count++} END {print count+0}')
	if [ "$live_count" -ne 1 ]; then
		echo "restore: RESTORE_RECORD_RESULT needs exactly one Compose container for service $service; found $live_count" >&2
		exit 1
	fi
	live_id=$(printf '%s\n' "$live_ids" | awk 'NF {print; exit}')
	live_mounts=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}} {{.Name}}{{"\n"}}{{end}}{{end}}' "$live_id")
	live_mount_count=$(printf '%s' "$live_mounts" | awk 'NF {count++} END {print count+0}')
	if [ "$live_mount_count" -ne 1 ]; then
		echo "restore: RESTORE_RECORD_RESULT expected exactly one /data volume mount on $service" >&2
		exit 1
	fi
	if [ "$(printf '%s' "$live_mounts" | awk 'NF {print $1}')" != "volume" ]; then
		echo "restore: RESTORE_RECORD_RESULT requires /data to be a named Docker volume" >&2
		exit 1
	fi
	# Same UID and atomic-rename pattern as the backup marker in backup.sh, so
	# the application never reads a half-written file.
	docker run --rm --user 10001:10001 --volumes-from "$live_id" "$HELPER_IMAGE" sh -ec '
umask 027
marker=/data/.artist-trackarr-last-restore
temporary="$marker.$$"
printf "%s\\n%s\\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "ok" > "$temporary"
mv -f "$temporary" "$marker"'
	echo "restore: recorded the rehearsal result on live service $service"
fi
echo "restore: readiness, foreign-key check, key-preserving startup, and restart persistence passed (state $after_fingerprint)"
