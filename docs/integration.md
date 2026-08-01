# Integration — Monarr's seams with nzbd and plurx, and how to prove they work

Companion to [plan-integration.md](plan-integration.md) (the master plan and
why each seam exists) and [settings.md](settings.md) (every field) — this is
*what each seam does, where you look at it, and the exact command that proves
it*.

Monarr is the **middle** of a three-application pipeline. It is the only one
that talks to both of the others, and the only one that holds the identifier
tying a grab to a playable file. When something in the chain is broken, Monarr
is where you look first — not because it is the likely culprit, but because it
is the only vantage point that sees both directions.

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
| 2 | nzbd event stream | outbound (held open) | SSE | Monarr |
| 3 | The transfer id | outbound, both seams | job param → `correlation_id` | Monarr |
| 4 | plurx targeted scan | outbound | `POST /api/v1/scan` | Monarr, on import |
| 5 | Delivery queue | internal, durable | SQLite | Monarr |
| 6 | plurx watch webhook | **inbound** | `POST /api/v1/webhooks/plurx` | plurx |
| 7 | Calendar + status reads | **inbound** | `GET /api/v1/calendar`, `/system/status` | plurx |
| 8 | Connections panel | observation | `GET /api/v1/system/connections` | you |
| 9 | In-flight transfers | observation | `GET /api/v1/system/transfers` | you |

Sections 1–5 are things Monarr does. Sections 6–7 are things done to it — and
they are the half that used to be invisible, which is why §8 exists.

---

## 1. nzbd native client — the rich path, not the compat path

**What it does.** The `nzbd (native)` client type drives nzbd's own REST API
instead of the NZBGet-compatible shim. That buys three things the shim cannot
carry: job params (§3), a real event stream (§2), and capacity reporting.

**Wire.** `POST /api/v1/jobs` to add · `GET /api/v1/jobs` for the queue ·
`GET /api/v1/history?limit=100` · `POST /api/v1/jobs/{id}/actions/delete` ·
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

## 2. nzbd event stream — latency, never correctness

**What it does.** With **Live updates** ticked
(`download_clients.mode = 'push'`), Monarr holds nzbd's SSE stream open and
imports the moment post-processing finishes, instead of up to 30 s later.

**What Monarr acts on.** Six event names out of the eleven nzbd emits:
`job_pp_stage` (step trace), `job_pp_finished` (completed/failed, carrying
`final_dir`), `job_deleted` (failed — *"removed in nzbd"*), `reset` and
`lagged` (both → immediate full queue refresh), `tick` (progress). Everything
else is dropped. `job_finished` is **deliberately ignored**: it fires when the
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

## 3. The transfer id — one identifier, grab to playable

**What it does.** At grab time Monarr mints `t-<downloadID>-<6 hex>` and
attaches it to everything downstream, so a single `grep` across three
applications reconstructs one transfer.

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

## 4. plurx targeted scan — "this exact folder changed"

**What it does.** When an import finishes, Monarr tells plurx the exact
directory to scan, instead of letting plurx discover it on its next scheduled
sweep. One POST **per directory** — not per file, because a season pack that
lands 10 episodes in one folder is one thing that changed, and ten scans of
the same folder is nine wasted scans.

**Wire.** `POST {plurx}/api/v1/scan` with the scoped key as
`Authorization: Bearer plx_…` (plurx accepts `X-Api-Key` too), and body:

```json
// an episode: the SHOW's id, under `series`
{"path":"/media/tv/Show/Season 02","hint":"episode","series":{"tmdb":1399},
 "correlation_id":"t-42-a3f9c1","source":"monarr"}

// a movie: its own ids, under `ids`
{"path":"/media/movies/Some Film (1999)","hint":"movie",
 "ids":{"tmdb":603,"imdb":"tt0133093"},
 "correlation_id":"t-43-b1e207","source":"monarr"}
```

Where the ids go depends on what the path is, and the asymmetry is deliberate:
an episode's own TMDB id is not what identifies the series it belongs to, so
putting the show's id in `ids` would stamp it on the episode row. For a series
Monarr sends the show's TMDB id only — plurx keys episodes off it alone, and
sending an IMDb id there would be inventing precision plurx does not use.

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

**Where you see it.** The add-notifier form's **Test** button →
`✓ delivered` / `✗ <msg>`.

**How to read the Test.** It POSTs a scan for the path
`/monarr/connection-test`, which cannot resolve under any library root, and
**counts the resulting rejection as a pass**. That is deliberate: it exercises
URL, key and scope without kicking off a real scan. A rejection mentioning
*"under any library root"* means the pairing is correct.

---

## 5. Delivery queue — the notification survives a restart

**What it does.** Notifications are enqueued in SQLite with the retry
schedule as a column, not as a sleeping goroutine. Backoff is
`5s → 30s → 2m`. Restart Monarr mid-retry and the retry still happens; a
delivery queue that lives in memory is a queue that silently loses everything
on the one event you most want it to survive.

**Where you see it.** Settings → **Notifications** → per-row **Delivery log**
(media-server types only — `plex`, `jellyfin`, `plurx`; it is an inline
expander on `/settings`, not a page of its own). Columns `When` / `Event` /
`Status` / `Detail`, polled every 5 s.

| Status pill | Detail reads |
|---|---|
| `ok` | the result, e.g. `scanned → plurx item 1201` |
| `pending` | `attempt N failed: <error>, retrying in Xm`, or `queued` |
| `failed` | the last error |

`×N` appears beside the pill when attempts exceeded one.

**How to verify.**

```bash
curl -sS -H "X-Api-Key: $KEY" "$MONARR/api/v1/notifiers/<id>/deliveries" | python3 -m json.tool
```

**How to read it.** `pending` with a climbing attempt count is plurx being
down — it will drain itself. `failed` is terminal and will not retry:
Monarr classifies permanent failures (a rejected key, a 4xx that will never
become a 2xx) separately from retryable ones, because retrying a wrong API key
every two minutes forever is not resilience, it is noise. `No deliveries yet —
this notifier fires after an import.` on a fresh install is correct, not broken.

---

## 6. plurx watch webhook — the only inbound push Monarr accepts

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
- **Nothing here deletes anything.** No cleanup policy, no unmonitoring, no
  auto-delete path exists in the code at all. Recorded, shown, and read by the
  upgrade ordering.

For an episode, `tmdb` is the **show's** id — an episode's own id does not
identify the series it belongs to.

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

## 7. Calendar and status reads — plurx as a client of Monarr

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
`plurx` row as §6 — the registry is per-caller, not per-endpoint, and `Detail`
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

## 8. Connections panel — the one screen that shows both directions

**Where.** **System → Connections**. Blurb: *"The other applications Monarr
talks to — and the ones that talk to it."* Refreshes every 10 s; the
underlying probe runs every minute. It serves the **last** snapshot and does
not probe on request, so `Last probed …` is part of the reading.

| Kind cell | Means |
|---|---|
| `download client` | Monarr polls it |
| `media server` | Monarr pushes to it |
| `calls Monarr` | it calls Monarr — inbound, §6 and §7 |

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
clock means what it says — before 2026-07 the poll skipped idle clients, and
the result was a permanent ERROR on any instance that had simply finished
everything. If you are on an older build, read a stale-contact WARNING/ERROR
as "Monarr has nothing downloading", not as a fault.

## 9. In flight — the data plane, reported separately

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

So since 2026-07: imports run on their own bounded worker pool, the poll
returns in milliseconds whatever the library is doing, and the data plane
reports itself here. Control-plane checks (`client:<name>`) only ever mean
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

Per-seam, added 2026-07 with the in-flight view:

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
   Settings → Notifications → Delivery log shows `ok` with
   `scanned → plurx item …`, and the file appears in plurx without you
   touching a scan button.

If step 6 stalls, the trace tells you which seam: no `Downloaded` is nzbd, no
`Imported` is Monarr's import, and `Imported` with an empty delivery log is the
notifier not configured at all.

---

## What Monarr deliberately does not do

- **Never holds a plurx admin token.** A scoped `scan:trigger` key only. See
  §4 for why the distinction is not cosmetic.
- **Never sends books to plurx.** plurx does not do books; a scan request for
  one is declined with an explanatory trace entry rather than silently dropped.
- **Never deletes anything on watch state.** No cleanup policy, no
  unmonitoring, no auto-delete path exists — not disabled, not built.
- **Never matches on titles across a seam.** Ids only, in both directions.
- **Never masks a secret only in the UI.** API keys are redacted in logs and
  in every error string that could carry a URL with credentials in it.
- **Never lets push become load-bearing.** Every push path has a poll
  underneath it, so an outage in the stream costs latency and nothing else.

## Keeping this honest

Every literal in this document — state names, pill text, error strings, wire
fields, column names — was read out of the code on the branch that ships it.
When a seam changes, this file changes in the same commit.
