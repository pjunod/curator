#!/bin/sh
# Fail the build when coverage drops below the floor in ./COVERAGE_MIN.
#
# Usage: scripts/coverage-gate.sh <coverage-profile>
#
# The floor is a RATCHET, not the goal. The goal is in TARGET below and in
# the README; the floor is wherever the repository actually is today, so
# that:
#
#   * a change that lowers coverage fails immediately, while the person who
#     lowered it is still holding it — which is the only moment the fix is
#     cheap, and
#   * the goal can be a real number rather than an aspiration nobody can
#     enforce, because the build is never red for a gap that predates the
#     commit under test.
#
# Setting the floor straight to TARGET would paint the build red for work
# nobody in that commit did, and a build that is always red is a build
# everybody learns to ignore. Raise COVERAGE_MIN in the same commit that
# earns it; this script nags when it has drifted far enough below the real
# number to be worth raising.
set -eu

TARGET=85.0        # where this repository is going. Also stated in the README.
SLACK=1.5          # how far above the floor we tolerate before nagging.

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
profile="${1:-}"
if [ -z "$profile" ] || [ ! -f "$profile" ]; then
	echo "usage: $0 <coverage-profile>" >&2
	exit 2
fi

min="$(tr -d ' \t\n\r' < "$root/COVERAGE_MIN")"
pct="$(sh "$root/scripts/coverage-badge.sh" "$profile")"

# awk rather than shell arithmetic: these are decimals.
if awk -v p="$pct" -v m="$min" 'BEGIN { exit !(p + 0 < m + 0) }'; then
	cat >&2 <<EOF
coverage ${pct}% is below the floor of ${min}% (./COVERAGE_MIN).

Something in this change is less covered than what it replaced. Add the
tests, or — if the drop is deliberate and defensible — say so in the commit
and lower the floor there, where a reviewer can see both at once.
EOF
	exit 1
fi

printf 'coverage %s%% (floor %s%%, target %s%%)\n' "$pct" "$min" "$TARGET"

if awk -v p="$pct" -v m="$min" -v s="$SLACK" 'BEGIN { exit !(p + 0 > m + s) }'; then
	printf 'the floor is stale: raise COVERAGE_MIN to %.1f to keep the gains\n' "$pct"
fi

if awk -v p="$pct" -v t="$TARGET" 'BEGIN { exit !(p + 0 < t + 0) }'; then
	printf 'still %.1f points short of the %s%% target\n' \
	  "$(awk -v p="$pct" -v t="$TARGET" 'BEGIN { printf "%.1f", t - p }')" "$TARGET"
fi
