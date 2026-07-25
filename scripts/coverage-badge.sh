#!/bin/sh
# Turn a Go coverage profile into a percentage and an SVG badge, with no
# third-party service in the loop. This replaced Codecov: a badge is one
# number and a rounded rectangle, and a repository that already runs the
# tests can render its own.
#
# Usage: scripts/coverage-badge.sh <coverage-profile> [output.svg] [filtered.out]
#
# Prints the percentage (e.g. "65.3") on stdout, so callers can use it in a
# summary or a threshold check. Writes the badge only when given a path, and
# keeps the filtered profile only when given a third — anything reporting a
# per-package breakdown should read that rather than re-implement the filter
# and quietly disagree with the badge.
#
# What counts, and why it is not simply the number `go test` prints:
#
#   * The profile must be produced with -coverpkg=./... — without it a
#     package appears only if it owns a _test.go file, so the population
#     being measured depends on where the tests live rather than on what they
#     exercise. Ten of this repo's forty packages were missing: cmd/monarr at
#     0%, and internal/domain/decision at 94%, thoroughly covered through its
#     callers. An arbitrary subset can flatter or understate; either way the
#     number is not about the tests.
#
#   * Generated packages are then excluded (see EXCLUDE below). sqlc and
#     oapi-codegen write tens of thousands of statements that no human will
#     ever write a test for; including them measures how much machine output
#     the repo contains, which is not a fact about the tests. Excluding them
#     is the difference between 60.7% and 65.3% here, and only the second
#     number changes when someone writes a test.
#
# Both filters live here so the badge, the Makefile target and CI cannot
# drift into reporting three different numbers.
set -eu

# EXCLUDE is an extended-regex matched against the profile's file paths.
EXCLUDE='/gen/'

profile="${1:-}"
out="${2:-}"
keep="${3:-}"

if [ -z "$profile" ] || [ ! -f "$profile" ]; then
	echo "usage: $0 <coverage-profile> [output.svg] [filtered.out]" >&2
	exit 2
fi

# Filter in a temporary copy, keeping the "mode:" header line, which
# go tool cover requires.
filtered="$(mktemp)"
trap 'rm -f "$filtered"' EXIT
head -n 1 "$profile" >"$filtered"
grep -v -E "$EXCLUDE" "$profile" | grep -v '^mode:' >>"$filtered" || true

pct="$(go tool cover -func="$filtered" | awk '/^total:/ {gsub(/%/, "", $NF); print $NF}')"
if [ -z "$pct" ]; then
	echo "$0: no total line in $profile — is it a Go coverage profile?" >&2
	exit 1
fi
printf '%s\n' "$pct"

[ -z "$keep" ] || cp "$filtered" "$keep"
[ -n "$out" ] || exit 0

# Shields' own thresholds, so the colour means what people already read it
# to mean elsewhere.
colour=$(awk -v p="$pct" 'BEGIN {
	if (p >= 90) print "#4c1";
	else if (p >= 80) print "#97ca00";
	else if (p >= 70) print "#a4a61d";
	else if (p >= 60) print "#dfb317";
	else if (p >= 50) print "#fe7d37";
	else print "#e05d44";
}')

label="coverage"
value="${pct}%"
# Verdana at font-size 11 averages a shade under 7px per character. The
# badge is a rounded rectangle either way, so approximate widths are fine as
# long as the text is not clipped — hence the generous padding.
label_w=$(awk -v n="${#label}" 'BEGIN { printf "%d", n * 6.8 + 10 }')
value_w=$(awk -v n="${#value}" 'BEGIN { printf "%d", n * 7.2 + 10 }')
total_w=$((label_w + value_w))
label_mid=$(awk -v w="$label_w" 'BEGIN { printf "%d", w * 5 }')
value_mid=$(awk -v lw="$label_w" -v vw="$value_w" 'BEGIN { printf "%d", (lw + vw / 2) * 10 }')
label_len=$((label_w * 10 - 100))
value_len=$((value_w * 10 - 100))

# Static SVG, no external fonts or references: it has to render inside
# GitHub's image proxy, which fetches the file and nothing else.
cat >"$out" <<SVG
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="${total_w}" height="20" role="img" aria-label="${label}: ${value}">
  <title>${label}: ${value}</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="${total_w}" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="${label_w}" height="20" fill="#555"/>
    <rect x="${label_w}" width="${value_w}" height="20" fill="${colour}"/>
    <rect width="${total_w}" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="110">
    <text aria-hidden="true" x="${label_mid}" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="${label_len}">${label}</text>
    <text x="${label_mid}" y="140" transform="scale(.1)" textLength="${label_len}">${label}</text>
    <text aria-hidden="true" x="${value_mid}" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="${value_len}">${value}</text>
    <text x="${value_mid}" y="140" transform="scale(.1)" textLength="${value_len}">${value}</text>
  </g>
</svg>
SVG
