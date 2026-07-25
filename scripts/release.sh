#!/bin/sh
# Bump VERSION and tag it. The version was stuck at 0.4.0 through 39 commits
# because nothing in the workflow ever asked for a bump: the file is the
# single source of truth (scripts/version.sh) and a source of truth nobody
# writes to is a constant.
#
# Usage: scripts/release.sh patch|minor|major
#
# Pre-1.0 reading of semver, which is what this project is in: a user-visible
# feature is a minor, a fix is a patch, and major stays 0 until the API and
# the compat personalities are stable.
set -eu
cd "$(dirname "$0")/.." || exit 1

part="${1:-}"
case "$part" in
patch | minor | major) ;;
*)
	echo "usage: $0 patch|minor|major" >&2
	exit 2
	;;
esac

current="$(cat VERSION)"
major="${current%%.*}"
rest="${current#*.}"
minor="${rest%%.*}"
patch="${rest#*.}"

case "$part" in
major)
	major=$((major + 1))
	minor=0
	patch=0
	;;
minor)
	minor=$((minor + 1))
	patch=0
	;;
patch) patch=$((patch + 1)) ;;
esac
next="$major.$minor.$patch"

if [ -n "$(git status --porcelain)" ]; then
	echo "release: working tree is dirty — commit or stash first" >&2
	exit 1
fi
if git rev-parse -q --verify "refs/tags/v$next" >/dev/null; then
	echo "release: v$next is already tagged" >&2
	exit 1
fi

printf '%s\n' "$next" >VERSION
git add VERSION
git commit -q -m "chore: cut $next"
git tag -a "v$next" -m "v$next"

echo "$current -> $next (tagged v$next)"
echo "push with: git push origin main --follow-tags"
