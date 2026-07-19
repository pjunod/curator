#!/bin/sh
# Print Monarr's version string. Single source of truth, used by both the
# Makefile and deploy/Dockerfile so the two build paths can't drift.
#
#   tagged clone   → git describe: "0.3.0", "0.3.0-2-gabc1234", …-dirty
#   tagless clone  → VERSION file + commit: "0.3.0+gabc1234"
#   no git at all  → VERSION file: "0.3.0+unknown"
#
# The leading v is stripped; the UI prepends its own. The VERSION file is
# the release base and is bumped together with every release tag.
cd "$(dirname "$0")/.." || exit 1
D="$(git describe --tags --always --dirty 2>/dev/null || true)"
B="$(cat VERSION 2>/dev/null || echo 0.0.0)"
case "$D" in
  v*) printf '%s\n' "${D#v}" ;;
  "") printf '%s\n' "$B+unknown" ;;
  *)  printf '%s\n' "$B+g$D" ;;
esac
