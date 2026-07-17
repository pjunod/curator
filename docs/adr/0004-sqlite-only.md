# ADR 0004 — SQLite only (modernc, WAL, single writer); no Postgres

- **Status:** Accepted
- **Date:** 2026-07-17

## Context

Upstream Sonarr/Radarr support both SQLite and Postgres, paying a permanent dual-dialect tax
in queries, migrations, and CI. Monarr is a homelab app: write volume is a few writes per
second at peak, and the deployment target is a NAS, a Pi, or a small container.

## Decision

- **SQLite is the only supported database.** Postgres is explicitly off the roadmap and will
  be reconsidered only on demonstrated need.
- Driver: **`modernc.org/sqlite`** (pure Go, no CGO) so cross-compilation stays trivial.
- **WAL mode** with a **single-writer discipline**: all writes flow through one connection
  (`SetMaxOpenConns(1)` on the write handle); reads use a separate pool. This sidesteps
  SQLite's write-lock contention entirely at this scale.
- Schema managed by **goose** migrations embedded via `go:embed` (up-only; we roll forward),
  queries by **sqlc** (compile-time-checked SQL, no ORM).

## Consequences

- One dialect everywhere: simpler queries, simpler tests, single-file backups (the online
  backup task is nearly free; a WAL checkpoint task runs on the scheduler).
- `go build` works on any host with no C toolchain; the binary stays static.
- If a future need for Postgres emerges, the sqlc layer localizes the pain, but nothing is
  designed for it today — that is the point.
