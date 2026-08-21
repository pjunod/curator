# Plan — tightening the nzbd ↔ monarr ↔ plurx pipeline

**Status:** ready to build · **Written:** 2026-07-26 ·
**Companions:** `nzbd/docs/INTEGRATION_PLAN.md`, `plurx/docs/INTEGRATION-PLAN.md`

**Amended 2026-08-21:** Cinema now has first-class Books libraries. The
original no-books boundary recorded below is historical; §5.5 and §10 now
require book imports to send an exact-path targeted scan with `hint:"book"`
and a separate explicit work/edition object, with no movie/series provider
ids.

This is the canonical plan for making the three apps behave like one
pipeline: monarr grabs, nzbd downloads and unpacks, monarr imports, plurx
scans and plays — with events instead of timers at every seam, and a
visible trace at every hop.

How to work from this document: build one repo at a time, in the phase
order of §7. Monarr's work packages are §5 of this file; nzbd and plurx
each carry a self-contained plan in their own repo (named above) that
restates exactly the slice of the contract they implement, so a session
opened in either repo needs nothing else. Every identifier in §2–§3 was
copied from the code on 2026-07-26 and is marked with the file it came
from — re-verify against that file before building on it, in case it
moved. If a step seems to require doing something §10 forbids, stop and
flag it instead of improvising.

---

## 1. Objective — every handoff pushed, every hop visible

Today the seams between the apps are timers: monarr polls nzbd's
nzbget-compat API every 30 s and infers completion from history rows, and
plurx never scans at all unless someone presses the button (it has no
scheduled scans — see §2.3). The work is two pushes and one principle:

1. **nzbd → monarr:** monarr subscribes to nzbd's existing SSE stream,
   which gains the events it is currently missing (post-processing
   stages, and a completion event that carries the final directory).
   Polling remains as a self-healing fallback, and the choice is a
   per-client option in monarr's UI.
2. **monarr → plurx:** when monarr finishes an import it tells plurx to
   scan exactly the imported folder, hands over the TMDB/IMDb ids so
   plurx skips fuzzy title matching, and records plurx's answer (item
   id, files added, or the error) in its own handoff trace.

**The no-black-box rule** that shapes every decision below: every
download carries one transfer id from grab to playable; every hop appends
to a trace a human can read; every push channel has a poll fallback that
still works alone; and every "it didn't happen" has a place where the
reason is written down. §8 is the inventory of those places.

```
┌────────┐ 1. grab: POST /api/v1/jobs?url=…&category=…          ┌──────┐
│ monarr │ ────────&params={"monarr-transfer":"t-42-a3f9c1"}───▶│ nzbd │
│  :7676 │                                                      │:6789 │
│        │ ◀─ 2. SSE /api/v1/events: tick · job_pp_stage ───────│      │
│        │      · job_pp_finished{final_dir, history_seq}       └──────┘
│        │ ◀─ 2b. fallback poll: GET /jobs · /history?since_seq
│        │
│        │    3. import: hardlink/copy into the library root
│        │
│        │ 4. POST /api/v1/scan {path, ids, correlation_id} ──▶┌───────┐
│        │ ◀─ 5. {item_id, report} ─▶ written into the        │ plurx │
└────────┘      download's handoff trace                       │:32600 │
                                                               └───────┘
```

Arrows point in the direction of the HTTP connection: **monarr is the
only client**. nzbd never learns monarr's address; plurx never learns
nzbd exists. That keeps nzbd and plurx free of consumer configuration and
puts all pairing config on monarr's settings pages, where the rest of the
client config already lives. (The two later-phase features — plurx-side
watched signals and the coming-soon rail — are the only exceptions, and
they are sketched separately in §11.)

---

## 2. What exists today (verified against the code, 2026-07-26)

### 2.1 nzbd (Rust, axum, one port for UI + API + compat)

- Native API under `/api/v1` (`crates/nzbd-api/src/lib.rs`), auth via a
  single `Authorization` header — `Basic user:pass` or `Bearer <token>`
  (`[api] username/password/token` in `nzbd.toml`). No query-param auth.
- SSE already exists: `GET /api/v1/events` streams `job_added`,
  `job_finished`, `job_deleted`, `file_finished`, `segment_exhausted`,
  `server_blocked`, `queue_pause_changed`, `speed_limit_changed`,
  `job_assigned`, plus a 1 Hz `tick` carrying `{status, jobs}` and a
  `lagged{skipped}` signal. **No `id:` field, no `Last-Event-ID` resume.**
- The event enum lives in `crates/nzbd-engine/src/events.rs`
  (`tokio::sync::broadcast::channel(512)`). **`job_finished` fires at
  download completion, before post-processing.** PP runs in
  `crates/nzbd-post/src/manager.rs` and finishes silently: it writes a
  `HistoryEntry` (with `final_dir`), stamps the `*PP:done` param
  (`SUCCESS | PAR_FAILURE | UNPACK_FAILURE | SCRIPT_FAILURE`), then
  `remove_job_silent`. No event carries a path today.
- History: append-only JSONL + SQLite index
  (`crates/nzbd-state/src/history.rs`). `GET /api/v1/history?limit=` only
  — no `since` cursor. Rows already track handoff observation:
  `first_seen_at_unix`, `seen_count`, `picked_up_by` (populated only for
  compat-API callers via their User-Agent).
- Compat layer emulates NZBGet JSON-RPC/XML-RPC (`nzbd-compat`) — this is
  what monarr speaks today, via its `nzbget` adapter.
- `StatusDto` already exposes `disk_low`, `quota_reached`,
  `blocked_servers`, `health_abort` — the capacity facts monarr wants for
  health warnings exist; nothing consumes them.
- **Known lie:** `[[category]] dest_dir/unpack` are parsed and projected
  to compat clients as `CategoryN.DestDir`, but the PP manager always
  writes to `dest_dir/<sanitized job name>`. Advertised paths ≠ actual
  paths. Fix is in scope (nzbd plan §N6) because monarr path-maps off
  reported paths.

### 2.2 monarr (Go, stdlib mux, oapi-codegen; React UI)

- Download clients implement `ports.DownloadClient`
  (`internal/ports/downloadclient.go`): `Add / Statuses / Remove / Test`.
  `Statuses(ctx)` is a whole-client poll; there is no per-handle or
  subscribe method. Tracking runs from scheduler task `queue.refresh`
  every `30 * time.Second` → `RefreshQueue`
  (`internal/app/acquisition/acquisition.go`).
- The handoff pipeline is already trace-shaped:
  `downloads.handoff_log` (migration `0012_handoff.sql`), steps
  `grabbed → downloading → downloaded → awaiting_import? → importing →
  imported | failed`, appended via `advance(...)`
  (`internal/app/acquisition/handoff.go`) and rendered in
  `web/src/pages/Activity.tsx`. This is the spine we extend — not
  replace — with nzbd stage detail and plurx scan results.
- Events: in-process bus (`internal/infra/bus/bus.go`), re-broadcast to
  the UI via `GET /api/v1/events` SSE. `ImportCompleted` (declared in
  `internal/app/acquisition/acquisition.go`, published from
  `import.go:174`) carries
  `{MediaItemID, Release, Files, Upgrade}` — **no file paths, no ids** —
  so a targeted "scan exactly this" call is impossible today.
- Notifiers (`internal/app/notify`, `internal/adapters/notify`): types
  `webhook | discord | plex | jellyfin` (DB CHECK constraint in
  `0005_automation.sql`), `refreshOnly()` hardcodes plex/jellyfin to
  import-only pokes. Delivery is serial, 20 s timeout, **no retry, no
  delivery log** — a plurx that is restarting would silently miss a scan.
- Health registry exists (`internal/app/health`) with checks for db,
  data dir, web ui, library folders, metadata provider. **No download
  client or media server reachability checks.**

### 2.3 plurx (Rust, axum; Plex-compat façade; embedded web UI)

- Scans are triggered only by library create/update or
  `POST /api/v1/libraries/{id}/scan` (admin). **No scheduled scans, no
  watcher** (the `notify` crate is in the workspace deps, used by
  nothing). A trigger during a running scan is **dropped**, not queued
  (`JobManager::trigger`, `crates/plurxd/src/state.rs`).
- A scan always re-walks every root of the library
  (`scan::scan_library_with_progress`, `crates/plurx-core/src/scan/mod.rs`)
  — there is no path-targeted entry point. Unchanged files are skipped by
  size+mtime, but the walk itself is the cost on a big NAS.
- Auth is user login tokens only (`Authorization: Bearer`, `?token=`,
  `X-Plex-Token`); the only privilege tier is `is_admin`. A caller that
  can trigger scans can also read TMDB/OMDb/Trakt secrets back out of
  `GET /api/v1/settings`. No API-key concept exists.
- Metadata matching is title+year fuzzy (`tmdb.find_movie/find_show`).
  There is **no channel to hand plurx an id**: `PATCH /items/{id}`
  refuses non-home libraries, and the NFO parser deliberately ignores
  `<uniqueid>`. `MetadataPatch` already has `tmdb_id`/`imdb_id` fields —
  the write path exists, only the entry points are closed.
- Config TOML is `deny_unknown_fields` (13 lines); runtime settings live
  in the DB behind `GET/PUT /api/v1/settings`. New knobs go in the DB,
  not the TOML.
- At this plan's 2026-07-26 baseline library kinds were
  `movies | shows | home`. Cinema added first-class `books` libraries before
  the 2026-08-20 amendment, so book imports now use the targeted-scan seam.

### 2.4 The four load-bearing gaps

| # | Gap | Where it bites |
|---|---|---|
| 1 | nzbd emits no event at PP completion, and no event carries `final_dir` | monarr can only discover a finished download by polling history |
| 2 | monarr's `ImportCompleted` carries no paths or ids | nothing downstream can be told *what* landed *where* |
| 3 | plurx has no targeted scan, no machine auth, no id intake | "scan it now" is impossible; matching stays fuzzy |
| 4 | no shared identifier crosses any seam | a stuck transfer can't be traced without reading three logs |

---

## 3. The contract

Everything below is normative for all three repos. Payload field names
are exact. Copies of the relevant slices live in the per-repo plans;
if a conflict is found, this file wins and the per-repo copy is the bug.

### 3.1 The transfer id

Minted by monarr at grab time, one per download row:

```
t-<downloads.id>-<6 lowercase hex>        e.g.  t-42-a3f9c1
```

Where it lives: in nzbd as job param `monarr-transfer` (set at add time,
§3.4, therefore visible in nzbd's queue UI, history rows, and compat
`Parameters`); in monarr on the download row and in every handoff-log
entry it writes for that download; in plurx as `correlation_id` on the
scan request, echoed in the scan response, logged under the
`plurxd::integrate` tracing target, and kept on the stored scan-request
record. Grepping any one app's log for `t-42-a3f9c1` finds the transfer;
that is the point.

### 3.2 nzbd event-stream additions

Two new events in the `Event` enum
(`crates/nzbd-engine/src/events.rs`, wire format
`#[serde(tag = "event", rename_all = "snake_case")]`):

```jsonc
// on every post-processing stage transition
{ "event": "job_pp_stage", "job": 7, "name": "Show.S01E01…",
  "stage": "par_verify" }
// stage ∈ par_rename | par_verify | par_repair | rar_rename | unpack |
//         cleanup | move | post_unpack_rename | script
// (snake_case of PostStage in nzbd-types; re-verify variant list)

// after PP finishes AND the history row is durably written
{ "event": "job_pp_finished", "job": 7, "name": "Show.S01E01…",
  "category": "tv", "pp_status": "SUCCESS",
  "final_dir": "/downloads/complete/Show.S01E01…",
  "size_bytes": 1234567890, "health": 1000,
  "params": [["monarr-transfer","t-42-a3f9c1"]],
  "history_seq": 913 }
// pp_status ∈ SUCCESS | PAR_FAILURE | UNPACK_FAILURE | SCRIPT_FAILURE
// (the existing PpFinal::as_str() values — do not invent new ones)
```

**Ordering guarantee (normative):** `job_pp_finished` is emitted only
after `HistoryDb::record` returns, so a consumer that reacts to it and
immediately calls `GET /api/v1/history?since_seq=<history_seq - 1>` will
see the row. Emission happens in the finalize block of
`process_job_ctx` (`crates/nzbd-post/src/manager.rs`).

**SSE resume protocol:** every SSE frame gains an `id: <seq>` line — a
monotonically increasing u64, process-lifetime, also embedded in the
JSON as `"seq"`. The server keeps a ring of the last 1024 events; a
client reconnecting with `Last-Event-ID: <seq>` gets the missed tail
replayed. If the requested seq has fallen out of the ring (or the server
restarted — seq resets), the server sends `event: reset` with
`{"reason":"gap"}` as the first frame, and the client must do a full
poll reconcile (`GET /api/v1/jobs` + `GET /api/v1/history?since_seq=`)
before trusting the stream again. `tick`/`hb`/`log` frames are
stream-local and carry no `id:`.

### 3.3 nzbd history cursor

`HistoryEntry` JSON gains `"seq"` — the SQLite rowid, monotone per node.
New query form:

```
GET /api/v1/history?since_seq=907&limit=100
→ rows with seq > 907, ascending seq order
```

The existing `?limit=` form (descending, for the UI) is unchanged. This
is the catch-up path after monarr downtime and the poll path's way to
fetch only news. Reason it exists: SSE alone is lossy by design
(broadcast ring); the cursor makes loss harmless.

### 3.4 nzbd native add: params and attribution

`POST /api/v1/jobs` (`AddJobQuery` in `crates/nzbd-api/src/lib.rs`)
gains an optional query field:

```
params = URL-encoded JSON object of string→string
         e.g. params=%7B%22monarr-transfer%22%3A%22t-42-a3f9c1%22%7D
```

Keys must not start with `*` (that prefix is reserved for nzbd-internal
params like `*PP:done`); reject with 422 if they do. Params flow into
`Job.params` and from there into history rows and compat `Parameters`
automatically — no second write path.

Attribution: native-API calls send `X-Nzbd-Client: monarr/<version>`
(the header the UI already uses for pause attribution). nzbd's
`ClientRegistry` — today populated only by compat calls — also notes
native-API and SSE callers, so `GET /api/v1/clients` and history
`picked_up_by` work regardless of which API the consumer speaks.

### 3.5 plurx: scoped API keys + targeted scan

**Keys.** New concept, stored like user tokens (SHA-256 of the secret;
plaintext shown exactly once at creation):

```
POST   /api/v1/keys        (admin)  {name, scopes:["scan:trigger", …]}
                           → {id, name, scopes, key:"plx_<32 hex>"}
GET    /api/v1/keys        (admin)  → list (no secrets), last_used_at
DELETE /api/v1/keys/{id}   (admin)
```

Presented as `Authorization: Bearer plx_…` — the `plx_` prefix routes
the extractor to key lookup instead of user-token lookup. Scopes for
this plan: `scan:trigger`, `status:read`. A key has exactly the scopes
it was created with; no key can read `/settings`, manage users, or play
media. Reason: monarr should hold a credential that can trigger scans
and nothing else — an admin user token can read the TMDB/Trakt secrets
back out of `GET /api/v1/settings`.

**Targeted scan.**

```
POST /api/v1/scan          (scope scan:trigger)
{
  "path": "/media/movies/Heat (1995)",        // absolute, a dir or file
  "ids": { "tmdb": 949, "imdb": "tt0113277" }, // optional, both optional
  "hint": "movie",                             // movie|episode|season|book
  "series": { "tmdb": 1396 },                  // for episodes: the SHOW id
  "book": {                                      // only for hint=book
    "title": "The Dispossessed",
    "author": "Ursula K. Le Guin",
    "medium": "ebook",
    "work_id": "curator:openlibrary:OL87320W",
    "edition_id": "curator:item:84:ebook",
    "cover_url": "https://covers.openlibrary.org/b/id/123-L.jpg"
  },
  "correlation_id": "t-42-a3f9c1",             // optional, echoed back
  "source": "monarr"                           // free-form label
}
```

Semantics (normative):

- plurx resolves the library by path: canonicalize, then
  component-wise prefix match against every library's roots. No match →
  `422 {"error":"path is not under any library root","roots":[…]}` —
  the error names the roots so a path-mapping mistake is self-explaining.
- Idle scanner → scan that subtree **synchronously** (it is one folder;
  the work is bounded) and return
  `200 {"status":"scanned","library_id":3,"report":{added,updated,
  unchanged,removed_files,skipped,errors,problems[]},"items":[{"item_id":
  1201,"file_id":88,"path":"…"}],"correlation_id":"t-42-a3f9c1"}`.
- Scanner busy for that library → coalesce into a pending set and return
  `202 {"status":"queued","request_id":"sr-…"}`. Progress:
  `GET /api/v1/scan/requests/{id}` (scope `status:read`) →
  `{status: queued|running|done|failed, report?, items?}`. Requests are
  kept in a bounded ring (256) — enough for "what happened last night",
  not an audit log.
- A targeted scan **never reconciles/prunes**: it upserts what it finds
  under `path` and touches nothing else. Reason: pruning against a
  partial view would delete the rest of the library — same precedent as
  the existing `walk_errors > 0` guard in `scan/mod.rs`.
- If `ids` are present, plurx applies them via the existing
  `MetadataPatch{tmdb_id, imdb_id}` → `store.apply_metadata` path after
  placement, marks the item matched so `items_needing_metadata` skips
  it, and enrichment fetches by id instead of `find_movie(title, year)`.
  For episodes, `series.tmdb` applies to the show item; season/episode
  numbers still come from the filename parse (they are monarr's naming
  and reliable).
- If `book` is present, Cinema applies Curator's bounded title, author,
  medium, work id, edition id, and optional Open Library cover after file
  placement. `work_id` is the only relationship key for ebook/audiobook
  editions; title and author never participate in linking.

### 3.6 monarr surfaces

**Outbound to plurx** (a new notifier type, §5.5): one POST per unique
imported directory, body exactly as §3.5, with retry 3× (5 s / 30 s /
2 m backoff) and a persistent delivery log. The scan response (or final
failure) is written into the download's handoff trace.

**Nothing inbound is required for the core plan.** Monarr stays a pure
client of both neighbors. (The later-phase plurx→monarr watched webhook
is §11.1.)

**Calendar for plurx (later phase, §11.2):** plurx's coming-soon rail
reads monarr's existing `GET /api/v1/calendar` with a monarr API key,
proxied server-side by plurxd so the key never reaches a browser.

### 3.7 Degradation ladder

Feature detection is by probing, never by version parsing:

| Situation | Behavior |
|---|---|
| nzbd lacks pp events (older build) | monarr's SSE consumer sees no `job_pp_finished`; the 30 s poll (still running in push mode at relaxed cadence, §5.2) keeps working. UI shows "live: connected (no pp events — update nzbd)" once the stream is open but only legacy events arrive. |
| SSE drops / nzbd restarts | reconnect with backoff + `Last-Event-ID`; on `reset`, full poll reconcile. Connection state is visible in monarr (§5.7). |
| plurx unreachable at import | notifier retries 3×, then the delivery log row is marked failed and the handoff trace records "plurx notify failed: <err>" — the import itself is already done and is never blocked. plurx's scheduled reconcile scan (§ plurx plan P5) eventually picks the files up anyway. |
| plurx lacks `/scan` (older build) | 404 → same failure path; health check flags the media server as degraded. |
| push disabled by the operator | everything works exactly as today: 30 s poll, scheduled scans. Push is an optimization, never a dependency. |

---

## 4. Work packages — nzbd (detail in `nzbd/docs/INTEGRATION_PLAN.md`)

| # | Package | One-liner |
|---|---|---|
| N1 | PP events | emit `job_pp_stage` + `job_pp_finished` (after history write) from the PP manager, via a new `EngineHandle::emit` |
| N2 | SSE resume | `id:` lines, 1024-event replay ring, `Last-Event-ID`, `reset` frame |
| N3 | History cursor | `seq` in `HistoryEntry` JSON + `?since_seq=` ascending query |
| N4 | Add params | `params` JSON object on `POST /api/v1/jobs`, `*`-prefix rejected |
| N5 | Attribution | `ClientRegistry` notes native-API + SSE callers (`X-Nzbd-Client`), UI shows event subscribers |
| N6 | Category honesty | `[[category]] dest_dir` actually used by PP move, or the projection removed — advertised paths must equal actual paths |
| N7 | Metrics + docs | `nzbd_events_emitted_total{event}`, `nzbd_sse_clients`, PP stage duration; ARCHITECTURE/USAGE/STATUS updated in the same commits |

Ships independently and first (§7): nothing in N1–N7 changes compat
behavior, so current monarr keeps working throughout.

---

## 5. Work packages — monarr (this repo; these are the marching orders)

### 5.1 Native `nzbd` client type

New adapter `internal/adapters/nzbd/nzbd.go` implementing
`ports.DownloadClient` against nzbd's native API (not the compat shim —
the native API is where params, `since_seq`, and SSE live):

- `Add` → `POST {url}/api/v1/jobs?url=<nzb>&category=<c>&params=<json>`
  with `params = {"monarr-transfer": <transfer id>}`; `Handle` is the
  returned id as a decimal string. Auth: `Authorization: Bearer <token>`
  when a token is configured, else `Basic`. Always send
  `X-Nzbd-Client: monarr/<buildinfo version>`.
- `Statuses` → `GET /api/v1/jobs` (queue: map `Queued|Paused|Fetching` →
  `StateQueued`, `Post*|PostQueued` → `StateDownloading` with the stage
  in `Message`, progress from `downloaded_bytes/size_bytes`) plus
  `GET /api/v1/history?limit=100` (`SUCCESS*` → `StateCompleted` with
  `SavePath = final_dir`; `FAILURE*` → `StateFailed`, status string as
  `Message`).
- `Remove` → `POST /api/v1/jobs/{id}/actions/delete` (or `delete-files`
  when `deleteData`), then hide the history row if present.
- `Test` → `GET /api/v1/status`.

Wiring: new case in `clientFactory` (`cmd/monarr/main.go`), `"nzbd"`
added to the openapi `type` enum for download clients, `CLIENT_DEFAULT_PORTS`
(`nzbd: '6789'`) and credential fields (host/port, token-or-basic) in
`web/src/pages/SettingsAcquisition.tsx`. Check `0007_client_zoo.sql` for
a CHECK constraint on `download_clients.type` — if one exists, rebuild
the table in the new migration (SQLite cannot alter CHECKs; `0012` is
the precedent).

**Acceptance:** with a live nzbd, adding a client of type nzbd passes
Test; a grabbed release appears in nzbd's queue with the
`monarr-transfer` param visible in nzbd's UI; on completion the poll
path imports it. `go test ./internal/adapters/nzbd/...` covers Add/
Statuses/Remove against a httptest fake serving canned native payloads.

### 5.2 Push subscription (the option)

New optional port capability, so only clients that can stream implement
it:

```go
// internal/ports/downloadclient.go
type ClientEvent struct {
    Handle  Handle
    Kind    string // "progress" | "pp_stage" | "completed" | "failed"
    Stage   string // pp stage name when Kind=="pp_stage"
    Status  DownloadStatus // populated for progress/completed/failed
    Seq     uint64
}
type Subscriber interface {
    Subscribe(ctx context.Context) (<-chan ClientEvent, error)
}
```

The nzbd adapter implements `Subscriber` by consuming
`GET /api/v1/events` with a raw HTTP client (EventSource can't send
`Authorization`), translating: `tick` → throttled `progress` events;
`job_pp_stage` → `pp_stage`; `job_pp_finished` → `completed` (with
`SavePath = final_dir`) or `failed` by `pp_status`; reconnect with
jittered backoff 1 s → 60 s and `Last-Event-ID`; on `reset` emit a
sentinel that triggers a full reconcile.

Supervisor `internal/app/acquisition/subscribe.go`: one goroutine per
enabled client whose `mode == "push"`. Progress events update an
in-memory progress map flushed to the DB only on state change or every
10 s (reason: nzbd ticks at 1 Hz; the DB must not). Completion events
call `reconcileOne` (§5.3). The supervisor records connection state
(connected-since, last event seq, last error) in memory, exposed via
the API for §5.7.

**The option:** `download_clients` gains
`mode TEXT NOT NULL DEFAULT 'poll' CHECK (mode IN ('poll','push'))`
(same migration as §5.1). UI: a "Updates" radio on the client form —
"Poll (every 30s)" / "Live (subscribe to events)" — with help text
saying exactly what each does. In push mode the poll does not stop; it
relaxes to a 5-minute reconcile sweep (belt and braces, and it is what
detects silently-dead streams). In poll mode nothing subscribes.

**Acceptance:** e2e (`test/e2e`) gains `fake-nzbd.mjs` (pattern:
existing `fake-arr.mjs`) that serves the native API + a scripted SSE
stream; a spec drives grab → pp stages → pp_finished and asserts the
Activity page shows the stage chips live and the import lands without
waiting for a poll tick. Kill the SSE mid-test; assert the 5-minute
reconcile (shortened via env) still completes the handoff and the UI
shows "live: reconnecting".

### 5.3 `reconcileOne` + idempotency

Extract the per-download switch out of `RefreshQueue` into
`reconcileDownload(ctx, dl, st)` so poll and push share one brain. Add a
per-download-id singleflight (mutex map) so a push event and a poll
tick racing on the same row cannot run `runImport` twice; `advance`
already appends — guard it so duplicate step entries aren't written
(same step+detail within a window is dropped). Handoff entries gain the
source in the detail string: `"completed (event seq 913)"` vs
`"completed (poll)"` — the trace should say which channel delivered.

**Acceptance:** a unit test fires `reconcileOne` and `RefreshQueue`
concurrently on the same completed download; exactly one import runs,
the handoff log contains one `downloaded` entry, and `place()` was
called once.

### 5.4 Paths and ids on `ImportCompleted`

`ImportCompleted` (declared in `internal/app/acquisition/acquisition.go`,
published from `import.go`) gains:

```go
Paths      []string // every placed file's absolute dest
Dirs       []string // unique parent dirs of Paths (what plurx scans)
MediaItemKind string // movie|series|book
Title      string
TmdbID     int64    // 0 when unknown
ImdbID     string
DownloadID int64
Transfer   string   // the §3.1 id
```

Collect `dest` where it is known — inside
`importMovieFile/importEpisodeFile/importBookFile` — by returning it on
`FileOutcome` rather than discarding it. The bus event is the only
source the notifier gets; if it isn't on the event, plurx can't be told.

**Acceptance:** existing import tests extended to assert `Paths`/`Dirs`
populated; `handoff_test.go` asserts the transfer id present.

### 5.5 The plurx notifier

- Migration: rebuild `notifiers` to widen the CHECK to include
  `'plurx'` (SQLite rebuild, `0012` precedent) **and** add
  `PUT /api/v1/notifiers/{id}` (today it's add/delete only — editing a
  URL means recreating; fix while the file is open).
- `internal/adapters/notify`: new case `plurx` with settings
  `{url, apiKey}`; implements a new optional capability
  `PathScanner` — `ScanPaths(ctx, ImportInfo) ([]ScanResult, error)` —
  instead of the blind `Send`. For each dir in `Dirs`: POST §3.5 body
  with ids and `correlation_id = Transfer`. Book requests carry a separate
  exact work/edition object, with no movie/series ids. `refreshOnly()`
  treatment applies (import events only) — plurx is never spammed with
  grab/health chatter.
- Dispatcher (`internal/app/notify/notify.go`): deliveries for plurx go
  through a small persistent queue — new table `notifier_deliveries
  {id, notifier_id, download_id, event, payload, attempts, last_error,
  status(pending|ok|failed), created_at, updated_at}` — retried 3× with
  5 s / 30 s / 2 m backoff. On terminal result, write the handoff trace:
  step `notify_plurx`, detail `"plurx scanned: item #1201, 1 added"` or
  `"plurx notify failed after 3 attempts: connection refused"`. Books use
  the same queue with `hint:"book"`, an exact directory, no TMDB/IMDb fields,
  and Curator's title, author, persisted medium, stable work/edition ids, and
  optional allowlisted Open Library cover. Cinema still owns file-derived
  format validation.
- API + UI: `GET /api/v1/notifiers/{id}/deliveries` (last 100), rendered
  as an expandable log on the notifier card in
  `SettingsNotifiers.tsx` (`TYPE_FIELDS` gains plurx: url + apiKey).

**Acceptance:** e2e `fake-plurx.mjs` accepts `/api/v1/scan`, returns a
canned report; spec asserts the Activity trace shows
`notify_plurx → plurx scanned: item #…` within one import, and that
killing fake-plurx produces a failed delivery row + trace entry, while
the import itself still completes.

### 5.6 Health checks

Register in `main.go` against the existing registry:

- `client:<name>` for every enabled download client: `Test()` plus
  last-successful-contact age (warn > 5 m, error > 30 m).
- `mediaserver:<name>` for every plurx/plex/jellyfin notifier:
  reachability probe (plurx: `GET /api/v1/server`, which is public).
- `client:<name>:capacity` for nzbd clients: map `StatusDto.disk_low` →
  warning "nzbd reports destination disk low", `quota_reached` →
  warning, `blocked_servers > 0` → warning naming the count,
  `health_abort` → error. Source: the status snapshot the poll/subscribe
  path already fetches — no extra requests.

**Acceptance:** unit tests fake each condition and assert the
`health.Changed` transition + the notifier `OnHealth` payload names the
condition in plain words.

### 5.7 Connections panel

`GET /api/v1/system/connections` → per remote app:
`{name, kind: downloadclient|mediaserver, type, url(redacted creds),
state: live|polling|degraded|unreachable, version, lastContact,
lastEventSeq?, detail}` — assembled from the §5.2 supervisor state, the
health results, and cached `Test()`/`server_info` responses. Rendered as
a "Connections" card on `web/src/pages/System.tsx`: one row per app,
state chip, last-contact age, and for push clients "event #913 · 2s
ago". This is the single screen that answers "are the three apps
actually talking right now".

**Acceptance:** e2e asserts the panel shows the fake nzbd as
`live` and flips to `polling`/`unreachable` when the fake is stopped.

### 5.8 Docs + STATUS (same-commit rule)

`docs/architecture.md` §5: replace the aspirational event list with the
real one (the brief found `DownloadCompleted`/`FileImported` documented
but never built — fix the doc). `docs/settings.md`: the new client mode,
notifier type, delivery log. `README.md` + `STATUS.md`: phase entry.
Reason: a stale doc claiming push exists is exactly the black box this
plan is against.

---

## 6. Work packages — plurx (detail in `plurx/docs/INTEGRATION-PLAN.md`)

| # | Package | One-liner |
|---|---|---|
| P1 | Scoped API keys | `plx_` bearer keys, hashed at rest, scopes `scan:trigger`/`status:read`, admin CRUD routes, keys UI section |
| P2 | Targeted scan core | `scan_path(store, &Library, &Path, ids)` — subtree walk, upsert only, **never prunes**; id application via `MetadataPatch` |
| P3 | `/api/v1/scan` endpoint | path→library resolution with self-explaining errors, sync when idle, 202 + request record when busy, coalescing pending set |
| P4 | Enrich by id | when `tmdb_id` present, fetch by id — skip `find_movie/find_show` fuzzy matching |
| P5 | Scheduled reconcile scan | DB setting `scan.interval_hours` (default 12, 0 = off) driving full per-library scans — the safety net under targeted scans, and plurx's first-ever scheduled scan |
| P6 | Integration visibility | `plurxd::integrate` tracing target with `correlation_id`, last-notification record in `/api/v1/system` + activity detail, `plurx_scan_total{trigger}` / `plurx_notify_received_total` metrics |
| P7 | Docs | SECURITY (keys), FEATURES, OPERATIONS (monarr pairing runbook), CHEATSHEET |

Later-phase packages P8 (watched → monarr) and P9 (coming-soon rail) are
sketched in §11 and in the plurx plan, flagged as not-yet-committed
designs.

---

## 7. Phasing — the order Opus sessions should run

| Phase | Repo | Packages | What ships / why this order |
|---|---|---|---|
| 0 | all | these docs | contract agreed before code |
| 1 | nzbd | N1–N7 | events + cursor exist; pure addition, current monarr unaffected; everything later consumes this |
| 2 | monarr | M §5.1–5.3, §5.6 partial | native client + push option + health; usable immediately against phase-1 nzbd |
| 3 | plurx | P1–P6 | keys + targeted scan + schedule; independently useful (manual `curl` can drive `/scan`) |
| 4 | monarr | M §5.4–5.5 | import paths + plurx notifier + trace feedback — the second seam closes |
| 5 | both | M §5.7–5.8, P7, N7 leftovers | connections panel, metrics, docs sweep |
| 6 | plurx+monarr | §11 (P8/P9 + monarr inbound) | watched signal + coming-soon; separate decision gate before building |

Each phase is a working system; nothing depends on a later phase to
avoid regressing. Rollback story per phase: disable the option
(client mode back to `poll`, notifier disabled, keys deleted) — no
migrations need reverting to turn features off.

---

## 8. Observability inventory — where to look when it sticks

The rule: for every hop there is exactly one first place to look, and it
names the next place.

| Symptom | First place to look | What it tells you |
|---|---|---|
| grabbed, nothing downloading | monarr Activity → handoff trace | last step + detail names the client and the add response |
| downloading forever | nzbd UI queue / `GET /api/v1/jobs` | per-file/segment state, health, server blocks |
| stuck "verifying/unpacking" | Activity trace `pp_stage` entries (push) or nzbd job detail | which PP stage, since when |
| finished in nzbd, not imported | nzbd History → handoff chips (`seen_count`, `picked_up_by`) | did monarr ever look? was it a different consumer? then monarr trace for the import error |
| imported, not in plurx | Activity trace `notify_plurx` entry | scanned+item id, or the delivery error; then plurx `GET /api/v1/scan/requests/{id}` / activity detail |
| plurx scanned, wrong match | plurx item — ids present? | ids missing ⇒ monarr didn't have them (check trace detail); present ⇒ plurx enrich-by-id bug |
| "is push even on?" | monarr System → Connections | live/polling per client, last event seq + age |
| nothing anywhere | each app's health: monarr `/api/v1/health`, nzbd `/healthz` + `/metrics`, plurx `/readyz` | process-level truth |

Every trace entry, log line, and scan-request record carries the §3.1
transfer id, so `grep t-42-a3f9c1` across any app's logs reconstructs
the story.

---

## 9. Testing strategy

- **nzbd** (`crates/nzbd/tests/daemon.rs` + engine/post crate tests):
  event ordering (pp_finished only after the history row is readable),
  `Last-Event-ID` replay and `reset`, `since_seq` pagination, params
  round-trip into history, `*`-prefix rejection, category dest_dir now
  honored. The existing nserv-backed e2e gains one full-pipeline case
  asserting the event sequence for a real download.
- **monarr** (Go unit + Playwright e2e): adapter against httptest
  fixtures copied from real nzbd responses; race test §5.3; e2e with
  `fake-nzbd.mjs` + `fake-plurx.mjs` driving the full trace including
  failure injections (SSE cut, plurx down).
- **plurx** (Rust http tests): auth matrix (no key / wrong scope /
  disabled key), path outside roots, sync vs queued vs coalesced,
  no-prune property (targeted scan of one folder never deletes rows
  elsewhere — assert row counts), id application short-circuits fuzzy
  match.
- **Tri-app live smoke (optional, after phase 5):** compose file in
  `monarr/test/integration/` booting real nzbd + its `nzbd-nserv` fake
  news server + monarr + plurx; script posts a release to nserv, waits,
  asserts a playable plurx item exists and the monarr trace has every
  step. Weekly CI like nzbd's `arr-live.yml`, not per-push (it's slow
  and involves four processes).

---

## 10. Non-goals and guardrails

Do not do these; each has a reason. An executing agent that thinks one
of these is blocking should stop and flag it.

1. **Do not touch the nzbget-compat surface** beyond the attribution in
   N5. Third-party arrs use it; the native path is additive. Golden
   tests (`nzbd-compat/tests/golden.rs`) must pass unmodified.
2. **No message broker, no shared database, no new daemon.** In-process
   buses + plain HTTP between apps. This is single-host homelab scale;
   monarr's ADR 0008 already made this call for its own internals.
3. **Push is never load-bearing.** Every push path degrades to the
   existing polls/scheduled scans (§3.7). If a design choice makes poll
   mode worse, it's wrong.
4. **Imports never block on plurx**, and downloads never block on
   monarr's SSE consumer. Notify is post-import, async, retried.
5. **plurx never writes to media storage** (existing invariant, stated
   in its ARCHITECTURE) — it scans what monarr placed, nothing more.
6. **Monarr never holds a plurx admin token** — scoped key only. If the
   key can read `/settings`, P1 is not done.
7. **No invented movie ids or fuzzy book identity.** Send the exact path with
   `hint:"book"` and Curator's explicit work/edition object. Cinema verifies
   text versus audio from the file and relates editions by exact work id;
   title and author never become a join key.
8. **nzbd gains no outbound HTTP** in these phases — no webhook
   subsystem, no knowledge of monarr's address. That was the §3
   direction decision; revisit only as a new plan.
9. **Don't rename existing events, steps, statuses, or params** —
   `PpFinal` strings, handoff step names, `DownloadState` values are
   all load-bearing for existing UI and tests. Extend, don't rename.
10. **Secrets stay masked** — nzbd's config `***unchanged***` mask,
    monarr's redacted URLs in the connections panel, plurx keys hashed
    at rest and shown once. A debugging surface that leaks a credential
    is a regression, not a feature.

---

## 11. Later phases (decided in, designed loosely — re-scope before building)

### 11.1 plurx watched → monarr

Purpose: monarr can prefer upgrades for shows someone is actively
watching, or run cleanup policies on fully-watched items. Sketch: plurx
DB settings `monarr.url` + `monarr.api_key` + `monarr.watched_sync`
(default off); on scrobble/95% crossing, plurxd queues
`POST {monarr}/api/v1/webhooks/plurx`
`{event:"watched", kind, tmdb, imdb, season, episode, watched_at}`
(aggregate any-user signal, no usernames — monarr doesn't need to know
who), retried like §5.5. Monarr records it on the media item and exposes
`watched` to decision/cleanup logic. **Open questions to settle when
this phase starts:** per-profile policy shape, multi-user semantics,
whether "watched" should ever gate deletion automatically.

#### Settled 2026-07-27 (Paul)

The three open questions above, answered. Two of these change the sketch;
where they do, **these answers win over the sketch** — they were made with
the code in front of us and the sketch was not.

1. **What monarr does with it: record and display, plus prefer upgrades for
   what is being watched.** Watched state lands on the media item and is
   visible; a show someone is actively watching gets priority in backlog
   search and upgrade decisions. Explicitly **not** in scope: unmonitoring
   fully-watched items, and cleanup policies.

2. **Per-user, with usernames** — *not* the sketch's aggregate any-user
   signal. This is the one that changes the design, and it has a cost worth
   naming: viewing history is personal, and this copies it into a second
   application that has no other reason to hold it. So it is gated: the
   whole feature is off by default (`monarr.watched_sync`), turning it on is
   a deliberate admin act, and the setting must say plainly what it sends.
   The reason for the answer is that per-user is the only shape that can
   later support "everyone who could has watched it", which is the meaning
   any cleanup policy would need — and an aggregate signal cannot be
   refined into a per-user one after the fact.

3. **Deletion: only behind an explicit opt-in, and not now.** No automatic
   deletion is built in this phase, because (1) did not ask for cleanup.
   When it is built: off by default, per-profile, and turned on
   deliberately. Nothing may ever delete a file as a side effect of somebody
   finishing an episode.

**Scope, therefore:**

- plurx: `monarr.watched_sync` (default off), reusing the `monarr.url` /
  `monarr.api_key` pair the coming-soon rail already added. On scrobble or
  the 95% auto-watch crossing, queue
  `POST {monarr}/api/v1/webhooks/plurx`
  `{event:"watched", kind, tmdb, imdb, season, episode, watched_at, user}`,
  retried like §5.5 and visible on `plurxd::integrate`. This is plurx's
  first outbound push, so it needs a small delivery queue of its own.
- monarr: receive it, record watched-by-user on the media item, show it, and
  let the decision engine prefer upgrades for actively-watched series.
- Neither side gains a delete path.

### 11.2 Coming-soon rail in plurx

Purpose: see what's on the way without leaving the player. Sketch:
plurxd proxies `GET /api/v1/coming-soon` → monarr
`GET /api/v1/calendar` using a monarr API key from plurx DB settings
(server-side only — the key never reaches a browser), cached 15 m,
rendered as a home rail with "expected <date>" cards. Read-only, one
endpoint, zero monarr changes (its calendar API exists).

---

## 12. Doc upkeep — what changes with the code

Same-commit rule per repo: nzbd — `ARCHITECTURE.md` (events §, SSE
resume), `USAGE.md` (the handoff section already says "there is no
push"; that sentence changes), `CONFIGURATION.md` (only if any knob is
added), `STATUS.md` checklist. monarr — `architecture.md` §5 event list
(currently documents events that were never built — fix), `settings.md`,
`README.md`, `STATUS.md`. plurx — `SECURITY.md` (keys), `FEATURES.md`,
`OPERATIONS.md` (pairing runbook: create key → paste into monarr →
verify on the Connections panel), `ROADMAP.md`. This file and the two
per-repo plans get their status lines updated as phases land.
