# Monarr — Project Status

> **Snapshot 2026-07-29 · v0.11.0 · Phase 9 "Discover" built: rows of trending / in theaters / coming soon / top rated, read live from TMDB (nine rows, no setup) and Trakt (five more, behind an optional client id), added to the library without leaving the page. No table, no job, nothing stored. Poster size is now S/M/L on both Discover and the Library grid. Two pre-existing red e2e specs on main repaired on the way through (§Phase 9). Full gate green, 90/90 e2e · next: launch logistics + the rest of the nzbd integration**
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
| 9 — Discover (ADR 0015) | **7/7 ✅** | browse what's good and add it without leaving the app |
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
      whether they are episodes, and the transfer id; the three per-file
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
- [x] **Poster size (S / M / L)** in the page head of Discover *and* the
      Library page. One `--card-w` token on `<html>`, so two components that
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
| [0015](docs/adr/0015-discovery.md) | Discovery is a read-only browse surface: TMDB always, Trakt when keyed, nothing stored |

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
