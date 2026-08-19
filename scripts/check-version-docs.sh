#!/bin/sh
set -eu

source_version=$(sed -n 's/^const Current = "\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)"$/\1/p' internal/version/version.go | head -n 1)
if [ -z "$source_version" ]; then
	echo "version-check: could not read a semantic Current version from internal/version/version.go" >&2
	exit 1
fi

# The source-controlled version is authoritative for every build. A tag may
# be supplied by CI, but it must agree with the version compiled into the
# application.
tag=${1:-$source_version}
tag=${tag#v}
if ! printf '%s\n' "$tag" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo "version-check: expected a semantic version, got '$tag'" >&2
	exit 1
fi
if [ "$source_version" != "$tag" ]; then
	echo "version-check: source version is $source_version, but validation requested $tag" >&2
	exit 1
fi

if grep -n -E 'APP_VERSION|dev-\$\{GITHUB_SHA|internal/version\.Current=\$\{APP_VERSION' Dockerfile .github/workflows/docker.yml; then
	echo "version-check: build-time version injection is not allowed" >&2
	exit 1
fi

if ! grep -Fq "current release is \`v$tag\`" README.md; then
	echo "version-check: README.md does not identify v$tag as the current release" >&2
	exit 1
fi
if ! grep -Fq "Pushing a tag such as \`v$tag\` publishes \`$tag\`" README.md; then
	echo "version-check: README.md release image example is not v$tag" >&2
	exit 1
fi
echo "version-check: documentation matches v$tag"
