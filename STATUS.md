# Monarr — Project Status

> **Snapshot 2026-09-23 · v0.28.0 · The native app gained the web's
> Interactive search: every release, rejections visible, Grab on any row, per
> item / pack / episode / copy / edition.**
>
> Previously: v0.24.1 · Curator hands Cinema exact book work and edition
> metadata when an ebook or audiobook import lands (§Phase 2.5 — Books).
>
> Previously: v0.23.0 added audiobook curation · v0.20.0 rebuilt the calendar ·
> v0.19.1 made Access a first-class web tab · v0.19.0 added configurable
> download-priority policies. Full gate green: lint · Go · 76 web unit tests ·
> 52 native unit tests · web build · 101/101 browser tests.
>
> This is the explicit work ledger: every deliverable we've committed to, and whether it is
> done. Checked = shipped and verified, not "mostly there". Update at the end of every
> working session. Design rationale: [docs/architecture.md](docs/architecture.md) ·
> decisions: [docs/adr/](docs/adr/) · this file: state only.

## Delivery — Native interactive search

**Status:** merged `7d1b5e7` · server deployed · clients handed off · **Updated:**
2026-09-23 · **Version:** 0.28.0 (mobile 0.27.0, build 25) · PR [#36](https://github.com/pjunod/curator/pull/36)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Branch `feat/mobile-interactive-search` on upstream `4a90fb0`; user checkout untouched. Pushed through a scratch clone on the Mac VM because the cloud git proxy refuses this repository; the repo has since been renamed `pjunod/curator` on GitHub. |
| API client | complete | `searchReleases` (120 s timeout, partial-result headers) and `grabRelease` in `mobile/src/api.ts`. |
| Search screen | complete | `mobile/src/screens/ReleaseSearchScreen.tsx`: every candidate, rejections visible, Would-be-grabbed view, Grab / Grab anyway (confirm), partial banner, season + episode chips, late answers dropped. |
| Detail entry points | complete | Interactive search on the item (series: first real season), Search pack per season, Search per quality copy / book edition (series copies open on the first season). Hardware Back returns to the detail page. |
| Feature gate | none | Nothing to enable: the screen calls the server's existing endpoints. No Developer-tab section because there is no toggle. |
| Adversarial review | addressed | 8 findings, all fixed (series copy without a season → 404; series with no seasons; copy name lost from subtitle; late search answer over a newer scope / setState after close; filter re-rendering every row; inert chips; two doc inaccuracies). Plus a pre-existing server bug it found: whole-item grabs (movies/books, web and mobile) failed candidate-token verification (search issues season 0, grab defaults to −1) and lost retained match evidence — fixed in `consumeCandidateToken`, regression-tested at both the service and API level. |
| Main was red — fixed here | complete | (1) e2e fake indexer advertised no search capabilities, so season searches were generic probes answered with the movie catalogue — 6 browser specs failed from that. (2) A grab/poll race erased the `grabbed` handoff step when a client finished instantly (phase7 flake) — the step is now written before the client is asked. (3) Queue rows were 156 px because the identity line wrapped four times — one elided line now. (4) Wanted's direct row hid which copy was wanted — pill restored. (5) Coverage was 82.4 % against the 85 % floor — six new test files cover the API/library/adapter/acquisition gaps; a blank alias now answers 400 not 500. |
| Full gate | green | Locally: lint 0 issues · Go race+coverage 87.1 % (floor now 86.0) · web 88 tests + build · mobile strict TS + 89 tests + Expo Doctor + iOS/Android bundles · e2e 111/111 twice · compat 17/17. On GitHub for `0346afe`: Lint, Tests (unit + e2e + mobile), Compat and Docker all green — the first fully green run since `4a90fb0`. |
| Deploy — server | complete | `deploy.yml -e only=monarr --limit nuc3 -e sync=false` from the Mac VM: nuc3 rebuilt and healthy, `/api/v1/system/status` reports 0.28.0 @ `7d1b5e7`. His `~/code/monarr` checkout was not synced (sync=false) and is untouched. |
| Deploy — clients | handoff | Physical iOS/Android installs need this Mac's Xcode/adb: `curator-mobile-deploy-handoff.md` (session outputs) is the prompt for a session with that access. |

## Previous delivery — Folder repair

**Status:** adversarial findings addressed; fast lane green · **Updated:** 2026-09-19 ·
**Version:** 0.25.2 · PR [#30](https://github.com/pjunod/monarr/pull/30)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Integrated onto upstream `3aaec3f`; user checkout left untouched after the workflow instruction. |
| Undownloaded titles | implemented | Missing-folder reporting requires previously recorded media; old reports are filtered on read. |
| Real folder issues | implemented | Missing, inaccessible, and non-directory paths have distinct explanations. |
| Bulk repair | implemented | Browse existing folders, apply entered paths across pages, retain per-row errors, and recheck restored drives. |
| File preservation | implemented | Matching files retain identity, copy assignment, quality, provenance, and episode links through a transactional location update. |
| Adversarial review | addressed | Preserve moved-file identities; remove silent retention of unavailable separate-copy records. Both findings have regression coverage. |
| Fast lane | green | Go 1.25.7 focused folder service/API regressions, one bulk-repair browser test, web typecheck/production build, backend build, lint (0 issues), generated API consistency, and focused identity/search regressions. |
| Full suites | delegated | Separate batch process handles full unit failures under the 2026-09-19 workflow. Existing GitHub Actions are unchanged. |

The review fixes are in `38f840e`; `e00579e` resolves three existing
staticcheck findings without changing behavior. Checks were run after the
review fixes. Full local unit and browser suites were not rerun. GitHub's
existing workflows still run automatically; the first PR run's mobile job
failed Expo Doctor because seven pinned Expo packages are behind the SDK's
expected patch versions. No mobile files change in this PR; that finding is
recorded for the separate batch process. The linked PR is the authoritative
merge state.

See [folder repair](docs/settings.md#disk-scan--repairing-folders-with-recorded-media)
for operator behavior. No feature toggle is introduced.

## Previous delivery — Open Library refresh resilience

**Status:** ready to merge; adversarial review approved; fast lane green ·
**Updated:** 2026-09-19 · **Version:** 0.25.1

| Item | State | Evidence |
|---|---|---|
| Separate clone | complete | Work based on upstream `8ce3590`; original checkout excluded from delivery. |
| Connection reset recovery | implemented | Three attempts with backoff for transient network and HTTP failures. |
| Open Library pacing | implemented | One request per second; retries share the same budget. |
| Retry-After and response cache | implemented | Respect provider delays; stop a lookup for waits over 30 seconds; cache only valid JSON. |
| Regression coverage | green | Reset recovery/exhaustion, interrupted body, timeouts, HTTP errors, cancellation, pacing, and cache behavior. |
| Adversarial review | approved | Independent review of `d316497` against `8ce3590`; no actionable introduced defects. |
| Fast lane | green | Go 1.25.7: adapter/library and architecture tests (`-count=1 -shuffle=on`), scoped vet, backend build, formatting and diff checks passed. |
| Delivery | ready for PR | One batch includes code, regression tests, operator documentation, and patch version. |

The operator behavior is documented under
[Refresh metadata](docs/usage.md#the-item-page). Per the delivery request,
this batch uses one fast lane after review instead of the full local suite.
Existing GitHub Actions remain unchanged. No feature toggle is added.

## Previous delivery — Wanted list controls

**Status:** ready to merge · adversarial review addressed · fast lane green ·
PR [#28](https://github.com/pjunod/monarr/pull/28) · **Updated:** 2026-09-19

| Item | State | Evidence |
|---|---|---|
| Missing/upgrade reason filter with counts | complete | `web/src/pages/Wanted.tsx` |
| Search across every visible field and target ID | complete | `web/src/wanted.ts` |
| Title/reason/type sorting and 25–500/all pagination | complete | shared `Pager` controls |
| Adversarial review | addressed | Persisted page clamp after live shrink; exposed current sort direction to assistive technology. |
| Fast lane | green | Production build/typecheck and 82 web tests passed on `fd34b44`. |
| Delivery | PR open | Forgejo has no Monarr repository, so the configured GitHub upstream is authoritative. |

## Progress at a glance

| Phase | Done | Definition of done |
|---|---|---|
| 0 — Walking skeleton | **17/17 ✅** | `docker run` → UI loads, status reports, tests pass |
| 0.5 — Tests, hooks, CI hardening | **7/7 ✅** | full pyramid green locally and in CI |
| 1 — Library | **13/13 ✅** | real media folders imported and browsable |
| 2 — Acquisition core | **16/16 ✅** | search → grab → import → correctly named file (movie + season pack) |
| 2.5 — Books (ADR 0006/0018) | **8/8 ✅** | keep and curate ebook + audiobook editions of one work |
| 3 — Automation | **9/9 ✅** | runs unattended for a month |
| 4 — Ecosystem compat | **5/6 ✅** (stretch deferred) | Jellyseerr/Prowlarr/Bazarr work against the shim |
| 5 — Depth & parity | **7/7 ✅** | custom formats, client zoo, lists, anime |
| 6 — Quality truth (ADR 0013/0014) | **9/9 ✅** | on-disk quality measured; target profiles; churn regression pinned |
| 9 — Discover (ADR 0015) | **7/7 ✅** | browse what's good and add it without leaving the app |
| Native mobile apps | **7/7 ✅** | daily workflows compile for native iOS + Android runtimes |
| Launch logistics | 4/9 | public repo, releases, name housekeeping |

## Native mobile apps ✅ (built 2026-08-01, v0.19.1)

- [x] One Expo/React Native project produces iOS and Android bundles
- [x] First-run setup automatically discovers and displays IPv4/IPv6 servers
      through DNS-SD on Wi-Fi, including a host-network companion for Docker;
      pairs by QR, or normalizes manual URLs
- [x] API key persists only in iOS Keychain / Android Keystore
- [x] Library/detail, item/profile/location edits, per-season monitoring,
      quality-copy management, search-now, Discover, and add flows
- [x] Wanted, Activity/queue progress and confirmed failed-row clearing,
      Calendar, System health, and web handoff
- [x] Per-device item sizing plus Auto/Light/Dark appearance; Auto follows the
      operating system and falls back to dark when its preference is unknown
- [x] Strict TypeScript and API/format/connection unit tests
- [x] Expo Doctor plus both native production bundles run in CI

Signed TestFlight and Play internal builds still require the release owner's
Apple/Google/Expo accounts and real-device QA; the code and build profiles are
ready, but credentials are deliberately not repository state.

## Phase 0 — Walking skeleton ✅ (shipped 2026-07-17)

- [x] Repo scaffold per blueprint §12 + GPL-3.0 license
- [x] Go module (`github.com/pjunod/monarr`) + Makefile build orchestration
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

## Phase 2.5 — Books (ADR 0006/0018) ✅ (shipped 2026-07-18; editions 2026-08-12)

- [x] Metadata bake-off → **Open Library** (open data, no key, work-level ids, covers; Google Books rate-limits anonymous callers, Hardcover needs an account) — `BookProvider` port + adapter, fixture contract-tested
- [x] Book external IDs end-to-end (isbn13 from editions, work OLID, asin column reserved) — unique `(kind, olid)`
- [x] Book-mode parser rules + golden corpus (12 cases seeded from Readarr/scene naming patterns; literal Readarr suite port remains an easy extension) — author extraction, order-swap tolerant matching
- [x] Book `SearchPlanner`/`ReleaseMatcher` (author+title queries); persisted edition type routes ebooks to Torznab 7000/7020 and audiobooks to 3030
- [x] Format families: PDF/MOBI/AZW3/EPUB ebooks plus MP3/WMA/AAC/OGG/Opus/M4A/M4B/FLAC/WAV audiobooks; seeded **Ebook** (id 4) and **Audiobook** (id 5) profiles with independent defaults
- [x] `{Author Name}/{Book Title}` naming, Calibre-friendly single files (`Title - Author.ext`) and stable multipart audiobook names (`… - 001.ext`); import + scan grade book files by extension
- [x] First-class Ebook/Audiobook add choices, library tabs, badges, same-family profile pickers, API `bookType`, and web/native curation — e2e covers one work holding EPUB + M4B together and a separate three-part M4B import
- [x] Side-by-side editions (ADR 0018): one Open Library work owns ebook and audiobook targets independently; API `bookTypes`, per-edition search/grab/wanted/import/files, add-the-missing-edition flows, and shared-folder scan attribution

## Phase 3 — Automation ✅ (shipped 2026-07-18)

- [x] Wanted index (in-memory, invalidated on add/scan/import/delete events, lazily rebuilt; in-flight downloads and pack-covered episodes excluded) — `GET /wanted`
- [x] RSS sync loop per indexer (15 min, jittered): fetch → parse → match wanted → decide → auto-grab
- [x] Backlog search (12 h + manual): planned queries per wantable, best accepted release grabbed, 20/run cap
- [x] Failed-download handling: blocklist (unique release+indexer, `GET/DELETE /blocklist`) + automatic re-search on client failure or import error
- [x] Calendar (API + UI): airing episodes + movie/book release dates, agenda view with -7/+30 day paging
- [x] Calendar rebuilt (2026-08-02, v0.20.0, [ADR 0016](docs/adr/0016-air-times.md)): month grid carries state as colour with a `+N more` day panel and a legend; new rolling **Agenda** view (sticky day headers, poster rows, `SxxEyy`, network, status pill) on web and native; real air times from TVmaze's show-level schedule composed to UTC per date; `/calendar` gains `airDateUtc`/`network`/`posterPath`/`season`/`episode`/`episodeTitle`/`runtime`/`monitored`, all optional, `detail` untouched
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
- [x] Access tab: user login, mobile pairing QR, and separately revealed API
      key; opt-in `authRequired` gate on `/api/v1` uses salted-hash credentials
      plus browser sessions and refuses to enable without credentials
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

## Phase 7 — The nzbd integration (plan: `docs/plan-integration.md`)

Making the three apps behave like one pipeline: events instead of timers at
every seam, and one id that names a transfer from grab to playable. nzbd's
side (N1–N7) is built; this is Monarr's.

- [x] **§5.1 native `nzbd` client** — `internal/adapters/nzbd`, a separate
      type from `nzbget` because it is a different API with different
      capabilities. Add / Statuses / Remove / Test against `/api/v1`, with
      post-processing reported as a named stage rather than a stall, and a
      job deleted by hand in nzbd reported as removed rather than as a bad
      release (which would blocklist it). Migration 0020 widens the type
      CHECK; UI, openapi and the web client types follow
- [x] **The transfer id** — `t-<downloads.id>-<6 hex>` on `downloads`,
      sent to nzbd as an add-time job param via the optional
      `ports.TaggedAdder` capability, and opening the handoff trace. The
      row is now inserted before the client add, because the id is built
      from the row id; a refused add cleans up its own row
- [x] **§5.2 push subscription** — `ports.Subscriber` + the nzbd SSE
      consumer (opaque `Last-Event-ID` resume, backoff reconnect, `reset`/
      `lagged` → reconcile by poll), a supervisor goroutine per push-mode
      client, `download_clients.mode` (`poll`|`push`, default poll), and a
      **Live updates** toggle that only appears for a client that can
      stream. Push runs beside the poll, never instead of it
- [x] **§5.3 one reconciler** — `reconcileDownload` extracted so poll and
      push share one brain, serialized per download id so the two channels
      cannot both import. The trace names the channel that delivered each
      step (`(event 913)` vs `(poll)`) — the difference between knowing
      push works and assuming it does
- [x] Verified against a **real nzbd**: a real download driven end to end
      while subscribed, receiving the actual `par_rename → rar_rename →
      post_unpack_rename` stages and a completion carrying the real final
      directory. The httptest fixtures are payloads captured from that run
- [x] **§5.4–5.5 import paths + the plurx notifier** — `ImportCompleted`
      carries the absolute paths that landed, the item's TMDB/IMDb ids,
      whether they are episodes, and the transfer id; book events also carry
      Curator's author, persisted medium, exact work/edition ids, and a
      strictly allowlisted Open Library cover URL. The three per-file
      importers return where each file went. The `plurx` notifier sits with
      Plex and Jellyfin in the import-only group but is not a refresh poke:
      it says "index this path, it is tmdb 949". Scoped `plx_` key, never an
      admin token. Retry covers the connection and 5xx and stops dead on
      401/403/422, and plurx's 422 — the path-mapping one everybody hits —
      is carried through verbatim with the roots it listed. Deliveries land
      recorded on a **persistent delivery queue** (`notifier_deliveries`,
      migration 24): first attempt immediately, retries at 5 s / 30 s / 2 m,
      terminal failures (401/403/422/404) not retried at all. The row is the
      point — the common miss is a host reboot where an in-memory retry dies
      with the process that owns it. The outcome lands on the download's
      handoff trace as a `notify_plurx` step, carrying plurx's own answer
      (`scanned → plurx item 1201`) via the optional
      `ports.DeliveryReporter`. `PUT /api/v1/notifiers/{id}` (edit in place —
      the log hangs off the id) and `GET .../deliveries` + a **Delivery log**
      on the notifier row
- [x] **§5.6 health checks** — one line per connection
      (`client:<name>`, `client:<name>:capacity`, `mediaserver:<name>`) via a
      new `RegisterGroup`, since what is worth checking is configured in the
      UI and not known when the registry is built. Probes run in parallel;
      a client that answers while nothing comes through it is reported at
      5 min / 30 min. nzbd capacity — low disk, quota, blocked servers,
      paused queue — via the optional `ports.CapacityReporter`, with an
      paused queue — via the optional `ports.CapacityReporter`. Chat
      notifiers and Plex/Jellyfin are listed but never probed: their only
      test is the action itself
- [x] **§5.7 Connections panel** — `GET /api/v1/system/connections` and a
      card on System: one row per remote app, `live | polling | degraded |
      unreachable | unprobed`, last-contact age and `event #913` for push
      clients, URLs redacted. Assembled from the last health probe plus the
      supervisor's live state rather than probing on demand, so the page
      does not become load on the servers it reports about. `polling` is
      styled as healthy, not as a lesser `live`: it is the fallback working
      as designed, and colouring it as a fault trains people to ignore the
      one that is

## Phase 8 — Plausibility (built 2026-07-27)

Real-world validation of the Phase 6 inference bands, which is exactly what
this section was waiting for — and the first thing it turned up was a hole.

A 500 MB file named `HIM (2025) [Remux 2160p].mkv` — a 96-minute feature —
imported, graded **REMUX 2160P**, and marked the movie satisfied. 500 MB over
96 minutes is 725 kbps: one sixtieth of a real 2160p remux, and less than the
TrueHD track it declares would need on its own.

The hole was specific. `Contradicts()` could only reject a remux claim when the
file had NO lossless audio, so declaring a TrueHD track made the claim
unfalsifiable — and a declared track is not a delivered one. Under that, every
band in `inference.go` is a lower-bound ladder bottoming out at
`SourceUnknown`, and `Resolve` accepts any filename token the measurements do
not actively contradict. "Inconclusive" and "obviously fabricated" reached the
same place: the name won.

The damage was never the badge. It was that a fake file **retired the want**.

- [x] `domain/mediainfo/plausible.go`: absolute bitrate floors per resolution
      tier (~3× below the lowest real encode), declared-audio-vs-total-bitrate
      contradiction, and measured-duration-vs-metadata-runtime. Skipped under
      60 s, where overall bitrate describes the container rather than the
      content — and a file that short is caught by the runtime rule anyway
- [x] `Contradicts()`: a remux claim below the bottom of the **encode** band is
      refused whatever the audio headers say
- [x] `ProvenanceImplausible`, distinct from `Failed`. Failed means we do not
      know, which is a reason to leave a file alone; this means we do know, and
      what we know is that it is not what it claims — a reason to keep hunting
- [x] `DiskStateForItem` + `episodeStates` skip implausible files, so the item
      goes back to wanted instead of sitting green over 500 MB. The row stays
      visible and stops voting
- [x] `SizeImplausible()` gates RSS and backlog before the grab; interactive
      search only warns. Gate the robot, never the person
- [x] Migration 0023: `media_files.source_release`/`source_indexer`, recorded at
      import — the link a blocklist needs, which used to live only in the
      queue row and only until it moved on
- [x] `DELETE /library/{id}/files/{fileId}` (`fromDisk`/`blocklist`/`search`) +
      Files-table row actions; the interactive search window pages, filters and
      collapses its rejections
- [x] `phase9b-plausibility.spec.ts` — 6 e2e covering the whole chain; 80/80 green

**Follow-ups from the first week in the field (v0.8.1):**

- [x] Apostrophes are elision, not word breaks. `NormalizeTitle` spaced every
      non-alphanumeric run, so "The Carpenter's Son" read as "carpenter s son"
      against a release's "carpenters son" and forty candidates came back
      "does not match". Accents now fold to ASCII and the three spellings of
      an ampersand unify, both the same class of bug
- [x] `place()` treated ANY file at the destination as already imported, so a
      same-quality replacement copied nothing and the row was rewritten from
      the OLD file's size — re-grabbing a bad file could never fix it. Only
      the same inode counts now; everything else is replaced through a temp
      name and an atomic rename
- [x] A movie payload with several video files took whichever sorted first
      alphabetically. It takes the largest
- [x] `short_delivery`: the advertised size was stored at grab time and never
      compared to what arrived. Without it "a 500 MB fake" and "a 13 GB
      release that arrived 500 MB short" are indistinguishable and want
      opposite responses
- [x] **Truncation detection**: a Matroska Segment states its finished length,
      so a file smaller than its own declaration is provably incomplete —
      not an inference, arithmetic on two known numbers. This is the rule that
      separates a fake from a cut-off download, which no bitrate threshold can
      do: a fake is built from a real release's header, so it carries the same
      tracks, chapters and stated runtime. Validated against the fixture
      corpus, every file of which is a truncated real file by construction
      (`SIZES.txt` records each original), plus two new whole controls
- [x] A provably cut-short payload is REFUSED at import rather than placed.
      The one refusal in the import path, because it is the one case that is
      proof and not judgement — and because placing it loops: the item returns
      to wanted, the next pass grabs, lands another stump, repeat. Not
      blocklisted; the release was never the problem

- [x] **Cleanup after import**: monarr never removed a payload from the
      download client, so every grab left a second full copy behind — real
      bytes, not a hardlink, whenever the download disk and the library are
      different filesystems. A user found 923 GB of it. Per-client
      `remove_completed` (migration 0025), defaulted ON for usenet and OFF for
      torrents, which are still seeding; removal runs inline at import and an
      hourly `downloads.cleanup` sweep drains the backlog that accumulated
      before the setting existed

**Field diagnosis, 2026-07-27 — the exactly-500 MiB movies are TRUNCATED, not
fake.** `ffprobe` on HIM (2025): "File ended prematurely", last video packet at
61.6 s of a 96-minute feature. 500 MiB over 61.6 s is ~68 Mbps, which is
exactly a 2160p DV remux — the content is genuine and the transfer stopped.
Two files measured: 523,975,057 and 523,967,206 bytes, both 499.70 MiB, 7.8 KB
apart. Just under a round 500 MiB with a few KB of per-release variance is a
500 MiB cap on the DOWNLOADED data, the extracted MKV coming out under it by
rar/par overhead. Upstream of monarr — nzbd or the provider — and still open

### Phase 8 — watched → monarr (master plan §11.1)

- [x] **`POST /api/v1/webhooks/plurx`** — matched by external id only, never
      by title; an unknown item is 200 `matched:false`, not a 404, because
      plurx retries failures and there is nothing to retry about a film
      Monarr never managed. Migration 26 records it per plurx user, thin by
      design (who, what, when), replacing on rewatch so the table is bounded
      by library size rather than by television
- [x] **Backlog prioritization** — anything watched in the last 30 days is
      searched first. The per-run cap makes that decisive rather than
      cosmetic: on a large backlog the order decides what gets searched at
      all. A stable partition, not a score — the signal is "somebody watched
      this", and a numeric intensity would be precision nobody could check
- [x] Decisions recorded in `docs/plan-integration.md` §11.1: per-user with
      usernames (overriding the sketch's aggregate), and **no deletion path
      on either side**
- [ ] Show it on the item page — the data is stored and read, but not yet
      rendered anywhere

## Phase 9 — Discover (ADR 0015, built 2026-07-29)

Plan: [docs/plan-discover.md](docs/plan-discover.md) · decision:
[ADR 0015](docs/adr/0015-discovery.md).

Monarr could find anything you could already name and nothing you could
not: every path into the library started at a search box. The answer to
"what should I watch" lived in a browser tab, and the round trip back was
copy-paste-a-title.

- [x] `ports.DiscoverProvider` — a provider publishes its own **catalogue**
      (`Lists`), says whether it can serve it (`Configured`), and serves one
      page of one row (`Discover`). The catalogue-first shape is what makes a
      second provider additive: Trakt's rows appear because Trakt names them,
      not because the UI learned about Trakt
- [x] **TMDB, nine rows, no setup** — trending (movies and shows), in
      theaters, coming soon, on the air, popular ×2, top rated ×2. Reached
      through the existing rate-limited, cached request path; the import
      lists' `DiscoverMovies` re-expressed over the same helper, signature
      untouched, because its vocabulary is stored in `import_lists.type`
- [x] **Trakt, five more, optional** — being watched right now (×2), most
      anticipated (×2), box office. Kept separate rather than merged because
      they answer a different question: Trakt counts scrobbles, TMDB counts
      lookups, and a blended ranking would be unverifiable. The adapter
      gained what it never had — a `KeyFunc` so the id comes from settings,
      a rate limiter, and a response cache keyed on id **and** URL, since the
      id is a header and a rotated one must not be served the old answer
- [x] `internal/app/discover` — 30 min fresh, **stale served for up to 24 h
      when upstream fails**, then the error. A page that goes blank because
      TMDB had a bad thirty seconds reads as a bug; a day-old row is not
      "less fresh", it is wrong. Artwork for providers that hand out bare ids
      is hydrated by the new shallow `tmdb.Summary` at concurrency 6 — never
      `GetSeries`, which would fetch every season of every show for a poster
- [x] `GET /discover/lists` + `GET /discover/items?list=&page=`; an empty
      catalogue is a **200 with an empty array**, not a 503, because "no
      provider configured" is a setup screen and not a failure. `inLibrary`
      answered by a new narrow `TmdbIDsByKind` query rather than the full
      `Library.List` the search handler uses — fourteen rows a page load is
      where that would have become expensive
- [x] `/discover` page: lazy rows (an `IntersectionObserver` 400 px ahead, so
      nothing below the fold is fetched), horizontal strips with desktop-only
      arrows, All/Movies/Shows tabs, and an add dialog that is a second door
      into `library.Service.Add` — never a second policy
- [x] **Poster size (S / M / L, labelled)** in the page head of Discover
      *and* the Library page. The label is on screen and is also the group's
      accessible name, so the two cannot drift; `.page-head` now wraps at
      every width, because the picker was what tipped Library past a 1000px
      window and nowrap crushes the actions rather than moving them. One `--card-w` token on `<html>`, so two components that
      must agree on a number share an attribute rather than state, CSS
      resizes the grid without re-rendering it, and a pre-paint script keeps
      the library grid from reflowing a frame after it draws. M is exactly
      what both were before, so an untouched install is unchanged
- [x] Discover strips show their scrollbar only under the pointer, with its
      space reserved at all times — fourteen rows that each grew 8 px on
      hover would shove the page around under the cursor
- [x] `zz-discover.spec.ts` (8 e2e) + `zz-cardsize.spec.ts` (2, measuring
      real geometry) + unit tests for both adapters and all four cache
      behaviours; **90/90 e2e green**

**Two pre-existing failures on `main` repaired to get there** — both test-
or CSS-only, no behaviour change, and neither caused by this work:

- [x] `smoke.spec.ts` asserted that any health check beyond a fixed five must
      be named `client:*`. The `imports` check (commit 719892b) was never
      added to that list, so main was red. The list is now written once and
      used twice — the drift was two copies of it
- [x] `phase9-layout.spec.ts` was failing on 156 px queue rows. The Activity
      rewrite (68575a3) widened the progress column to 210 px, and with every
      other column at a stated minimum the release title is the one on
      `width: auto` — it was left about 60 px and wrapped a scene name **one
      character per line**. `col-actions` 268 → 190: buttons wrapping to a
      second line is a far smaller cost than an unreadable title

## Launch logistics

- [x] `monarr-media` GitHub org (free) + private repo + initial push
- [x] **Moved to `pjunod/monarr`; module path renamed to match** (2026-07-29).
      The repo left the org, and GitHub forwards traffic from a renamed
      repository only until someone claims the old name — so the redirect was
      never something to build a module path on. Everything is `pjunod/monarr`
      now: `go.mod`, 242 files of imports, `arch_test.go`'s module constant,
      both sets of LDFLAGS, the ghcr.io image, the four README badges, the
      clone URL in usage.md. Done while nothing outside the repo depends on the
      old path, which is the only time this is a cheap change — ADR 0001
      amendment 2
- [x] Container registry decision: ghcr.io (no Docker Hub subscription — ADR 0001 amendment)
- [ ] File dormant-username request with GitHub Support for `monarr`
- [ ] Register a domain
- [ ] Flip repo public + branch ruleset requiring the four CI checks
      — **when this happens, the orphan `badges` branch can be deleted**; the
      coverage badge no longer uses it (see below)
- [ ] goreleaser: tagged releases with binaries + ghcr.io images (target: end of Phase 1)
- [x] **Coverage at 86.5%, floor enforced at 85%** (2026-07-29). Was 68.4%
      against no floor. The gap was concentrated, not spread: `internal/api`
      held 1,126 of the 3,061 uncovered statements while every domain package
      was already 85–100%, and the reason was structural — `newTestServer`
      wired four dependencies, so every handler reading Store/Library/
      Acquisition/Discover was unreachable from a test. The harness had to
      exist before the tests could. `COVERAGE_MIN` + `scripts/coverage-gate.sh`
      now fail the build below 85% on every push, in `make coverage` and in CI
      after the per-package summary is written. Two things deliberately left
      under: `cmd/monarr` (204 statements of `main()` — needs a `run(ctx, cfg)`
      extraction, worth doing on its own) and the storage-failure branches in
      `internal/api`/`internal/app/library`, which only fault injection reaches
- [ ] Extract `run(ctx, cfg) error` from `cmd/monarr/main.go` so the
      composition root can be started and shut down in a test — the last
      package at 0%, and the only one whose coverage needs a refactor rather
      than a test
- [x] **Coverage badge renders while the repo is private** (2026-07-29). It
      pointed at `raw.githubusercontent.com/…/badges/coverage.svg`, and GitHub
      fetches an absolute image URL through its proxy *anonymously* — so on a
      private repo it 404s. The four workflow badges are served by github.com
      itself and honour the viewer's session, which is why only this one was
      broken. `coverage.svg` is now tracked on main and linked relatively,
      which is served the same authenticated way and keeps working after the
      repo goes public; CI commits it back only when the number moves,
      rebuilding on the current tip so it can never revert a real commit.
      Generation was already local (`scripts/coverage-badge.sh`, no Codecov)
      and is unchanged

## Activity: bounded, paged, sweepable ✅ (2026-07-31, v0.15.0)

The page rendered `ListRecentDownloads` — the last 100 rows, every state, one
table — and nothing in the schema had ever deleted a download row or a history
event. "Finished" was therefore a section that only grew, and the table under
it grew for the life of the install.

- [x] **Retention.** `activity.retention`, daily: terminal rows and history
  events older than the window are deleted. Window is a setting
  (Settings → Library, `activity.retention_days`), default 30 days, `0` keeps
  everything. Rows that are still moving are never swept, at any age.
- [x] **Paging and filtering.** `GET /queue?filter=active|imported|failed&q=&limit=&offset=`
  (no params = the old behaviour, byte for byte) and `GET /queue/summary` for
  the section counts, so a collapsed "Finished (1,284)" costs one aggregate
  instead of 1,284 rows. `GET /history` gained `offset` and now pages in SQL
  rather than filtering a fixed 200 in Go.
- [x] **The page.** Active work always whole and never paged; Finished and
  Failed collapsed to a count, opened deliberately, 25 rows at a time. Title
  filter across all groups. **Clear finished** and **Clear failed** (rows only,
  both confirmed) plus the existing per-row Remove as a dismiss.
- [x] Four e2e specs cover the page: three follow imported rows behind the
  collapsed section, and one pins the confirmed Clear failed action to the
  Failed grouping without disturbing Clear finished.

## The re-grab loop — monarr's half ✅ (fixed 2026-07-31, v0.14.0)

Field report 2026-07-31: a terabyte of duplicate downloads. Diagnosed live
against nzbd (`nzbd/docs/REGRAB_LOOP_PLAN.md`); four defects feeding one
loop, of which these four are monarr's (M1–M4 of that plan).

- [x] **M1 — an import that runs out of disk fails, visibly, and finishes
  itself later.** Per-file copy errors were recorded and stepped over, and
  an import that placed *some* files reported success — so a season pack
  imported its first half, the second half stayed listed as missing, the
  payload was cleaned up, and the next backlog pass grabbed the same pack
  again. Now an environmental failure (ENOSPC/EDQUOT, read-only or vanished
  mount) stops the import at the first occurrence, marks the download
  failed with `import stopped: N of M files placed`, leaves the payload
  alone, and a `downloads.import-retry` sweep finishes it once there is
  room — every 15 min, six attempts, then it waits for a person. Only
  environmental failures retry; a payload monarr cannot parse fails the
  same way forever.
- [x] **M2 — cleanup no longer takes the client's word for it.** When the
  download client will not or cannot delete an imported payload, monarr
  removes the completed folder itself — the folder it just imported from —
  and records it (`payload_removed`). Guarded: only the directory the
  client reported, only when "clean up after import" is on for that client,
  and never at, above, or inside a root folder.
- [x] **M3 — the same release is not grabbed twice, and replacement is
  bounded.** `Grab` now refuses a release already in flight (titles
  compared with separators folded) instead of relying on the in-flight
  filter at the automation entry points, which a manual grab, an overlapping
  pass, and an instant post-failure re-search all walked straight past. And
  the automatic replacement search stops after three failed grabs for one
  want inside twelve hours, recording `regrab_capped` — five releases of one
  movie in ten hours was the field shape.
- [x] **M4 — the pipeline is readable.** `history_events` had been written
  since phase 2 and read by nothing: `ListHistory` had no caller outside its
  own package. `GET /api/v1/history?mediaItemId=&limit=` now serves the
  per-release timeline — grab → client outcome → import result, plus the new
  `import_blocked` / `import_retried` / `regrab_capped` events. (UI panel
  not built yet; the data is now reachable.)

## Defects the coverage work found ✅ (fixed 2026-07-29, v0.12.0)

Writing tests for the handlers that had never been reachable turned up six real
defects, not one of them theoretical. Each has a regression test in
`internal/api/defects_test.go`, and each of those was checked to actually fail
with its fix reverted — a test that passes either way is not a regression test.

- [x] **A permanent auth lockout.** Enabling authentication checked for a
      username and never looked at the password, but `Login` refuses on
      `wantUser == "" || wantHash == ""`. A username with an empty password
      therefore turned auth ON and then answered every login attempt with "no
      credentials configured" — forever. On an install with no API key set, the
      only way back in was editing `app_meta` by hand. The guard now reads both
      settings, spelled the way `Login` spells them, so the two cannot drift
- [x] **The mass editor counted rows it had not changed.** `UPDATE … WHERE
      id = ?` succeeds against nothing, so `POST /library/bulk` answered
      `{"updated": 1}` for a library of none. The query is `:execrows` now and
      the store reports `ErrNotFound`
- [x] **Three refusals answered 500.** A relative manual-entry path, an unknown
      root-folder kind, and a blank manual title (that one wrapped in
      `ErrNotFound`, so it answered 404 — "the endpoint does not exist" for a
      short body). None carried a sentinel, so `libraryErr` reported a server
      fault for a typo. They are one fact about the caller, so they share one
      new sentinel, `library.ErrInvalidInput`, matched once
- [x] **Deleting a profile some kind defaults to answered 500.** The same class
      of refusal as deleting one still in use — a live reference the caller must
      clear first — but only `ErrProfileInUse` was mapped, so an actionable
      message arrived behind a status code that said the server broke.
      `ErrProfileIsDefault` now joins it as a 409
- [x] **Import lists never validated their type.** oapi-codegen already
      generates `Valid()` from the spec's enum and five sibling creates call it;
      this one did not, so a list of a type no syncer knows about stored
      happily, appeared in the UI, and silently never synced — `syncOne` answers
      "unknown list type" into a log nobody reads
- [x] **DELETE of an absent id answered 204 on five endpoints and 404 on
      seven**, and the spec documented 404. The five were both inconsistent and
      untrue: they reported removing something that was never there. All five
      queries are `:execrows` now, the spec documents their 404, and the compat
      personality's own "indexer not found" branch — written from the start and
      until now unreachable, because the store made delete a silent no-op —
      finally fires
- [x] **`make coverage` reported a number that depended on build-cache state.**
      It did not pass `-count=1`. `go test` caches coverage profiles, and with
      `-coverpkg=./...` a cached package's profile still carries blocks for every
      *other* package, measured against that package's source as it was when the
      entry was cached. Edit a file, re-run, and the merge sees two disjoint sets
      of line ranges for it: the denominator inflates and the percentage
      collapses. It read 71.9% against a true 86.5% — caught because the drop was
      too large to believe, confirmed by measuring `internal/api` alone (1,599
      statements versus 2,736 in the poisoned profile). CI was always immune (a
      fresh checkout has no cache); a laptop was not, and a gate that reports a
      cache-dependent number is worse than no gate
- [ ] Left deliberately: an `internal/infra/jobs` backoff that overflows at
      absurd attempt counts. Latent — unreachable in practice — and it needs its
      own decision about what the ceiling should be rather than a quick clamp

## Auto Search did nothing ✅ (fixed 2026-07-30)

Reported from the real deployment: pressing **Auto search** on a movie appeared
to do nothing, twice; interactive search on the same movie then listed copies,
and grabbing one from that list started downloading immediately. So releases
existed, the profile would take one, and the automatic path still declined to
act. Two defects — one causing it, one hiding it.

- [x] **A terminal download row counted as "in flight" forever.**
      `notInFlight` — the filter every search path runs its wantables
      through — read `ListInFlightDownloads`, which was
      `SELECT * FROM downloads WHERE state != 'imported'`. That is every row
      that ever failed. Three paths park a row at `failed` and none of them
      ever move it out: `handleFailure` (the client said the download broke),
      `handleRemoved` (the user deleted the job in their client), and a failed
      import handoff. Only the user deleting the row from the queue clears it.
      So a single failure, at any point in the past, made that wantable
      invisible to Auto Search, the 15-minute RSS sync, **and** the 12-hour
      backlog pass — permanently and silently. `handleRemoved`'s own comment
      promises the opposite ("the want stays wanted… the next deliberate search
      still finds it"); it did not. The suppression was redundant besides: a
      release that fails is blocklisted, which is what stops us re-grabbing
      *that* copy, while retiring the whole wantable stopped us grabbing a
      different one — the exact thing that should happen next. `notInFlight`
      now reads `ListActiveDownloads` (grabbed · downloading · downloaded ·
      awaiting_import · importing), so only work that is still moving suppresses
- [x] **The endpoint could not report any of this.** `POST
      /library/{id}/autosearch` answered a bare `202` and ran the search in a
      goroutine, so the UI printed "Searching in the background — grabs appear
      under Activity" whether the search grabbed a remux, declined everything
      it found, or never ran at all. That is why a total no-op went unnoticed
      for as long as it did. It is synchronous now and returns
      `AutoSearchResult`: every target, searched or skipped-and-why, with the
      funnel that makes the outcome diagnosable — releases **seen**, of those
      **matched** to this target, of those **accepted** by the profile.
      Whichever number went to zero first is the answer, and "no releases came
      back", "none of them were this film" and "the profile turned all of them
      down" are now three different sentences. Interactive search already
      blocked on the same fan-out against the same indexers, so there was never
      a latency argument for the 202 — only the assumption that there was
      nothing worth saying. Search-on-add stays fire-and-forget: nobody is
      waiting on that one
- [x] The indexer fan-out in `searchAndGrabBest` is concurrent now, as it long
      has been in interactive search. Serial was tolerable when only the
      nightly backlog called it; with a person waiting and a 30 s per-call
      bound, four indexers meant a two-minute worst case
- [x] Regressions in `internal/app/acquisition/autosearchreport_test.go` (a
      failed download no longer suppresses; an *active* one still does) and
      `web/src/autosearch.test.ts` (the three no-grab outcomes must read
      differently)

## Decisions to date

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-name-and-license.md) | Name **Monarr**, **GPL-3.0**, port **7676**; amended twice: images on ghcr.io, org/module now `pjunod/monarr` |
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
| [0015](docs/adr/0015-discovery.md) | Discovery is a read-only browse surface: TMDB always, Trakt when keyed, nothing stored |
| [0016](docs/adr/0016-air-times.md) | Calendar schedule enrichment carries exact UTC air times where providers know them |
| [0017](docs/adr/0017-audiobook-curation.md) | Audiobook format routing and multipart import; profile-derived identity superseded by ADR 0018 |
| [0018](docs/adr/0018-side-by-side-book-editions.md) | Ebook and audiobook are persistent, independently curated editions of one Open Library work |

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
