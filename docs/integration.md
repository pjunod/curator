# Integration — Monarr's seams with nzbd and plurx, and how to prove they work

This page is the operator reference for the pipeline: what each seam does,
where you look at it, and the exact command that proves it. The design it
implements is [PIPELINE_CONTRACT.md](PIPELINE_CONTRACT.md) (pipeline contract
v2) and the decision record is [ADR 0026](adr/0026-pipeline-contract-v2.md);
where this page and the contract disagree about intent the contract wins, and
where they disagree about a literal the code wins and this page is a bug.
[settings.md](settings.md) lists every field; [usage.md](usage.md) is the
walkthrough of the screens. Every other document is listed in
[README.md](README.md).

Names: **Runner** is nzbd (the downloader), **Cinema** is plurx (the player),
**Curator** is Monarr (this application). The code, the API and the screens use
the product names (`nzbd`, `plurx`, `monarr_*`); the contract and the status
pages use the role names. They mean the same processes.

Monarr is the **middle** of a three-application pipeline. It is the only one
that talks to both of the others, and the only one that holds the identifier
tying a grab to a playable file. When something in the chain is broken, Monarr
is where you look first — not because it is the likely culprit, but because it
is the only vantage point that sees both directions.

## How one download moves

| Step | Who owns it | Where you see it | Section |
|---|---|---|---|
| Grab: Monarr picks a release, mints a **transfer token**, submits it to Runner | Monarr | Activity row `grabbed`, `submission_phase` | [3](#3-the-transfer-token--one-identifier-grab-to-playable) |
| Runner downloads, repairs, unpacks; Monarr is told as **facts** (stream) and checks them by polling | Runner states, Monarr records | Activity progress, System → Connections | [2](#2-nzbd-event-stream-and-the-poll--latency-never-correctness), [4](#4-facts-terminals-and-the-next-grab) |
| A **terminal** (`completed`, `failed{reason}`, `cancelled`) is recorded exactly once | Monarr's reducer | Activity trace; `downloads.runner_outcome` | [4](#4-facts-terminals-and-the-next-grab) |
| A failure that blames the release is **the next grab** | Monarr | Activity: replacement grab; history `failure_to_next_grab` | [4](#4-facts-terminals-and-the-next-grab) |
| Completed bytes are a **publication**: a directory plus its manifest, imported per file | Runner publishes, Monarr imports | Activity → publication, Storage | [5](#5-publications-import-and-per-file-cleanup) |
| Each placed directory becomes a **sub-delivery** to Cinema, read back until every file is visible | Monarr's outbox | Settings → Notifications → Delivery log | [6](#6-telling-cinema--one-verified-delivery-per-directory) |

Three timings measure the seams — `failure_to_next_grab`,
`runner_done_to_import`, `import_to_cinema_visible` — plus an overdue count,
all on System → Connections (`GET /api/v1/system/pipeline`, see
[Metrics](#metrics)).


## The pipeline

```
   ┌───────────────┐     grab (params: monarr-transfer)     ┌──────────────┐
   │               │  ────────────────────────────────────▶ │     nzbd     │
   │    Monarr     │                                        │ (downloads)  │
   │  (decides)    │  ◀──────────────────────────────────── │              │
   │               │   SSE /api/v1/events  ·  30s poll      └──────────────┘
   └───┬───────▲───┘
       │       │
       │       │ POST /api/v1/webhooks/plurx   (watch state, ids only)
       │       │ GET  /api/v1/calendar         (the Coming soon rail)
       │       │ GET  /api/v1/system/status    (plurx's Test connection)
       │       │
       │ POST /api/v1/scan  (correlation_id = the transfer id)
       ▼       │
   ┌───────────┴───┐
   │     plurx     │       nzbd and plurx never speak.
   │   (plays it)  │       Every path between them runs through Monarr.
   └───────────────┘
```

Default ports: nzbd 6789 · **Monarr 7676** · plurx 32400.

## Wiring the three together in Docker

Do this first. Every "cannot reach" in the rest of this document is more often
a container that cannot resolve a name than a wrong key or a wrong port.

**One shared user-defined network, and address everything by container name.**
That is the recommended setup, not merely one that works.

```bash
docker network create media          # once, on the host
```

Then in **each** compose file — monarr's, plurx's, nzbd's — attach the service
and declare the network as external:

```yaml
services:
  monarr:
    container_name: monarr           # this name IS the hostname
    # ...the rest of your service...
    networks:
      - media

networks:
  media:
    external: true                   # created above; compose must not own it
```

`docker compose up -d` in each directory.

**Get the names from Docker, not from memory.** One command answers both "what
do I put in the URL" and "are they actually on the same network":

```bash
docker ps --format '{{.Names}}\t{{.Networks}}'
```

```
plurxd	media
nzbd	media
monarr	media
```

Column 1 is the hostname. Column 2 must contain the shared network for every
container that has to reach another — anything showing `bridge` or `host`
cannot be resolved by name from the others.

Read column 1 literally. A container called `plurxd` is **not** reachable as
`plurx`, and the failure does not read like a typo. Monarr reports
`cannot reach plurx: Post "http://plurx:32400/api/v1/scan": dial tcp: lookup
plurx on 127.0.0.11:53: server misbehaving` — that is Docker's embedded
resolver at `127.0.0.11` saying the name does not exist on this network.
Nothing was ever dialled, and the port in that message is irrelevant. Fix the
name before touching anything else.

So, for the containers above:

| Setting | Value |
|---|---|
| Monarr → nzbd download client | host `nzbd`, port `6789` |
| Monarr → plurx notifier URL | `http://plurxd:32400` |
| plurx → monarr URL | `http://monarr:7676` |

Three more things that trip people up, each with its reason:

**The hostname is the `container_name`, or the service key if you did not set
one.** Not the image, not the directory, and not the product's name — `plurxd`
is a real example of all three being different.

**Published ports are irrelevant on this path.** `ports: - "7676:7676"` maps
host→container; container→container traffic goes straight to the *internal*
port and works even with no published port at all. Keep the published ones for
your own browser, and stop reasoning about them when debugging a seam.

**`host.docker.internal` is a per-container setting, and it is the wrong tool
here.** It only exists in a container whose compose declares
`extra_hosts: ["host.docker.internal:host-gateway"]`, so putting that line on
*Monarr's* service does nothing for plurx — plurx is the process making the
plurx→Monarr call, and plurx's container is the one that has to resolve the
name. It also routes container→host→container for traffic that never needed to
leave the bridge. It is a valid fallback when you cannot change all three
compose files; it is not the good answer.

**How to tell which one you are hitting.** plurx now reports the root cause
rather than the wrapper: `dns error: failed to lookup address information`
means the name is not resolvable from that container — wrong network, or a
missing `extra_hosts`. `Connection refused` means the name resolved and nothing
was listening on that port — the application is down, or you have the internal
and published ports mixed up.

## The seams Monarr has

| # | Seam | Direction | Transport | Who starts it |
|---|---|---|---|---|
| 1 | nzbd native client | outbound | HTTP `/api/v1/*` | Monarr |
| 2 | nzbd event stream and the poll | outbound (held open) | SSE + HTTP | Monarr |
| 3 | The transfer token | outbound, every seam | job param → `correlation_id` | Monarr |
| 4 | Facts, terminals and the next grab | internal | SQLite, one transaction per terminal | Monarr |
| 5 | Publications: import and per-file cleanup | internal, on the shared completed root | filesystem + SQLite | Monarr |
| 6 | Telling Cinema | outbound | `POST /api/v1/scan` + `GET /api/v1/scan/requests/{id}` | Monarr, on import |
| 7 | plurx watch webhook | **inbound** | `POST /api/v1/webhooks/plurx` | plurx |
| 8 | Calendar + status reads | **inbound** | `GET /api/v1/calendar`, `/system/status` | plurx |
| 9 | Connections panel | observation | `GET /api/v1/system/connections` | you |
| 10 | In-flight transfers | observation | `GET /api/v1/system/transfers` | you |

Sections 1–6 are things Monarr does. Sections 7–8 are things done to it — and
they are the half that used to be invisible, which is why §9 exists.

---


## 1. nzbd native client — the rich path, not the compat path

**What it does.** The `nzbd (native)` client type drives nzbd's own REST API
instead of the NZBGet-compatible shim. That buys three things the shim cannot
carry: job params (§3), a real event stream (§2), and capacity reporting.

**Wire.** `POST /api/v1/jobs` to add · `GET /api/v1/jobs` for the queue ·
`GET /api/v1/history?since_seq=&limit=100` (the ordered reader, §4) · `GET /api/v1/deliveries?after=&include_accepted=true` (publication facts, §5) · `GET /request-admissions/<token>` (the receipt lookup, §4) · `POST /api/v1/jobs/{id}/actions/delete` ·
`GET /api/v1/status` for the probe. Auth is a single `Authorization` header:
Bearer when the client has a password and no username, HTTP Basic otherwise.
Every request carries `X-Nzbd-Client: monarr/<version>`.

**Where you configure it.** Settings → **Download clients** → type
`nzbd (native)`. Fields: `Name`, `Host`, `Port` (default 6789),
`Username (blank if using a token)`, `Token, or password`, `Category`,
`Require approval`, `Clean up after import`, and — only for this type — the
**Live updates** checkbox (§2).

**Where you see it working.**

- Settings → Download clients, per-row **Test** → `✓ reachable` / `✕ <message>`.
  This probes **stored** credentials, not what is typed in the box.
- System → **Connections**, kind `download client`, state `polling` or `live`.
- nzbd's own History tab shows a `monarr/<version>` chip.

**How to verify.**

```bash
MONARR=http://127.0.0.1:7676
KEY=<Access → API access → Reveal>

curl -sS -H "X-Api-Key: $KEY" "$MONARR/api/v1/system/connections" | python3 -m json.tool
curl -sS -H "X-Api-Key: $KEY" "$MONARR/api/v1/health"             | python3 -m json.tool
```

**How to read it.** `unreachable` carries the reach error verbatim — read it,
it is usually the answer. `degraded` means nzbd answered but is not
working, and the detail is one or more of these, joined by `; `:
`the destination disk is low on space` · `the download quota is used up` ·
`N news server(s) are blocked` · `the queue is paused`.

nzbd also reports `health_abort`, and Monarr **deliberately does not surface
it**. It reflects nzbd's `[post] health_action` being `park` or `delete` — an
operator policy that is on by default and correct to leave on. Surfacing it
produced a red badge that could never be cleared, which trains you to ignore
the panel. A signal that is always on is not a signal. `unprobed` on a download client should never happen —
it is the state reserved for Plex/Jellyfin, whose only real test is a full
library rescan.

---

## 2. nzbd event stream and the poll — latency, never correctness

**What it does.** With **Live updates** ticked
(`download_clients.mode = 'push'`), Monarr holds nzbd's SSE stream open and
imports the moment post-processing finishes, instead of up to 30 s later.

**What Monarr acts on.** Each event becomes a fact for the reducer (see
[§4](#4-facts-terminals-and-the-next-grab)): `job_pp_finished` (the terminal:
the typed `outcome`/`reason`/`blames_release`/`salvage`; the v1 `pp_status`
and `final_dir` strings are not read), `job_status_changed` and `job_control_changed`
(status and holds), `job_publication_ready` (a salvage), `job_pp_stage` and
`tick` (progress), `reset` and `lagged` (immediate poll). `job_deleted` carries
no transfer token and is advisory: the cancellation arrives as a terminal from
Runner's own record. Everything else is dropped. `job_finished` is **deliberately ignored**: it fires when the
download ends, before repair and extraction, and importing then would import a
directory that is not finished.

**Where you see it.**

- System → **Connections**: state `live` — *"A push stream is open —
  completions arrive the moment they happen"*. If the stream is down but push
  is configured, the state reads `polling` with the detail
  `push is configured but the stream is not connected`.
- Settings → Download clients: the `live` / `poll` pill. **This pill is an
  echo of your config, not a probe.** It says what you asked for; Connections
  says what you got. When they disagree, believe Connections.
- System → **Live events** — the SSE tail out of Monarr's own event bus, with
  a dot titled `SSE connected` / `SSE disconnected`.

**How to read it.** The 30 s poll runs in push mode too. A dropped stream
costs latency and nothing else, and Monarr reconnects forever with `1s → 60s`
jittered backoff, injecting a synthetic reset on every reconnect because a gap
in the stream is a gap in knowledge whatever caused it. `live` degrading to
`polling` is a performance regression, not an outage — do not page yourself
for it.

---

## 3. The transfer token — one identifier, grab to playable

**What it does.** At grab time Monarr mints a transfer token —
`t-<downloadID>-<32 hex>`, 16 random bytes, the row id there only so a log line
is readable (`0` when the row is not committed yet) — and attaches it to
everything downstream, so a single `grep` across three applications
reconstructs one transfer. The token is **opaque**: every comparison is exact
equality, never a parse, so values minted by earlier releases keep working.
Every fact Runner sends about a job is admitted only when its token equals the
row's (§4).

**Where it lives.**

| Stage | Name | Storage |
|---|---|---|
| Monarr | `transfer` | column `downloads.transfer`, and appended to `downloads.handoff_log` |
| nzbd | `monarr-transfer` | job `params`, returned on history + `job_pp_finished`, re-attached on requeue |
| plurx | `correlation_id` | request field on `POST /api/v1/scan`, echoed back and logged |

**Where you see it.** **Activity** → expand a row → the handoff detail shows
`Client reported <savePath>`, `Monarr looked in <importPath>`, and the ordered
trace: `Grabbed` · `Downloading` · `Downloaded` · `Awaiting approval` ·
`Importing` · `Imported` · `Failed`.

**How to verify.**

```bash
# monarr's copy
sqlite3 <datadir>/monarr.db "select id, transfer, title from downloads order by id desc limit 5;"

# the same id, as nzbd stored it
curl -sS -H "Authorization: Bearer $NZBD_TOKEN" \
     "$NZBD/api/v1/history?limit=5" | grep -o 'monarr-transfer[^]]*'
```

**How to read it.** The id is attached opportunistically — Monarr type-asserts
a tagging-capable client and falls back to a plain add when the client cannot
carry params. So an empty `params` on an nzbd job means the grab took the
untagged path; the download and import still work, you just lose the join.
Losing the join is a diagnostic cost, not a functional one.

---

## 4. Facts, terminals and the next grab

The design is [PIPELINE_CONTRACT.md](PIPELINE_CONTRACT.md) section 3; the
decision record is [ADR 0026](adr/0026-pipeline-contract-v2.md). The code is
`internal/app/acquisition/reducer.go` (`observe`), `internal/adapters/nzbd/facts.go`
(wire → fact) and `internal/infra/sqlite/pipeline_v2.go` (the guarded writes).
Runner clients are on the reducer by capability (`ports.RunnerFeeds`); every
other client keeps the established path and records its failures the same way.

- **Facts, one reducer.** Every observation of a Runner job — an SSE event,
  a queue row, a history row, a `/deliveries` row, a request receipt — is a
  *fact* (`admitted`, `status`, `terminal`, `publication_ready`, `gone`), and
  one function applies them all. A fact is admitted only when its `transfer`
  token equals the row's; a mismatch (a job id reused after a Runner state
  wipe) is dropped and counted as `monarr_runner_fact_mismatch_total`. An
  event without a token falls back to the job handle.
- **A terminal is recorded once.** `downloads.runner_outcome` is set by one
  guarded write; in the same transaction Curator writes the work it implies:
  a `publications` row for `completed`, a `replacement_grabs` row for a
  failure that re-grabs, the blocklist entry when `blames_release`, the
  history row. A replay from the other feed is a no-op. A status never moves
  a terminal row; a terminal ends a hold unconditionally.
- **Curator's own terminals are provisional**: `failed{submit}` (ten minutes
  of Runner positively answering "no such token" — an unreachable Runner does
  not count), `failed{lost}` (ten minutes of successful polls with the job in
  no feed and no receipt) and rows migrated from the old parks
  (`failed{parked: …}`). Any later Runner fact re-opens them. Curator never
  writes `hold_expired`: a hold past Runner's `deadline_in_s` plus five
  minutes (24 hours when an older Runner sends none) is shown as
  **held (stale)** and reconciled through the request receipt.
- **The history cursor** (`runner_history_cursors`: incarnation, since_seq)
  is advanced only by the poll's ordered reader, after its page commits.
  `GET /api/v1/history?since_seq=&limit=100` replaces the old `limit=100`
  re-read. A changed incarnation, a cursor below `retained_min_seq` or no
  stored cursor at all (the first start of a build with the reducer) looks every outstanding transfer
  up via `GET /request-admissions/<token>` and sets the cursor to Runner's
  head. Events never move the cursor.
- **`/deliveries` is read with `include_accepted=true`**, only to raise
  facts: a `completed` row the stream missed, a `salvage` row (or any row for
  a download Runner finally failed) as `publication_ready`. Curator no longer
  acknowledges deliveries; the lease/verify/ack/import cycle is gone.
- **Publications are the import unit**, one per Runner generation, each with
  its own state (`discovered → importing → imported | import_failed`) and a
  per-file outcome: `placed`, `discarded{superseded|duplicate|rejected}`, or
  `unresolved` (retained, decided in Activity). The manifest comes from the
  publication's `.runner-manifest.json`; a directory without it waits and is
  checked again each poll. A publication has no deadline until its first import
  starts; six hours from that start (never reset by a retry), with nothing
  running or queued for it, or five attempts that failed on the payload
  (spaced on the retry ladder), end it; so does, at once, a file that does
  not match the manifest (missing, resized, different bytes as copied) or a
  manifest that is malformed. Ending records what it already placed (and
  notifies Cinema), discards the rest, and grabs the still-wanted part of the
  want again. A publication the **environment** holds — a full or blocked
  library, the completed root's storage unavailable or outside the
  configured roots, the manifest unreadable for now or still missing 30
  minutes after discovery — waits: it is retried on the retry ladder (not
  every poll), never ended by its deadline (which starts over when the hold
  ends), and the sweep never discards its pending media. The hold is
  recorded once when it begins (the download row's `import_held` step and an
  `import_held` History row) and once when it clears (`import_hold_cleared`);
  the queue's publication carries `held`, `heldReason` and `heldSince`, and
  `GET /api/v1/system/pipeline` counts them (`heldPublications`). A
  completion for a publication that finished, was ended or was held in the
  meantime writes nothing. A person's retry starts a
  publication over (no attempts, no deadline). A file of another attempt is
  superseded only while that attempt is live or its file is in the library. Hash once: the manifest digest is the
  authority, and the placement proves each file it copies against it in the
  copy stream (nothing is read back; a same-filesystem placement is a hard
  link). When the publication was copied across volumes a linked file is
  hashed once, so every placed byte is verified once.
- **Which attempt wins a destination** is Curator's own record
  (`wantable_attempts`: the download it last chose for each wantable), never
  generation ids or arrival time. A file from another attempt is
  `discarded{superseded}` when the library already holds the current
  attempt's file, otherwise `unresolved`; no record is `unresolved`.
- **Failure is the next grab.** The replacement grab worker (every 5 s)
  executes `replacement_grabs`: a movie or book is searched; an episode is
  searched for a single-episode release, never a pack; an episode with no
  single release goes to the season planner. `cancelled`, `lost` and
  `fetch{indexer_unavailable}` write no grab; only `blames_release` failures
  blocklist and spend fetched bytes against the re-grab budget. A
  `ReplacementOperation` advances with the failed bytes retained. The failure
  reasons, and which of them blame the release, are Runner's typed terminal
  (`outcome`, `reason`, `blames_release`, `salvage`, `fetch_cause`): `health`,
  `fetch`, `par`, `unpack`, `script`, `publish`, `io`, `identity`, `hold_expired`
  are Runner's; `submit`, `lost` and `parked` are Curator's and always
  provisional. `publish`, `io` and `identity` park the bytes whatever the
  client's `failure_action` says; a `fetch` whose cause is
  `indexer_unavailable` re-queues the same want after 15 minutes.
- **The historical cohort** is described under §5.

What Curator no longer does with Runner: acknowledge deliveries, pace receipt
lookups per row, park on receipt shapes, refuse controls by revision, wait for
Runner's cleanup before re-grabbing, or ask Runner to clean its source.

### Held jobs

A Runner job that needs a person or a resource is **held**: Runner sends
`job_status_changed` with status `held` (the `control` object — cause, stage,
message — is optional) and persists the hold before it emits. Curator shows it
as held with Runner's message and its deadline, never as failed, and a missing
fact cannot clear it. Resuming goes through `POST /api/v1/queue/{id}/resume`,
which calls Runner's `POST /api/v1/jobs/{id}/actions/resume`; it does not add
an NZB. A terminal ends a hold unconditionally. Manual pause intent survives a
resource release.

---

## 5. Publications: import and per-file cleanup

Contract section 3.4 and section 1. A **publication** is one directory Runner
published for one generation of one transfer, with a manifest naming its files.
It is the unit of import, cleanup and notification, so a late salvage, a
second generation and the historical directories all take the same path.

- **States**: `discovered → importing → imported | import_failed` (and
  `removed` once every file is gone). Six hours from the first import start
  is `import_failed{stalled}`, and the want is grabbed again. A directory with no manifest yet waits and is
  checked again each poll.
- **The manifest** is `.runner-manifest.json` in the directory, written last as
  the commit marker (a directory without it is not a publication). The digest is
  the deployed shared algorithm (`runner-manifest-v1`: `len:path`, size and
  SHA-256 per bytewise-sorted file, bare hex). The per-file digests are the
  authority for the bytes: each placement compares its copy stream with them.
- **Per-file outcome**: `placed{library_path, sha256}`, `discarded{superseded |
  duplicate | rejected}`, or `unresolved` (retained, decided in Activity →
  Resolve). Sidecars are decided by the import: subtitles are placed beside
  their video (`<video>.<lang>[.forced][.sdh].<ext>`; one video takes every
  subtitle, a season pack matches by name prefix or folder, an unmatched one
  is `discarded{rejected}`), and every other non-media file is
  `discarded{rejected}`. No sidecar is ever `unresolved`; ones an earlier
  build left so in an imported publication are re-decided by the cleanup
  pass (migration 0076 marks them pending). Replacing a video removes the
  subtitles Curator placed for it (by their `placed` record, while they hold
  the placed bytes) and places the new release's. Curator never acknowledges a delivery to Runner and never asks
  Runner to clean its source.
- **Which attempt wins a destination** is Curator's own record
  (`wantable_attempts`: the download it last chose for each wantable), never
  generation ids or arrival time. A file from another attempt is
  `discarded{superseded}` when the library already holds the current
  attempt's file, otherwise `unresolved`; no record is `unresolved`.
- **The historical cohort.** Every v1 receipt (accepted or not) and every
  completed directory present at the first start of a build with publications
  is a `historical` publication. It imports like any other; nothing deletes it
  until its review batch is approved (below).

### Per-file cleanup

Reading a manifest during import, adoption or termination must persist its
provenance before files are seeded, placed or discarded. A database failure
stops that operation and preserves its source. Cleanup independently refuses
missing, malformed or unlisted manifest evidence while payload files remain;
it never turns missing evidence into a discard. Valid stored `v1_record`
manifests remain supported when the old publication has no manifest file.
The historical batch-approval and storage-identity fences still apply.

- **One rule, one implementation.** Curator deletes a publication file only
  when its recorded outcome is evidence: `placed` — the placement recorded
  the manifest's SHA-256 *and* the library file is still the object it
  published (the same inode, or the placement journal's recorded size and
  inode generation; a library file with no placement record, such as a
  historical import or a subtitle, is hashed once) — or
  `discarded{superseded|duplicate|rejected}`. Both outcomes still require
  the source's size and SHA-256 to match its manifest before deletion;
  a matching old library copy cannot authorise deleting replacement source
  bytes. This adds one source verification read at cleanup: placement plus
  cleanup reads about one payload for a hardlink, two for a copied placement.
  Legacy manifests with no digest use their recorded size and stable
  file identity, and that exception is logged. `unresolved` and `pending`
  files are never touched.
- **Re-checked before every unlink**, in order: the generation (the
  directory's `.runner-manifest.json` — or, for a v1 record, the stored
  manifest — must name the publication's generation), the file identity
  (its size and SHA-256 against the manifest, then an unchanged inode
  generation from the verified open descriptor through unlink), and the **storage** of the completed root: its live
  identity against the one the inventory scan recorded (a changed identity is
  an unmounted share), then the storage-change review fences — after an
  accepted replacement volume the recorded identity matches the new one, and
  the fence on what was on the old volume is what keeps automatic deletion
  off it (an operator's own discard is the review it asks for). The unlink
  goes through a directory handle that refuses symlinks. The directory is
  `rmdir`ed when only the manifest is left; a stray Curator cannot account
  for (`.DS_Store`, an NFS silly-rename) keeps it, and once every manifest
  file is gone Storage offers **Delete files…** on it whole (identity and
  ownership checks still apply). The publication is then `removed` and its
  row's payload is gone.
- **Attributable and stoppable**: every pass runs on the cleanup lane (the
  sweep only queues them), shows in `GET /api/v1/system/transfers` as stage
  `publication_cleanup` with why it runs, and on the publication in Activity
  (*Cleanup queued/running (hourly sweep)*) with **Pause cleanup**
  (`POST /api/v1/downloads/publications/{id}/cleanup {"action":"pause"}`);
  a pause cancels the pass and keeps the sweep off the publication until
  **Resume cleanup**.
- **Who runs it**: the cleanup lane right after an import, the hourly sweep
  (publications `imported`/`notified` with evidenced files still on disk),
  Resolve → **Discard**, Download storage → **Delete selected files…** on a
  publication directory (only its unresolved files; they become
  `discarded{rejected}`), and the receipt action **Retry Curator cleanup**.
  None of them asks Runner for anything.
- **Nothing waits for a person.** A placed file whose library copy something
  newer replaced or removed (no library row at that path, a newer placement
  there, a subtitle no longer the one Curator placed) becomes
  `discarded{superseded}` and leaves. A library file whose record remains
  but whose bytes are missing or differ keeps the publication copy (it may be
  the only good one): the reason is recorded on the publication and the
  next pass looks again. Storage problems and a file changing while checked
  are transient the same way.
- **The historical cohort** — every v1 receipt and every completed directory
  present at the first start of a build with publications, plus (migration 0073) every path of the old
  migration-0040 review cohort — is never touched by automatic deletion nor
  by an operator discard until its review batch is approved:
  `POST /api/v1/downloads/publications/approve-cleanup {"ids": [...]}` sets
  `cleanup_approved_at` and the same per-file rule then applies.
- **Activity retention and Clear** hide a row whose publication still has
  files on disk instead of deleting it (a publication cascades with its row);
  the row goes once its publications are removed.
- Directories the cutover froze with no manifest and no download row are
  `import_failed` with that explanation (nothing can import them); once their
  review batch is approved, Storage deletes them whole.
- **Non-Runner clients** keep their whole-payload cleanup (the client's own
  delete, then the guarded local fallback) — they have no manifest. It never
  touches a directory a publication claims.

### If the upgrade refuses to start

One migration refuses rather than guesses, and names what blocks it.

- `migration 0073 refused: live v1 dependents: N pending explicit deletion
  request(s) (<batch ids>)`: nothing has been changed. Deploy the previous
  Curator build (v0.39.x), open Activity → the delivery's lifecycle trace, and
  let each named request finish or **Cancel deletion request**; then deploy
  again. `SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE
  abandoned_at=0` lists them.
Migration 0079 (v0.47.0) refuses nothing: a publication an earlier build
(v0.45.x) recorded with a `v1_record` manifest keeps its stored body and
drains through its import and per-file cleanup; only removed ones are
normalised. A database older than 0072 migrates straight through to the
latest migration.

---

## 6. Telling Cinema — one verified delivery per directory

**What it does.** When an import finishes, Monarr tells plurx the exact
directory to scan, instead of letting plurx discover it on its next scheduled
sweep — and then asks plurx whether the files arrived. One POST **per
directory** — not per file, because a season pack that lands 10 episodes in one
folder is one thing that changed, and ten scans of the same folder is nine
wasted scans. The row that carries it is a **sub-delivery** (below): written in
the import's own transaction, retried until Cinema reports every file placed.

**Wire.** `POST {plurx}/api/v1/scan` with the scoped key as
`Authorization: Bearer plx_…` (plurx accepts `X-Api-Key` too), one request per
sub-delivery (one directory), and body, which also carries
`"request_key":"<transfer>/<generation>/<dir-hash>/<attempt>"`,
`"expected_files":[{"path":…,"bytes":…,"mtime_ns":…}]` and `"wait_ms":10000`
(how they are used is under *How a sub-delivery is sent and read back*, below):

```json
// an episode: the SHOW's id, under `series`
{"path":"/media/tv/Show/Season 02","hint":"episode","series":{"tmdb":1399},
 "correlation_id":"t-42-<32 hex>","source":"monarr"}

// a movie: its own ids, under `ids`
{"path":"/media/movies/Some Film (1999)","hint":"movie",
 "ids":{"tmdb":603,"imdb":"tt0133093"},
 "correlation_id":"t-43-<32 hex>","source":"monarr"}

// a book: exact Curator facts; work_id is the sole edition relationship key
{"path":"/media/books/Andy Weir/Project Hail Mary","hint":"book",
 "book":{"title":"Project Hail Mary","author":"Andy Weir","medium":"ebook",
         "work_id":"curator:openlibrary:OL17091839W",
         "edition_id":"curator:item:84:ebook",
         "cover_url":"https://covers.openlibrary.org/b/id/123-L.jpg"},
 "correlation_id":"t-44-<32 hex>","source":"monarr"}
```

Where the ids go depends on what the path is, and the asymmetry is deliberate:
an episode's own TMDB id is not what identifies the series it belongs to, so
putting the show's id in `ids` would stamp it on the episode row. For a series
Monarr sends the show's TMDB id only — plurx keys episodes off it alone, and
sending an IMDb id there would be inventing precision plurx does not use.
Books carry a separate `book` object, not movie/series ids. Curator sends the
title, author, persisted edition medium, stable work id, stable edition id,
and—when it has one—an exact Open Library HTTPS cover URL. The scanner still
validates the file family. Cinema relates editions only when `work_id` is an
exact match; title and author are display facts, never a fuzzy join. An Open
Library work uses `curator:openlibrary:<OL…W>`; a manual work falls back to
`curator:item:<media-item-id>`, and editions use
`curator:item:<media-item-id>:<ebook|audiobook>`. The cover field is omitted
unless its parsed host is exactly `covers.openlibrary.org`, with HTTPS, no
credentials, and no explicit port.

### How a sub-delivery is sent and read back

Contract section 4; the outbox is `internal/app/notify`, the HTTP exchange is
`internal/adapters/notify/plurx.go`.

- **An outbox, not the bus.** Every notification of an import is a
  `notifier_deliveries` row written in the import's own transaction — the
  publication's completing transaction, or, for a legacy client or a manual
  import, the one that writes its `imported` history row. Nothing travels the
  in-memory bus to be dropped when a subscriber's 32 slots are full.
  (`ImportCompleted` is still published as the UI's live signal; nobody is
  notified from it.) A chat notifier (webhook, Discord) and a Plex/Jellyfin
  poke get one row each, attempted once.
- **One sub-delivery per directory.** Cinema gets one row per `(notifier,
  publication, directory)`, keyed `<transfer>/<generation>/<dir-hash>`
  (the first 16 hex of sha256 of the directory), carrying the media files the
  import placed under that directory with their `bytes` and `mtime_ns` as they
  stood once placed. Cinema takes at most 64 expected files (and 16 KiB of
  them) a request, so a bigger directory sends its largest, newest 64 and the
  log says `(verified 64 of 91 files)`. Each row has its own 20 s deadline per
  attempt, and each notifier is its own lane of up to four deliveries in
  flight: three slow directories are posted side by side, none starves the
  others, a slow third one never makes the first two be posted again, and a
  Discord timing out never holds Cinema's rows behind it. Importing the same
  directory of the same publication again refreshes the row and advances its
  attempt.
- **The request.** `POST /api/v1/scan` carries, beside the unchanged body,
  `request_key: "<key>/<attempt>"`, `expected_files` and `wait_ms: 10000`.
  Cinema answers a repeated key with the same request and result, so a lost
  response does not create a second scan. The attempt is **reused** after an
  ambiguous outcome (a timeout, a connection error, no response, 429, a 502 or
  504, a plain 503) and **advanced** after a confirmed retryable result (an
  expected file not placed, `503 path_unreadable`, a `failed` request — which
  Cinema answers with a 500, first time and on every replay of its key — or a
  request Cinema no longer knows), so the retry gets a fresh scan instead of
  the cached answer. A `Retry-After` on a 503 is honoured: the next try is no
  sooner.
- **Reading the answer.** `200 scanned` is classified at once; `202 queued`
  is polled with `GET /api/v1/scan/requests/{id}` (the creating key may read
  its own requests) on the same schedule. A sub-delivery is **visible** only
  when every expected file comes back `placed` with the same `bytes` and
  `mtime_ns`; a same-path replacement with equal size and the same
  whole-second mtime differs only in `mtime_ns`. Anything else is `warning:
  expected <path> not placed (<cinema's reason>)` and is retried.
- **What is kept.** Each row also stores the item it belongs to
  (`media_item_id`) and Cinema's answer for every expected file
  (`file_results_json`: `placed` with its cinema item, bytes and mtime_ns,
  `skipped` with the reason, or `missing`), so the item page's status panel
  can say which episode of a season pack is in Cinema. **Ask cinema to
  confirm** on the item page (`POST /library/{id}/status/confirm`) writes the
  same sub-delivery rows for the item's (or one episode's) current files,
  with no download and a stable generation (`confirm-<item>` or
  `confirm-<item>-e<episode>`), so asking again refreshes the row and advances
  its attempt; chat notifiers are not told. A retried delivery clears its
  stored file results with the rest of the last answer.
- **The schedule** is the outbox ladder in `internal/domain/backoff` —
  `5s → 30s → 2m → 5m → 15m → 1h`, then hourly, without end for anything
  transient (the same helper, with its own rungs, spaces Runner submission,
  manual admission and the blocked-import retry: `30s → 1m → 2m → 5m`, then
  every 5 minutes). Permanent only on `401`/`403` on the POST,
  `400`, and `422 path_outside_roots` (or an older Cinema's 422 that lists
  its `roots`). A sub-delivery still not visible 30 minutes after it was
  written is shown as a **warning** with Cinema's `waiting_on`, never failed.
- **An older Cinema** ignores the new fields, answers 202, and refuses the
  status read to a `scan:trigger` key (403): the row is done, reading
  `queued (cinema does not report results)`. A current Cinema also answers
  403 for a request id it no longer knows; once a read of the request has
  answered, a later 403 is that — a lost request — and is posted again as a
  new attempt. A 200 without a per-file report
  is done the same way, with the item ids it did report.
- **`import_to_cinema_visible`** starts when the publication is imported (in
  the same transaction, when a Cinema notifier gets a sub-delivery) and ends
  when the last sub-delivery of the import becomes visible. An import Cinema
  cannot report on (an older Cinema), or with a sub-delivery that failed
  permanently, is dropped from the measurement rather than left waiting.
- **Chat notifiers speak for themselves.** Their rows show in their own
  delivery log (every notifier type has one now), name their own type on the
  in-flight view, and write `notify_<type>` on the download's trace;
  `notify_plurx` is Cinema's alone.

**What Monarr holds.** A **scoped key** (`scan:trigger`), never a plurx admin
token. A token *is* a user, so an admin token handed to a neighbouring app
also hands over every secret in plurx's settings. A key carries a scope list
and cannot widen itself. Mint one on the plurx side:

```bash
# There is no cookie to copy out of a browser: log in for an admin token.
TOKEN=$(curl -sS -X POST "$PLURX/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"<admin>","password":"<password>"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

curl -sS -X POST "$PLURX/api/v1/keys" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"monarr","scopes":["scan:trigger"]}'
# the secret is in the response ONCE — plx_… — and is never retrievable again
```

If the notifier reports `plurx rejected the key (401)`, list the keys and look
at `last_used_at`: an empty list means plurx's database was reset (its data dir
is not on a persistent volume), and a key present but never used means the
secret Monarr holds is not that key.

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$PLURX/api/v1/keys"
```

**Where you configure it.** Settings → **Notifications** → add a notifier of
type `plurx`. Fields: `plurx server URL`, `Scoped key (plx_…, scan:trigger)`.
The events column reads `on import (targeted scan)`.

**Where you see it.** The add-notifier form's **Test** button, and the
per-row **Test** → `✓ delivered`, `✓ <note>` or `✗ <msg>`.

**How to read the Test.** It POSTs `{"probe_paths": [<every library root>]}`
and no `path`. Cinema answers which roots it can `stat`; a root it cannot see
fails the Test with both sides named:
`✗ cinema cannot see 1 of 2 library root(s): Curator /data/media/movies ↔
cinema: not under any of its library roots (/media/movies, /media/tv)`. Fix
the mount mapping before the first import rather than finding it one import
at a time. Nothing is scanned.

An older Cinema requires `path`, and its JSON extractor rejects the body with
its own 422 (no `roots` in it). The Test then falls back to the legacy probe —
a scan for `/monarr/connection-test`, which cannot resolve under any library
root, counting the handler's *"under any library root"* rejection as the pass
(URL, key and scope all work) — and reads `✓ connected; checking the library
paths needs a newer cinema`. With no library roots configured it runs the
legacy probe and says so.

---

---

### The delivery queue — the notification survives a restart

**What it does.** Notifications are `notifier_deliveries` rows in SQLite,
written in the import's own transaction, with the retry schedule as a column,
not as a sleeping goroutine. For Cinema the backoff is the outbox ladder above,
hourly without end for a transient failure; a chat notifier's row is attempted
once. Restart Monarr mid-retry and the retry still happens; a
delivery queue that lives in memory is a queue that silently loses everything
on the one event you most want it to survive.

**Where you see it.** Settings → **Notifications** → per-row **Delivery log**
(every notifier type; it is an inline expander on `/settings`, not a page of
its own, and scrolls sideways inside its own box on a phone). Columns `When` / `Event` /
`Directory` (with `attempt N` once a fresh scan was needed) / `Status` /
`Detail`, plus a **Retry** button on failed rows, polled every 5 s. **Retry
all failed** above the table requeues every failed row of the notifier — the
whole history, not only the 100 rows shown — and reports how many it
requeued. Retry keeps a sub-delivery's key and advances its attempt, so
Cinema scans afresh.

| Status pill | Detail reads |
|---|---|
| `visible` | `visible → plurx item 1201` — Cinema reported every expected file placed with the bytes and mtime the import left |
| `ok` | the result: `queued (cinema does not report results)` from an older Cinema, `scanned → plurx item 1201` from a row written by an earlier release, or `delivered` |
| `pending` | `queued in cinema, waiting on full_scan:3, retrying in 5m` (Cinema's exact `waiting_on`), `warning: expected <path> not placed (<cinema's reason>), retrying in 1h`, `attempt N failed: <error>, retrying in Xm`, or `queued` |
| `warning` | the same, still not visible 30 minutes after the import — surfaced, still retried |
| `failed` | the last error: a rejected key, a 400, a directory outside Cinema's roots |

`×N` appears beside the pill when attempts exceeded one. The API row carries
`directory`, `attempt`, `requestKey`, `remoteRequestId`, `remoteState`,
`waitingOn`, `visibleAt`, `itemIds`, `expectedFiles` and `warning`.

**How to verify.**

```bash
curl -sS -H "X-Api-Key: $KEY" "$MONARR/api/v1/notifiers/<id>/deliveries" | python3 -m json.tool
```

**How to read it.** `pending` with a climbing attempt count is plurx being
down or the share unmounted — it will drain itself, however long that takes.
`failed` does not retry on its own: Monarr classifies permanent failures (a
rejected key, a 400, `422 path_outside_roots`) separately from retryable ones
(429, `503 path_unreadable`, a 5xx, a timeout), because retrying a wrong API
key every hour forever is not resilience, it is noise. A failed row is
retried when somebody asks — **Retry** on the row, **Retry all failed** for
the notifier, or `POST /api/v1/notifiers/<id>/deliveries/<deliveryId>/retry`
and `POST /api/v1/notifiers/<id>/deliveries/retry` — which puts it back to
`pending`, due now, with the schedule starting over and the last error kept
until the next attempt replaces it. Permanent failures requeued this way fail
again on their first attempt; a disabled notifier answers 409 (it does not
fire on an import, so it does not fire on request). `No deliveries yet —
this notifier fires after an import.` on a fresh install is correct, not
broken.

---

### Moves (Re-match, 0.48.0)

A Re-match ([ADR 0027](adr/0027-rematch-file-moves.md), contract §4.4) writes
one sub-delivery per notifier **per library root touched**, keyed
`op/<operation-id>/<dir-hash>` (no publication; `operation_id` on the row),
event `moved`. `path` is the common parent of the files placed and the files
that left; the body adds `"event":"moved"` and
`"removed_paths":["…/Dune (1984)/Dune (1984) [Bluray-2160p].mkv"]` beside the
usual `expected_files`. Cinema probes exactly those paths (no walk, no full
scan) and answers each removed path `gone`, `unknown` or `present`. The row is
visible when every expected file is `placed` and every removed path is `gone`
or `unknown`; a `present` path is `warning: <path> still exists in the
library`, retried on the schedule.

Whether Cinema supports this is its connection test's
`"features":["removed_paths"]`, stored on the notifier at **Test** and re-read
before a session's first `moved` send. Without it (or when an answer carries
no entry for a removed path — the backstop, which also drops the stored
feature) Curator sends the **new folder only**, as a plain scan, and the
delivery reads `visible …; warning: cinema does not support removed paths; the
old entry stays until its next scan`. Not a failure; nothing retries. Chat
notifiers get one `moved` message. `GET /api/v1/rematch/{id}` lists the
operation's sub-deliveries with their results.

### Offload (0.49.0)

An Offload ([ADR 0028](adr/0028-offload.md), contract §4.5) writes the same
operation-keyed sub-deliveries as a move (`op/<operation-id>/<dir-hash>`, one
per notifier per library root, every chunk `probe_only`) with
`"event":"offload"` and `"offloaded_paths":[…]`. They are written **held** in
the Offload's start transaction and released when its last file is deleted,
so Cinema never reads `present` for a file that has not been deleted yet.
Cinema keeps the title and its watch history, greys it out, and answers each
path `offloaded`, `unknown` or `present`.

Curator sends it only when the notifier's stored features include `offload`
(read at **Test**, re-read before the send). Without it: a Cinema with
`removed_paths` gets plain `removed_paths` (it will drop the title and its
watch history; the Offload dialog says so beforehand), a Cinema with neither
gets nothing. The Developer tab shows, per Cinema notifier, whether `removed
paths` and `offload` are advertised and when that was read. It is advice;
nothing is switched off by it.

## 7. plurx webhook — the only inbound push Monarr accepts

**What it does.** plurx tells Monarr what was watched, and by whom. Monarr
**records and displays it**, and uses it to order upgrade work toward actively
watched shows. That is the whole of it.

**Wire.** `POST /api/v1/webhooks/plurx`, header `X-Api-Key: <monarr key>`:

```json
{"event":"watched","kind":"episode","tmdb":1399,"season":2,"episode":5,
 "watched_at":1753500000,"user":"paul"}
```

Three properties are deliberate:

- **Ids only.** The item is matched by TMDB or IMDb id, never by title.
  An application guessing which item you meant is exactly the failure this
  integration was built to remove; accepting a title would reintroduce it from
  the other direction. A body with neither id gets a 400 saying so.
- **Unknown is not an error.** A notification for something Monarr does not
  have returns **200 with `matched: false`**, not a 404 — plurx's outbox
  retries failures, and there is nothing to retry about a film Monarr was never
  asked to manage.
- **One acting event.** `watched` is recorded, shown, and read by the upgrade
  ordering; it deletes nothing. The single event that makes Monarr act is
  `reacquire` (below): an admin in plurx asked for an offloaded title back.
  It only ever brings files back; no cleanup policy, no unmonitoring and no
  auto-delete path exists, and Offload is started from Monarr's own UI.

For an episode, `tmdb` is the **show's** id — an episode's own id does not
identify the series it belongs to.

### `reacquire` and `reacquire_status` (0.49.0)

```json
{"event":"reacquire","kind":"season","tmdb":95396,"season":2,"user":"paul"}
```

`kind` is `movie`, `show`, `season` or `episode`; ids as above (a movie by
its tmdb or imdb id; the rest by the **show's** id plus `season` and
`episode`); `user` is plurx's user, for the log. It clears *offloaded* inside
the scope, monitors it, cancels the Offload's unconfirmed Cinema requests and
searches at the current profile (a season pack-first), answering at once:

```json
{"matched":true,"mediaItemId":812,"state":"searching"}
```

`state` is `searching`, `downloading`, `importing`, `on_disk`, `not_offloaded`
(nothing in the scope is offloaded), `offloading` (an Offload of it is
running), `offloaded` (still offloaded; Re-acquire not started, ask again) or
`not_found` (with
`matched:false`: no such title, season or episode). No ids, a `season` or
`episode` without its numbers, or an unknown kind is a 400. It is idempotent:
asking again answers where the title stands. `reacquire_status` with the same
body reads that answer and changes nothing. A curator older than 0.49.0
answers without `state`.


**Where you see it.** System → **Connections**, kind `calls Monarr`, name
`plurx`, state `calling` — detail `last called /api/v1/webhooks/plurx (N since
startup)`.

**How to verify.**

```bash
curl -sS -X POST "$MONARR/api/v1/webhooks/plurx" \
     -H "X-Api-Key: $KEY" -H "Content-Type: application/json" \
     -d '{"event":"watched","kind":"movie","tmdb":603,"watched_at":1753500000,"user":"test"}'
# -> {"matched":true}  if The Matrix is in your library, {"matched":false} if not
```

**How to read it.** `matched: false` is not a failure — it is Monarr saying
"not mine". If you get it for something you *know* is in the library, the item
is missing its TMDB/IMDb id, not the webhook is broken.

---

## 8. Calendar and status reads — plurx as a client of Monarr

**What it does.** plurx reads two endpoints: `GET /api/v1/calendar?start=&end=`
for its home-screen **Coming soon** rail, and `GET /api/v1/system/status` as
the target of its **Test connection** button. Both use `X-Api-Key`.

Calendar entries carry `tmdbId` and `imdbId` — the SHOW's, for an episode —
so plurx can resolve each one against its own library by id and put its own
poster on the card. Ids rather than titles, for the same reason as everywhere
else: an application guessing which item you meant is the failure this
integration exists to remove. Monarr sends no artwork and plurx fetches none;
a series whose next episode is airing is already in plurx's library, and its
poster is already cached there.

**Where you see it.** System → **Connections**, kind `calls Monarr`. The same
`plurx` row as §7 — the registry is per-caller, not per-endpoint, and `Detail`
names the last path so you can tell which half is active.

**How it identifies a caller.** The `noteCallers` middleware records only
requests presenting `X-Api-Key` (header or `?apikey=`), names the row from the
User-Agent product token (`plurx` out of `plurx/0.4.1 (…)`), and excludes
browsers. Sessions and UI traffic never appear — the panel is for applications.

**How to read it.** `calling` means a call within the last hour;
`quiet` means it has called since Monarr started but not lately, with the
detail `nothing since <timestamp> — last called <path>`. The registry is
**in-memory and resets on restart**, so an empty inbound list right after a
restart means nothing has called *yet*, not that nothing is configured. Give
it a few minutes — plurx's coming-soon cache is 15 minutes wide.

---

## 9. Connections panel — the one screen that shows both directions

**Where.** **System → Connections**. Blurb: *"The other applications Monarr
talks to — and the ones that talk to it."* Refreshes every 10 s; the
underlying probe runs every minute. It serves the **last** snapshot and does
not probe on request, so `Last probed …` is part of the reading.

| Kind cell | Means |
|---|---|
| `download client` | Monarr polls it |
| `media server` | Monarr pushes to it |
| `calls Monarr` | it calls Monarr — inbound, §7 and §8 |

| State | Colour | Means |
|---|---|---|
| `live` | ok | a push stream is open |
| `polling` | ok | answering, on the 30-second poll |
| `degraded` | warn | answering, but not working properly |
| `unreachable` | warn | not answering |
| `unprobed` | neutral | configured, never probed — its only test is the action itself |
| `calling` | ok | this application has called Monarr recently |
| `quiet` | neutral | it has called since startup, but not lately |

The panel has **no test button**, by design: it reflects a background probe
plus an observation log, and both are records of what actually happened rather
than a synthetic check you have to remember to run. The per-row **Test**
buttons live on the Settings pages, next to the config they exercise.

**`degraded` on age alone.** A client that answers its test but has not
produced anything for five minutes degrades with the detail *"answering, but
nothing has come through it for Nm"*. Monarr polls every enabled client every
30 seconds whether or not it has anything in flight, precisely so that this
clock means what it says — an idle client is still polled, because a poll that
skipped idle clients produced a permanent ERROR on any instance that had simply
finished everything.

## 10. In flight — the data plane, reported separately

**Where.** **System → In flight**, under Connections. Refreshes every 2
seconds, because it is the panel that moves.

**What it does.** Answers one question the rest of this document does not:
*is anything actually moving right now*. Columns: title and transfer id,
stage, progress with bytes and rate, elapsed, detail.

| Stage | Means |
|---|---|
| `downloading · nzbd` | nzbd is fetching it. Monarr is watching, not working |
| `importing` | Monarr is moving bytes into the library itself |
| `notifying · plurx` | telling plurx what landed, including waits between retries |

`importing` has no peer, because Monarr is moving its own files and naming one
would invent a seam that does not exist.

**Why it is not part of Connections.** Those are two different questions and
conflating them produced a specific, memorable failure. A 20 GB import ran
*inside* the queue poll, held it open for seventeen minutes, and every
control-plane signal that reads that loop — the contact clock, the task
duration, the next-run time — reported a file copy instead of a connection.
The panel degraded the download client Monarr was importing **from**, while
the Test button beside it passed, because the probe is a separate call. And
the import itself was visible nowhere: the only evidence in the whole process
was the duration column of an unrelated scheduled task.

So imports run on their own bounded worker pool, the poll
returns in milliseconds whatever the library is doing, and the data plane
reports itself here. Two workers import; payload cleanup has two workers of
its own, because cleanup re-reads both copies before deleting one (tens of
gigabytes for a remux) and that must never hold up an import. Library
placement is serialized per title: two imports of the same title publish one
at a time, but a long placement (a 68 GB remux over a busy NAS link) never
holds up another title's import or cleanup. Operator deletes by
completed-folder path and the placement journal's reconciliation at start
still take the whole library. Control-plane checks (`client:<name>`) only ever mean
"can Monarr reach this"; data-plane trouble has its own check (`imports`),
which warns when an import has been running long enough to be stuck rather
than merely slow.

**How to verify.**

```bash
curl -sS -H "X-Api-Key: $KEY" "$MONARR/api/v1/system/transfers" | python3 -m json.tool
```

**How to read it.** `not measurable` in the progress column is deliberate and
is not 0% — a notify has no byte count, and an empty progress bar would claim
nothing had happened. `bytesPerSecond` is averaged over the life of the
transfer rather than instantaneous: an instantaneous rate on a network mount
is mostly noise, and the question being asked is "will this finish tonight".

---

System → **Health** carries the same information as named checks —
`client:<name>`, `client:<name>:capacity`, `mediaserver:<name>` — with `ok` /
`warning` / `error` and a message. A connection answering but delivering
nothing goes `warning` after 5 minutes and `error` after 30.

---

## Metrics

`GET /metrics` (no auth header; requires `MONARR_METRICS=1`). Names:
`monarr_build_info{version,commit}` · `monarr_media_items{kind}` ·
`monarr_queue_active` · `monarr_wanted_total` · `monarr_uptime_seconds`.

Pipeline timings (contract section 3.5; one histogram per seam over the last 24 h):
`monarr_failure_to_next_grab_seconds` · `monarr_runner_done_to_import_seconds` ·
`monarr_import_to_cinema_visible_seconds` · `monarr_pipeline_waiting{kind}` ·
`monarr_pipeline_overdue_rows` · `monarr_runner_fact_mismatch_total` (facts
dropped because their transfer token was not the row's) ·
`monarr_pipeline_scrape_errors_total`.

Per seam, with the in-flight view:

| Metric | Reads |
|---|---|
| `monarr_transfers_in_flight{stage="downloading\|importing\|notifying"}` | work in flight at each seam |
| `monarr_transfer_bytes_moved` | bytes those transfers have moved so far |
| `monarr_transfer_bytes_total` | bytes they are expected to move |

Before these, Monarr — the application in the *middle* of the pipeline —
exposed no integration counters at all, so the pipeline could not be joined in
Prometheus from the one vantage point that sees both seams. nzbd has
`nzbd_events_emitted_total{event}` / `nzbd_sse_clients`; plurx has
`plurx_scan_total{trigger}` / `plurx_notify_received_total` /
`plurx_watched_outbox{status}`.

Still only in JSON: inbound caller counts (`/system/connections`) and
per-delivery outcomes (the delivery log).

---

---

## Verify the whole chain in five minutes

```bash
NZBD=http://127.0.0.1:6789;  TOK=<nzbd api.token>
MONARR=http://127.0.0.1:7676; KEY=<Access → API access → Reveal>
PLURX=http://127.0.0.1:32400
```

1. **nzbd is up and honest** — `curl -sS "$NZBD/healthz"` → `ok`, then
   `/api/v1/status` with auth and confirm a `version` field.
2. **Monarr sees nzbd** — System → Connections shows it `polling` or `live`.
   If `live` was expected and you got `polling`, check nzbd's client strip for
   a `subscribed` chip.
3. **Monarr can reach plurx** — Settings → Notifications → the plurx row's
   config, Test → the rejection mentioning *"under any library root"* is a pass.
4. **plurx can reach Monarr** — on plurx: Settings → Metadata → monarr card →
   **Test connection** → `✓ connected · monarr <version>`.
5. **plurx is actually calling** — back on Monarr: System → Connections should
   now list `plurx` as `calls Monarr` / `calling`.
6. **Grab something** and watch it end to end: Activity → the handoff trace
   walks `Grabbed → Downloading → Downloaded → Importing → Imported`, then
   Settings → Notifications → Delivery log shows `visible` with
   `visible → plurx item …`, and the file appears in plurx without you
   touching a scan button.

If step 6 stalls, the trace tells you which seam: no `Downloaded` is nzbd, no
`Imported` is Monarr's import, and `Imported` with an empty delivery log is the
notifier not configured at all.

---

## What Monarr deliberately does not do

- **Never holds a plurx admin token.** A scoped `scan:trigger` key only. See
  §6 for why the distinction is not cosmetic.
- **Never invents provider ids for books.** Book imports reach Cinema with an
  exact path and `hint:"book"`; Cinema's Books library identifies them from
  disk rather than pretending TMDB knows publications.
- **Never deletes anything on watch state.** No cleanup policy, no
  unmonitoring, no auto-delete path exists — not disabled, not built.
- **Never matches on titles across a seam.** Ids only, in both directions.
- **Never masks a secret only in the UI.** API keys are redacted in logs and
  in every error string that could carry a URL with credentials in it.
- **Never lets push become load-bearing.** Every push path has a poll
  underneath it, so an outage in the stream costs latency and nothing else.

## Literals this page quotes, and who checks them

Every literal in this document — state names, pill text, error strings, wire
fields, column names — was read out of the code on the branch that ships it.
The ones below are checked by `internal/docs` (`go test ./internal/docs`),
which fails when the page, the code and the table disagree. When a seam
changes, this file changes in the same commit.

| Literal | Value | Source |
|---|---|---|
| Runner stream events acted on | `job_pp_finished`, `job_status_changed`, `job_control_changed`, `job_publication_ready`, `job_pp_stage`, `tick`, `reset`, `lagged` | `internal/adapters/nzbd/events.go` |
| Event deliberately ignored | `job_finished` | `internal/adapters/nzbd/events.go` |
| Event that is advisory only | `job_deleted` | `internal/adapters/nzbd/events.go` |
| Submission, manual-admission and import-retry ladder | `30s, 1m, 2m, 5m`, then every 5m | `internal/domain/backoff` |
| Outbox ladder | `5s, 30s, 2m, 5m, 15m, 1h`, then hourly | `internal/domain/backoff` |
| Per-attempt deadline of a sub-delivery | 20 s | `internal/app/notify/deliver.go` |
| Deliveries in flight per notifier | 4 | `internal/app/notify/deliver.go` |
| Sub-delivery shown as a warning after | 30 minutes | `internal/app/notify/deliver.go` |
| `failed{lost}` after | 10 minutes of successful polls | `internal/app/acquisition/reducer.go` |
| Publication `import_failed{stalled}` after | 6 hours | `internal/app/acquisition/reducer.go` |
| Replacement grab worker tick | 5 s | `internal/app/acquisition/replacement_grab_worker.go` |
| Indexer-outage re-queue | 15 minutes | `internal/app/acquisition/reducer.go` |
| Pipeline timing kinds | `failure_to_next_grab`, `runner_done_to_import`, `import_to_cinema_visible` | `internal/api/openapi.yaml` |
| Cinema scope Monarr holds | `scan:trigger` | `internal/adapters/notify/plurx.go` |
| Ports | nzbd 6789 · Monarr 7676 · plurx 32400 | `docs/settings.md` |
