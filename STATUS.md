# Monarr — Project Status

> **Snapshot 2026-07-17 · Phases 0, 0.5, and 1 complete (37/37) · next: Phase 2 — Acquisition core (0/16)**
>
> This is the explicit work ledger: every deliverable we've committed to, and whether it is
> done. Checked = shipped and verified, not "mostly there". Update at the end of every
> working session. Design rationale: [docs/architecture.md](docs/architecture.md) ·
> decisions: [docs/adr/](docs/adr/) · this file: state only.

## Progress at a glance

| Phase | Done | Definition of done |
|---|---|---|
| 0 — Walking skeleton | **17/17 ✅** | `docker run` → UI loads, status reports, tests pass |
| 0.5 — Tests, hooks, CI hardening | **7/7 ✅** | full pyramid green locally and in CI |
| 1 — Library | **13/13 ✅** | real media folders imported and browsable |
| 2 — Acquisition core | **0/16 ⟵ next** | search → grab → import → correctly named file (movie + season pack) |
| 2.5 — Books (ADR 0006) | 0/7 | grab an ebook and an audiobook, correctly named |
| 3 — Automation | 0/9 | runs unattended for a month |
| 4 — Ecosystem compat | 0/6 | Jellyseerr/Prowlarr/Bazarr work against the shim |
| 5 — Depth & parity | 0/7 | custom formats, client zoo, lists, anime |
| Launch logistics | 2/6 | public repo, releases, name housekeeping |

## Phase 0 — Walking skeleton ✅ (shipped 2026-07-17)

- [x] Repo scaffold per blueprint §12 + GPL-3.0 license
- [x] Go module (`github.com/monarr-media/monarr`) + Makefile build orchestration
- [x] Single static binary serving the embedded React shell (go:embed, SPA fallback page)
- [x] Spec-first `/api/v1` (OpenAPI 3 + oapi-codegen): `system/status`, `health`, `system/tasks`, `tasks/{name}/run`
- [x] SQLite via modernc: WAL mode, single-writer discipline, read pool
- [x] goose migrations embedded in the binary (up-only policy)
- [x] sqlc compile-time-checked queries (app_meta, scheduled_tasks)
- [x] Config: env > optional JSON file > defaults (port 7676, data dir, log level/format)
- [x] Structured logging via log/slog (text/json)
- [x] Typed in-process event bus (non-blocking fan-out, typed subscribe)
- [x] Scheduler: jittered intervals, manual trigger, panic recovery, state persisted across restarts
- [x] Health-check registry (database, data-directory, web-ui) publishing transitions on the bus
- [x] SSE event stream `/api/v1/events` (bus → UI live)
- [x] React + TS + Vite shell: Dashboard + System pages, live event feed, dark theme
- [x] Dockerfile: multi-stage, distroless, `/data` volume, port 7676
- [x] ADRs 0001–0006 + full architecture blueprint in `docs/`
- [x] Architecture dependency-arrows enforced by test (`internal/arch_test.go`)

## Phase 0.5 — Tests, hooks, CI ✅ (shipped 2026-07-17)

- [x] Go unit tests across all infra/app packages (run with `-race` in CI)
- [x] Web unit tests (vitest) for the UI API/formatting helpers
- [x] E2E suite: Playwright boots the real compiled binary, 7 specs incl. bus→SSE→browser round-trip
- [x] Git hooks in `.githooks/` (`make hooks`): pre-commit fmt/vet/typecheck, pre-push unit suites, graceful skip when a toolchain is missing
- [x] golangci-lint v2 pinned (`make lint`), zero findings
- [x] CI: 4 parallel jobs (unit + coverage summary + gen-drift check, lint, e2e, docker), least-privilege permissions, per-ref cancellation
- [x] STATUS.md as the explicit work ledger (this file)

## Phase 1 — Library ✅ (shipped 2026-07-17)

- [x] Migration 0002: `media_items` with `kind` CHECK (`movie|series|book` — book reserved per ADR 0006), external ids incl. isbn/olid/asin
- [x] `seasons` + `episodes` child tables
- [x] `media_files` + n:m file↔episode link table (ADR 0002)
- [x] `root_folders` + `settings` tables (TMDB API key lives in the DB, managed in UI)
- [x] `MetadataProvider` port (search + hydrate) — first port with a real consumer
- [x] TMDB adapter: auth, rate limiting, response caching
- [x] Add-movie flow: search TMDB → add → metadata hydrated
- [x] Add-series flow with full season/episode hydration
- [x] Library list/detail API endpoints
- [x] Library browse UI (posters, list, detail)
- [x] Root-folder management (API + UI)
- [x] Disk scan: walk root folders, parse on-disk files
- [x] Reconcile: diff disk vs DB, repair links, flag TMDB-ordering disagreements for manual review

## Phase 2 — Acquisition core

- [ ] Release-name parser v1 (pure function, table-driven)
- [ ] Golden corpus: port Sonarr/Radarr parser test suites into `testdata/releases/`
- [ ] Parser fuzzing (must never panic on arbitrary bytes)
- [ ] Wantable implementations (movie, episode, season) — season-pack fan-out modeled
- [ ] `SearchPlanner` + `ReleaseMatcher` per media kind
- [ ] Matcher: title normalization, year tolerance, season/episode + pack coverage
- [ ] Decision engine v1 with machine-readable rejection reasons
- [ ] Quality model + profiles (ordered groups, cutoff, upgrades)
- [ ] Torznab/Newznab indexer adapter (search + RSS, one adapter for both)
- [ ] Indexer management (API + UI)
- [ ] Interactive search UI with rejection reasons attached
- [ ] qBittorrent download-client adapter
- [ ] SABnzbd download-client adapter
- [ ] Queue tracker: client polling reconciled to the Download state machine
- [ ] Importer: scan payload, map files→Wantables, per-file decisions
- [ ] Renamer with Sonarr/Radarr-compatible tokens; hardlink-or-copy import

## Phase 2.5 — Books (ADR 0006)

- [ ] Metadata bake-off (Open Library vs Google Books vs Hardcover) → book adapter behind the same port
- [ ] Book external IDs end-to-end (isbn10/13, olid, asin)
- [ ] Book-mode parser rules + Readarr GPL corpus ported
- [ ] Book `SearchPlanner`/`ReleaseMatcher` (author+title queries; Torznab 7000s + 3030)
- [ ] Format quality ladder: EPUB/AZW3/MOBI/PDF + M4B/MP3 as profile quality groups
- [ ] `{Author Name}/{Book Title}` naming, Calibre-friendly default layout
- [ ] Add/browse books in the UI

## Phase 3 — Automation

- [ ] Wanted index (in-memory, rebuilt on library events)
- [ ] RSS sync loop per indexer (~15 min, jittered)
- [ ] Backlog search
- [ ] Failed-download handling: blocklist + automatic re-search
- [ ] Calendar (API + UI)
- [ ] Webhook notifier
- [ ] Discord notifier
- [ ] Plex/Jellyfin library-refresh notifiers
- [ ] Scheduled SQLite online backups

## Phase 4 — Ecosystem compat

- [ ] `/sonarr/api/v3` personality (scoped to what Jellyseerr/Prowlarr/Bazarr call — §6)
- [ ] `/radarr/api/v3` personality
- [ ] X-Api-Key auth, case-insensitive routes, plausible upstream version strings
- [ ] Unknown-v3-request logging to guide shim expansion
- [ ] Conformance harness: docker-compose vs real Jellyseerr + Prowlarr + Bazarr
- [ ] (stretch) one-shot *arr DB importer

## Phase 5 — Depth & parity tail

- [ ] Custom-format scoring engine
- [ ] More download clients (Transmission, Deluge, NZBGet, …)
- [ ] Import lists (Trakt/TMDB)
- [ ] Anime absolute numbering (TVDB/AniDB mapping tables)
- [ ] Auth hardening: API key + session login for UI
- [ ] Mass editor + richer season views
- [ ] Optional Prometheus `/metrics`

## Launch logistics

- [x] `monarr-media` GitHub org (free) + private repo + initial push
- [x] Container registry decision: ghcr.io (no Docker Hub subscription — ADR 0001 amendment)
- [ ] File dormant-username request with GitHub Support for `monarr`
- [ ] Register a domain
- [ ] Flip repo public + branch ruleset requiring the four CI checks
- [ ] goreleaser: tagged releases with binaries + ghcr.io images (target: end of Phase 1)

## Decisions to date

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-name-and-license.md) | Name **Monarr**, **GPL-3.0**, port **7676**; amended: org/module `monarr-media`, images on ghcr.io |
| [0002](docs/adr/0002-single-media-table.md) | One `media_items` table + `kind`; files↔episodes n:m; Wantable abstraction |
| [0003](docs/adr/0003-compat-personalities.md) | v3 compat as `/sonarr` + `/radarr` URL-base personalities, translation-only |
| [0004](docs/adr/0004-sqlite-only.md) | SQLite only (modernc, WAL, single writer); no Postgres |
| [0005](docs/adr/0005-filesystem-adoption.md) | Migration by filesystem adoption; *arr DB import is a stretch item |
| [0006](docs/adr/0006-books-third-media-kind.md) | Books as a third kind — ebooks + audiobooks, Phase 2.5 |

## Milestone commits

```
192ba45..743bb93  Phase 1 (6 commits): schema+storage · TMDB port/adapter ·
                  library service · API+wiring · UI · e2e flow suite
8bd785a  STATUS.md work ledger        b3c9af1  hooks degrade gracefully
0da9a51  test pyramid + hooks + CI    ce6b3d9  images via GHCR
f13d70d  module → monarr-media        1261c78  ADR 0006: books
154b461  Phase 0: walking skeleton
```
