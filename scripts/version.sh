#!/bin/sh
# Monarr's version is the VERSION file, verbatim — the single source of
# truth. No git describe, no commit suffix, no dependency on tags being
# present: a fresh clone, a Docker build on a host that never fetched tags,
# and a tagged release all report exactly what VERSION says. Bump VERSION
# when you cut a release (and tag the same value, for git's sake). The
# commit hash is stamped separately into buildinfo.Commit and shown on the
# Dashboard, so nothing is lost by keeping the version itself clean.
cd "$(dirname "$0")/.." || exit 1
V="$(cat VERSION 2>/dev/null || echo 0.0.0)"
printf '%s\n' "${V#v}"
