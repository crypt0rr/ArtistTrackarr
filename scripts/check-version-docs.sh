#!/bin/sh
set -eu

# Release tags are the source of truth for the binary/image version. Branch,
# local, and development builds intentionally skip this check because they
# use a dev-<sha> identity rather than a semantic release.
tag=${1:-${APP_VERSION:-}}
tag=${tag#v}
case "$tag" in
	""|dev|dev-*)
		exit 0
		;;
esac

case "$tag" in
	[0-9]*.[0-9]*.[0-9]*)
		;;
	*)
		echo "version-check: expected a semantic version, got '$tag'" >&2
		exit 1
		;;
esac

if ! grep -Fq "current release is \`v$tag\`" README.md; then
	echo "version-check: README.md does not identify v$tag as the current release" >&2
	exit 1
fi
if ! grep -Fq "Pushing a tag such as \`v$tag\` publishes \`$tag\`" README.md; then
	echo "version-check: README.md release image example is not v$tag" >&2
	exit 1
fi
echo "version-check: documentation matches v$tag"
