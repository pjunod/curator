# Monarr — Project Status

> **Snapshot 2026-07-25 · ALL PHASES COMPLETE (90/91 — only the stretch *arr DB importer deferred) · v0.6.0 · Phase 6 "quality truth" built: on-disk quality is measured (ADR 0013), a profile is a target (ADR 0014), and the duplicate-grab chain is dead · next: launch logistics + real-world validation of the inference bands**
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
| 2 — Acquisition core | **16/16 ✅** | search → grab → import → correctly named file (movie + season pack) |
| 2.5 — Books (ADR 0006) | **7/7 ✅** | grab an ebook and an audiobook, correctly named |
| 3 — Automation | **9/9 ✅** | runs unattended for a month |
| 4 — Ecosystem compat | **5/6 ✅** (stretch deferred) | Jellyseerr/Prowlarr/Bazarr work against the shim |
| 5 — Depth & parity | **7/7 ✅** | custom formats, client zoo, lists, anime |
| 6 — Quality truth (ADR 0013/0014) | **9/9 ✅** | on-disk quality measured; target profiles; churn regression pinned |
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

## Phase 2 — Acquisition core ✅ (shipped 2026-07-17)

- [x] Release-name parser v1 (pure function, table-driven)
- [x] Golden corpus harness over `testdata/releases/` at 100% conformance (54 cases hand-seeded from upstream naming patterns; literal Sonarr/Radarr suite port remains an easy extension)
- [x] Parser fuzzing (must never panic on arbitrary bytes)
- [x] Wantable implementations (movie, episode, season) — season-pack fan-out modeled
- [x] `SearchPlanner` + `ReleaseMatcher` per media kind
- [x] Matcher: title normalization, year tolerance, season/episode + pack coverage
- [x] Decision engine v1 with machine-readable rejection reasons
- [x] Quality model + profiles (ordered groups, cutoff, upgrades)
- [x] Torznab/Newznab indexer adapter (search + caps test, one adapter for both; RSS lands with Phase 3)
- [x] Indexer management (API + UI)
- [x] Interactive search UI with rejection reasons attached
- [x] qBittorrent download-client adapter
- [x] SABnzbd download-client adapter
- [x] Queue tracker: client polling reconciled to the Download state machine
- [x] Importer: scan payload, map files→Wantables, per-file decisions
- [x] Renamer with Sonarr/Radarr-compatible tokens; hardlink-or-copy import

## Phase 2.5 — Books (ADR 0006) ✅ (shipped 2026-07-18)

- [x] Metadata bake-off → **Open Library** (open data, no key, work-level ids, covers; Google Books rate-limits anonymous callers, Hardcover needs an account) — `BookProvider` port + adapter, fixture contract-tested
- [x] Book external IDs end-to-end (isbn13 from editions, work OLID, asin column reserved) — unique `(kind, olid)`
- [x] Book-mode parser rules + golden corpus (12 cases seeded from Readarr/scene naming patterns; literal Readarr suite port remains an easy extension) — author extraction, order-swap tolerant matching
- [x] Book `SearchPlanner`/`ReleaseMatcher` (author+title queries; Torznab 7000/7020 + 3030)
- [x] Format quality ladder: PDF<MOBI<AZW3<EPUB + MP3<M4B; seeded **Ebook** (id 4) and **Audiobook** (id 5) profiles; books default to Ebook
- [x] `{Author Name}/{Book Title}` naming, Calibre-friendly (`Title - Author.ext`); import + scan grade book files by extension
- [x] Add/browse books in the UI (Book tab in Add, Books library filter, author on detail, release search + grab) — e2e: search → add → grab EPUB → auto-import, M4B rejected by profile

## Phase 3 — Automation ✅ (shipped 2026-07-18)

- [x] Wanted index (in-memory, invalidated on add/scan/import/delete events, lazily rebuilt; in-flight downloads and pack-covered episodes excluded) — `GET /wanted`
- [x] RSS sync loop per indexer (15 min, jittered): fetch → parse → match wanted → decide → auto-grab
- [x] Backlog search (12 h + manual): planned queries per wantable, best accepted release grabbed, 20/run cap
- [x] Failed-download handling: blocklist (unique release+indexer, `GET/DELETE /blocklist`) + automatic re-search on client failure or import error
- [x] Calendar (API + UI): airing episodes + movie/book release dates, agenda view with -7/+30 day paging
- [x] Webhook notifier (stable lowercase JSON wire format)
- [x] Discord notifier (embeds via webhook URL)
- [x] Plex/Jellyfin library-refresh notifiers (poked on imports only, never chatter)
- [x] Scheduled SQLite online backups: daily `VACUUM INTO`, last 7 kept, listed on the System page — e2e proves the whole circle: delete file → rescan → wanted → RSS re-grab → re-import → webhook notified

## Phase 4 — Ecosystem compat ✅ (shipped 2026-07-18; stretch importer deferred)

- [x] `/sonarr/api/v3` personality: status/health/profiles/languageprofile/tag/rootfolder/queue/history/command + series list/get/add (tvdbId resolved via TMDB /find), series/lookup (`tvdb:` + text), episode + episodefile for Bazarr
- [x] `/radarr/api/v3` personality: movie list/get/add (tmdbId native), movie/lookup, moviefile; books never leak through either personality
- [x] X-Api-Key auth (header or ?apikey=; key auto-generated, shown in Settings), case-insensitive routes, plausible version strings (Sonarr 4.0.10 / Radarr 5.14.0)
- [x] Unknown-v3-request logging (counter + warn line naming the path) to guide shim expansion
- [x] Conformance harness: `test/conformance/` docker-compose vs real Jellyseerr + Prowlarr + Bazarr, plus an in-process fake-consumer suite replaying their exact call flows (Jellyseerr add flows, Prowlarr indexer sync incl. idempotent re-sync + PUT/DELETE, Bazarr enumeration) that runs in CI
- [ ] (stretch) one-shot *arr DB importer — deferred post-1.0; filesystem adoption (ADR 0005) is the supported migration path

## Phase 5 — Depth & parity tail ✅ (shipped 2026-07-18)

- [x] Custom-format scoring engine: regex rules with scores summed per release; breaks ties in search ranking and automation best-pick; API/UI CRUD, invalid patterns rejected up front, scores shown in interactive search
- [x] More download clients: **Transmission** (RPC + 409 session handshake), **Deluge** (web JSON-RPC), **NZBGet** (JSON-RPC append-by-URL, usenet routing) — contract-tested, in the client factory + settings UI
- [x] Import lists: TMDB Popular/Top Rated + public Trakt lists (per-list client id); `importlists.sync` (12 h) adds missing entries with list policy, duplicate-safe; API/UI CRUD
- [x] Anime absolute numbering: `[Group] Show - 15` parsing (v2/ranges), absolute matching on episodes, cumulative absolute numbers derived at TMDB hydration (AniDB/TVDB mapping tables remain the refinement path); corpus 70/70
- [x] Auth hardening: API key everywhere (X-Api-Key / ?apikey=), opt-in `authRequired` gate on /api/v1 with salted-hash credentials + session login (UI login page); refuses to enable without credentials; key revealed in Settings → Security
- [x] Mass editor: select-mode on the Library page → bulk monitor/unmonitor/profile via `POST /library/bulk`
- [x] Optional Prometheus `/metrics` (`MONARR_METRICS=true`): build info, items by kind, active queue, wanted total, uptime — hand-rolled exposition, no new dependency

## Phase 6 — Quality truth (ADR 0013 + 0014 — built 2026-07-25, v0.6.0)

Plan: [docs/plan-quality-truth.md](docs/plan-quality-truth.md). Fixes the
adopted-file blind spot (unknown on-disk quality is hunted as *missing* →
duplicate grabs) and replaces the allowed-list+cutoff profile model with
target-based profiles ("Any — upgrades until WEB-DL 1080p, then stops" was
the symptom).

- [x] `domain/mediainfo` native prober: MKV (EBML) + MP4 header walkers — resolution, codec, bit depth, HDR/DV, audio, duration/bitrate; fixture corpus + fuzz; no runtime deps (plan §4, M1)
- [x] Migration 0018: `media_files` media_info + provenance/confidence/probed_at; `DiskStateForItem` replaces ambiguous `BestQualityForItem` (plan §5/§7, M2)
- [x] Probe wiring: `library.probe` jobs from scan (deduped per file), inline probe on import; backfill = first post-upgrade scan (plan §6.1–6.2, M2)
- [x] Source inference + filename cross-check: measured resolution absolute; source inferred with confidence; tokens demoted to hints; provenance recorded (plan §4.3, M3)
- [x] Target-based `Profile` (floor/target/upgrades) + migration 0019 in-place seed rewrite, ids stable; "Any" retired → "1080p" (ADR 0014 §4, M4)
- [x] Decision engine + wanted index on the `Met/Acceptable/Upgrade` predicates — unknown ≠ missing; `TestUnknownQualityOnDiskIsNotHunted` pins the churn fix (M4)
- [x] Profile CRUD API + editor UI; QualityFacts measured pill + provenance badges; Files table quality columns (plan §6.3, M5)
- [x] Compat `/qualityprofile` synthesized from targets — fake-consumer suite green untouched (plan §6.4, M5)
- [x] Post-import verification: `quality_mismatch` history event (log-only); docs updated (usage/settings/architecture); ADRs 0013/0014 → Accepted (M6)

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
| [0007](docs/adr/0007-remote-database-postgres.md) | Postgres for cluster HA evaluated, deferred with revisit triggers — superseded by 0008 |
| [0008](docs/adr/0008-distributed-execution.md) | Multi-host execution: leased job queue first (on SQLite), Postgres for clustered mode, mount-aware routing |
| [0009](docs/adr/0009-root-folder-kinds.md) | Root folders carry a media kind; mixed roots always ask |
| [0010](docs/adr/0010-scan-adopts.md) | Scan proposes matches; adoption is bulk work with a confidence bar, not a to-do list |
| [0011](docs/adr/0011-series-metadata-provider.md) | Series metadata is a chain: TheTVDB when keyed, TVmaze free, TMDB always |
| [0012](docs/adr/0012-manual-entries.md) | Manual entries: a library record no provider backs, episodes read off the disk |
| [0013](docs/adr/0013-measured-quality.md) | **Proposed:** on-disk quality is measured (native probe); filename demoted to hint; unknown ≠ missing; don't churn |
| [0014](docs/adr/0014-target-profiles.md) | **Proposed:** a profile is a floor + target + upgrades switch; grabs capped at target resolution; "Any" retired |

## Milestone commits

```
84e29a7..HEAD     Phase 6 (5 commits): native mediainfo prober + corpus/fuzz ·
                  migration 0018 + probe jobs · import measures what it
                  placed · target profiles + migration 0019 + churn fix ·
                  profile CRUD/editor + provenance UI
70d3ba9..84e29a7  Phase 5 (2 commits): custom formats · transmission/deluge/
                  nzbget · import lists · anime absolute · auth + sessions ·
                  mass editor · /metrics
1b8671b           Phase 4: /sonarr + /radarr v3 personalities, X-Api-Key,
                  conformance harness + fake-consumer suite
3037805..21662ec  Phase 3 (3 commits): wanted index · rss/backlog loops ·
                  blocklist + auto re-search · calendar · notifiers · backups
b661b2e..bde394d  Phase 2.5 (3 commits): book domain · Open Library adapter ·
                  books API/UI/e2e (ADR 0006)
16fea93..6b35b62  Phase 2 (8 commits): parser+corpus+fuzz · wantables/matcher/
                  decision · schema · torznab/qbit/sab adapters · services ·
                  API · UI · full-loop e2e (movie + season pack)
192ba45..743bb93  Phase 1 (6 commits): schema+storage · TMDB port/adapter ·
                  library service · API+wiring · UI · e2e flow suite
8bd785a  STATUS.md work ledger        b3c9af1  hooks degrade gracefully
0da9a51  test pyramid + hooks + CI    ce6b3d9  images via GHCR
f13d70d  module → monarr-media        1261c78  ADR 0006: books
154b461  Phase 0: walking skeleton
```
