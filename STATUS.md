# Monarr — Project Status

> **Snapshot 2026-07-17 · Phase 0 complete · next up: Phase 1 — Library**
>
> This file is the single place to answer "where are we?". Update it at the
> end of every working session. Design rationale lives in
> [docs/architecture.md](docs/architecture.md); decisions in
> [docs/adr/](docs/adr/); this file only tracks state.

## Now

- **Repo:** [github.com/monarr-media/monarr](https://github.com/monarr-media/monarr) (private), branch `main`, CI: 4 jobs (unit, lint, e2e, docker).
- **Runs today:** one binary serving the embedded React shell on **:7676** — `/api/v1` (system status, health, scheduler tasks, SSE event stream), SQLite with migrations, persisted task state, live events in the UI. No media management yet — that is Phase 1.
- **Working model:** builds/tests happen in the Claude cloud session (the Mac checkout has no Go/Node toolchain); CI is the enforcing gate; hooks skip gracefully where toolchains are missing.

## Phase ledger

| Phase | State | Delivered / definition of done |
|---|---|---|
| **0 — Walking skeleton** | ✅ **done** (2026-07-17) | Repo+CI, embedded UI, `/api/v1/system/status`, SQLite+goose+sqlc, config, slog, event bus, scheduler w/ persisted state, health page. Verified: UI loads, status reports, tests pass. |
| 0.5 — Test & CI hardening | ✅ **done** (2026-07-17) | Test pyramid (Go unit + arch rules, web vitest, Playwright E2E booting the real binary), git hooks, golangci-lint, modernized 4-job CI. |
| **1 — Library** | ⬜ **next** | TMDB adapter behind `MetadataProvider` port · `media_items` schema (`kind` CHECK includes `'book'` from migration one, ids incl. isbn/olid/asin — ADR 0006) · add/search movie & series with season/episode hydration · root folders · disk scan + reconcile · library browse UI. Done = real media folders imported and browsable. |
| 2 — Acquisition core | ⬜ | Parser + golden corpus, matcher, decision engine, quality profiles, Torznab, qBittorrent + SABnzbd, queue, importer + renamer. |
| 2.5 — Books | ⬜ | `book` kind end-to-end, ebooks + audiobooks (ADR 0006): metadata bake-off, book parser rules + Readarr corpus, format quality ladder, Calibre-friendly naming. |
| 3 — Automation | ⬜ | Wanted index, RSS loop, failed-download handling + blocklist, calendar, notifiers, backups. Done = runs unattended for a month. |
| 4 — Ecosystem | ⬜ | Sonarr/Radarr v3 compat personalities, conformance harness vs real Jellyseerr/Prowlarr/Bazarr. |
| 5 — Depth | ⬜ | Custom formats, more download clients, import lists, anime numbering. |

## Decisions to date

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-name-and-license.md) | Name **Monarr**, **GPL-3.0**, port **7676**. Amended: org/module is **monarr-media** (bare `monarr` = dormant 2011 GitHub account); images publish to **ghcr.io**, not Docker Hub (paid orgs). |
| [0002](docs/adr/0002-single-media-table.md) | One `media_items` table + `kind`; files↔episodes n:m; Wantable abstraction. |
| [0003](docs/adr/0003-compat-personalities.md) | v3 compat as `/sonarr` + `/radarr` URL-base personalities, translation-only. |
| [0004](docs/adr/0004-sqlite-only.md) | SQLite only (modernc, WAL, single writer); no Postgres. |
| [0005](docs/adr/0005-filesystem-adoption.md) | Migration by filesystem adoption; *arr DB import is a stretch item. |
| [0006](docs/adr/0006-books-third-media-kind.md) | **Books as a third kind**, ebooks + audiobooks together, Phase 2.5. |

## Commit history (milestones)

```
b3c9af1  hooks degrade gracefully when toolchain missing
0da9a51  test pyramid, git hooks, golangci-lint, modernized CI
ce6b3d9  distribute container images via GHCR, not Docker Hub
f13d70d  rename module to github.com/monarr-media/monarr
1261c78  ADR 0006: books as a third media kind (Phase 2.5)
154b461  Phase 0: walking skeleton
```

## Open items / parked

- File a **dormant-username request** with GitHub Support for the `monarr` handle (2011 account, zero repos). If granted: transfer repo + one-commit module rename.
- Register a domain before going public (ADR 0001).
- Flip the repo public when ready; then enable a branch ruleset requiring the four CI checks.
- goreleaser config (binaries + ghcr.io image publishing) — natural to add when there's something worth tagging, likely end of Phase 1.

## Phase 1 — first concrete steps

1. Migration `0002`: `media_items` (kind CHECK `movie|series|book`), `seasons`, `episodes`, `media_files` + n:m link table, `root_folders`, `settings` (TMDB API key lives in DB per the config philosophy).
2. Define the `MetadataProvider` port (first port with a real consumer) + TMDB adapter with search/hydrate + rate limiting.
3. `/api/v1` endpoints + UI: search TMDB → add movie/series → browse library.
4. Disk scan + reconcile against root folders (flag TMDB/TVDB ordering disagreements — risk register §11.6).
