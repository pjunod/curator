#!/bin/sh
# Create Monarr's host directories with the right ownership BEFORE the first
# `docker compose up`.
#
# Why this exists: a bind mount whose host directory does not exist is
# created by the Docker daemon as root:root. The container runs as
# ${PUID}:${PGID}, cannot create the SQLite file inside it, and Monarr won't
# start. Docker gives you no way to declare the ownership of a bind mount, so
# the host has to be right first. Run this once per host.
#
#   cd deploy && ./bootstrap.sh          # uses ./.env, or the built-in defaults
#   MONARR_DATA=/mnt/fast/monarr ./bootstrap.sh
#   ./bootstrap.sh --dry-run             # print what it would do
#
# Idempotent, and deliberately conservative: it CREATES missing directories
# with the right owner, and only REPORTS ones that already exist with the
# wrong owner. It will not recursively chown a directory you already have —
# on a media pool that is a multi-hour surprise, and on the wrong path it is
# a disaster. The report tells you the exact command if you want it.

set -eu

DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

cd "$(dirname "$0")"

# .env is optional; every value below has a default matching the compose file.
if [ -f .env ]; then
    # shellcheck disable=SC1091
    . ./.env
    echo "config: .env"
else
    echo "config: built-in defaults (no .env next to this script)"
fi

MONARR_DATA="${MONARR_DATA:-/srv/monarr}"
MONARR_POOL="${MONARR_POOL:-/srv/pool}"
PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

echo "target: uid ${PUID} gid ${PGID}"
echo "  data: ${MONARR_DATA}"
echo "  pool: ${MONARR_POOL}"
echo

SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

problems=0

ensure_dir() {
    dir="$1"
    if [ -d "$dir" ]; then
        owner="$(stat -c '%u:%g' "$dir" 2>/dev/null || stat -f '%u:%g' "$dir")"
        if [ "$owner" = "${PUID}:${PGID}" ]; then
            echo "  ok      $dir"
        else
            echo "  MISMATCH $dir (owned by $owner, want ${PUID}:${PGID})"
            echo "           fix: ${SUDO} chown -R ${PUID}:${PGID} $dir"
            problems=$((problems + 1))
        fi
        return
    fi
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "  create  $dir (dry run)"
        return
    fi
    ${SUDO} install -d -o "${PUID}" -g "${PGID}" -m 0755 "$dir"
    echo "  created $dir"
}

# /data: SQLite + backups. Must be local-disk semantics (docs/deployment.md).
ensure_dir "${MONARR_DATA}"
ensure_dir "${MONARR_DATA}/backups"

# The pool: media + the download client's completed folder, one filesystem so
# imports hardlink instead of copy.
ensure_dir "${MONARR_POOL}"
ensure_dir "${MONARR_POOL}/downloads"
ensure_dir "${MONARR_POOL}/media"
ensure_dir "${MONARR_POOL}/media/movies"
ensure_dir "${MONARR_POOL}/media/tv"
ensure_dir "${MONARR_POOL}/media/books"

echo
if [ "$problems" -gt 0 ]; then
    echo "$problems existing directory/directories have the wrong owner (listed above)."
    echo "Nothing was chowned — review the paths, then run the fix commands yourself."
    exit 1
fi

if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run — nothing was created."
else
    echo "Ready. Now: docker compose up -d --build"
fi
