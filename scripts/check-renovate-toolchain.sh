#!/bin/sh
set -eu

config=renovate.json
if [ ! -f "$config" ]; then
	printf '%s\n' "renovate-check: missing $config" >&2
	exit 1
fi

# Renovate names the same Go runtime differently for each manager. Keep all
# three package identities in one group so a toolchain update cannot create
# independently failing Docker, module, and setup-go pull requests.
for dependency in '"go"' '"docker.io/library/golang"' '"actions/go-versions"'; do
	if ! grep -Fq "$dependency" "$config"; then
		printf 'renovate-check: Go toolchain group is missing %s\n' "$dependency" >&2
		exit 1
	fi
done

# The quality tooling is pinned inside `go run <module>/cmd/...@<version>` strings,
# which no built-in Renovate manager parses: gomod reads go.mod and these modules
# are not required there, dockerfile reads FROM, and github-actions reads uses.
# Without a custom manager the group matched nothing and the pins sat frozen.
if ! grep -Fq '"custom.regex"' "$config"; then
	printf '%s\n' "renovate-check: the Go quality tooling group does not match custom.regex, so the go-run pins are invisible to Renovate" >&2
	exit 1
fi
if ! grep -Fq '"customManagers"' "$config"; then
	printf '%s\n' "renovate-check: no customManagers block, so nothing parses the go-run pins" >&2
	exit 1
fi

# Every `go run <module>@<version>` pin is hand-copied into the Makefile, the
# Dockerfile and the workflow. Renovate updates all three together, but a manual
# bump can update one and leave the others behind - and the lint that runs in CI
# would then differ from the lint a maintainer runs locally.
# Reduce each invocation to <module>@<version>, dropping the /cmd/... suffix so
# the module matches the depName Renovate's custom manager captures.
pins=$(git grep -h -o -E 'go run [^@[:space:]]+@v[^[:space:]"'"'"']+' -- Makefile Dockerfile .github/workflows |
	sed 's/^go run //; s|/cmd/[^@]*@|@|' | sort -u)
if [ -z "$pins" ]; then
	printf '%s\n' "renovate-check: no pinned go-run tooling found; the check no longer matches reality" >&2
	exit 1
fi
modules=$(printf '%s\n' "$pins" | sed 's/@.*//' | sort -u)
for module in $modules; do
	versions=$(printf '%s\n' "$pins" | grep "^$module@" | sed 's/.*@//' | sort -u)
	if [ "$(printf '%s\n' "$versions" | wc -l)" -ne 1 ]; then
		printf 'renovate-check: %s is pinned to more than one version: %s\n' \
			"$module" "$(printf '%s' "$versions" | tr '\n' ' ')" >&2
		exit 1
	fi
	# A module Renovate can see but that no package rule groups will still open
	# an ungrouped PR; keep the group and the pins in agreement.
	if ! grep -Fq "\"$module\"" "$config"; then
		printf 'renovate-check: %s is pinned but not named in any renovate.json package rule\n' "$module" >&2
		exit 1
	fi
done

# Every script that installs an EXIT trap to undo something - stopping a service,
# creating a container or a volume - must also trap HUP. In dash and busybox ash
# an untrapped fatal signal terminates the shell through its default disposition
# and the EXIT trap never runs, so a dropped SSH session during a backup would
# leave the application stopped with temporary files behind.
for script in scripts/*.sh; do
	if ! grep -q '^trap ' "$script"; then
		continue
	fi
	if ! grep -q '^trap .*\bHUP\b' "$script"; then
		printf 'renovate-check: %s installs a trap that does not cover HUP, so a dropped session skips its cleanup\n' "$script" >&2
		exit 1
	fi
done

# Keep the Alpine helpers used by shell rehearsals and smoke tests aligned with
# the pinned runtime image. Renovate scans these duplicate script references,
# while this check makes an accidental manual digest drift fail locally.
docker_alpine_pin=$(sed -n 's/^FROM //p' Dockerfile | tail -n 1 | awk '{print $1}')
if [ -z "$docker_alpine_pin" ]; then
	printf '%s\n' "renovate-check: no pinned Alpine runtime image found in Dockerfile" >&2
	exit 1
fi
helper_image_name=alpine
for script in scripts/backup.sh scripts/container-smoke.sh scripts/disaster-recovery-smoke.sh scripts/restore-smoke.sh; do
	helper_pins=$(grep -Eo "$helper_image_name:[^@[:space:]\"}]+@sha256:[a-f0-9]{64}" "$script" | sort -u)
	pin_count=$(printf '%s\n' "$helper_pins" | awk 'NF {count++} END {print count+0}')
	if [ "$pin_count" -ne 1 ] || [ "$helper_pins" != "$docker_alpine_pin" ]; then
		printf 'renovate-check: %s Alpine helper pin does not match Dockerfile runtime (%s)\n' \
			"$script" "$docker_alpine_pin" >&2
		exit 1
	fi
done

printf '%s\n' "renovate-check: Go toolchain identities are grouped"
printf 'renovate-check: %s pinned quality tools agree across all call sites\n' "$(printf '%s\n' "$modules" | wc -l | tr -d ' ')"
