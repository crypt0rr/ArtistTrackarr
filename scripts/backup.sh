#!/bin/sh
set -eu
umask 077
CDPATH=''

# Pin the helper image so backups do not silently change behavior when Alpine
# publishes a new tag. The manifest digest supports the runtime architectures.
HELPER_IMAGE=${BACKUP_HELPER_IMAGE:-alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b}

# Resolve the volume mounted at /data from the Compose app container instead
# of guessing the project-prefixed Docker volume name.
service=${COMPOSE_SERVICE:-app}
output=${1:-artist-trackarr-backup.tgz}
output_dir=$(cd -- "$(dirname -- "$output")" && pwd)
output_file="$output_dir/$(basename -- "$output")"
checksum_file="$output_file.sha256"
backup_name=$(basename -- "$output_file")
container_id=""
restart_needed=0
host_uid=$(id -u)
host_gid=$(id -g)

cleanup() {
	if [ "$restart_needed" -eq 1 ]; then
		docker compose start "$service" >/dev/null || true
	fi
}
trap cleanup EXIT INT TERM

# Resolve the exact Compose container before stopping it.  Do not choose an
# arbitrary replica: a backup is only safe when the service has one known
# mounted volume to archive.
container_ids=$(docker compose ps -aq "$service" 2>/dev/null || true)
container_count=$(printf '%s\n' "$container_ids" | awk 'NF {count++} END {print count+0}')
if [ "$container_count" -eq 0 ]; then
	echo "backup: no Compose container found for service $service" >&2
	exit 1
fi
if [ "$container_count" -ne 1 ]; then
	echo "backup: expected exactly one Compose container for service $service" >&2
	exit 1
fi
container_id=$(printf '%s\n' "$container_ids" | awk 'NF {print; exit}')

mount_info=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}} {{.Name}}{{"\n"}}{{end}}{{end}}' "$container_id")
mount_count=$(printf '%s' "$mount_info" | awk 'NF {count++} END {print count+0}')
if [ "$mount_count" -ne 1 ]; then
	echo "backup: expected exactly one /data volume mount" >&2
	exit 1
fi
mount_type=$(printf '%s' "$mount_info" | awk 'NF {print $1}')
mount_name=$(printf '%s' "$mount_info" | awk 'NF {print $2}')
if [ "$mount_type" != "volume" ] || [ -z "$mount_name" ]; then
	echo "backup: /data is not backed by a named Docker volume" >&2
	exit 1
fi

# Set the trap before stopping the app so a partial/failed stop still leaves
# the service running again when the script exits.
restart_needed=1
docker compose stop "$service" >/dev/null

if ! docker run --rm --volumes-from "$container_id" "$HELPER_IMAGE" sh -ec 'test -s /data/artist-tracker.db'; then
	echo "backup: /data/artist-tracker.db is missing or empty" >&2
	exit 1
fi

docker run --rm --volumes-from "$container_id" -v "$output_dir:/backup" \
	-e "BACKUP_UID=$host_uid" -e "BACKUP_GID=$host_gid" -e "BACKUP_NAME=$backup_name" "$HELPER_IMAGE" sh -ec '
	tar czf "/backup/$BACKUP_NAME" -C /data .
	chown "$BACKUP_UID:$BACKUP_GID" "/backup/$BACKUP_NAME"
	chmod 600 "/backup/$BACKUP_NAME"'
(
	cd "$output_dir"
	sha256sum "$(basename -- "$output_file")" > "$(basename -- "$checksum_file")"
)
# Write the marker only after both the archive and checksum are complete. It is
# deliberately not part of the just-created archive; the next backup will
# include it, while the live application immediately sees the latest result.
docker run --rm --volumes-from "$container_id" "$HELPER_IMAGE" sh -ec \
	'printf "%s\\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /data/.artist-trackarr-last-backup && chown 10001:10001 /data/.artist-trackarr-last-backup && chmod 640 /data/.artist-trackarr-last-backup'
echo "backup: wrote $output_file and $checksum_file from volume $mount_name"
