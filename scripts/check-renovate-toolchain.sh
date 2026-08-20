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

printf '%s\n' "renovate-check: Go toolchain identities are grouped"
