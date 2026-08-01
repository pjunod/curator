# Settings reference

Everything on the Settings and System pages, field by field. Process-level
configuration (port, data dir, log level) is environment-based and lives in
the [README](../README.md#configuration).

## Metadata provider (TMDB)

One key serves movies and TV. Paste a TMDB **v3 API key** or a **v4 read
access token** (the long `eyJ…` one) — both are accepted. The key is stored
in the database, takes effect immediately (no restart), and is never
returned by the API in full — only a last-4 hint. Books don't use TMDB;
Open Library needs no key.

### Extra ratings (OMDb — optional)

A free OMDb key (omdbapi.com, 1000 req/day) adds Rotten Tomatoes, IMDb,
and Metacritic scores to items with an IMDb id, on add and on every
metadata refresh. Responses are cached for 12 h to respect the quota.
Without a key, TMDB/Open Library ratings still work.

### Trakt client id (optional)

A free Trakt app client id (trakt.tv/oauth/applications — no account link,
no OAuth) adds five rows to the [Discover](usage.md#discover) page that
TMDB cannot answer: what is being played right now for movies and for
shows, what is most anticipated on each, and last weekend's box office.
Trakt's "trending" counts scrobbles — actual playback — where TMDB's counts
lookups, which is why both are offered rather than merged (ADR
[0015](adr/0015-discovery.md)).

Without it, Discover still serves its nine TMDB rows and never mentions
Trakt. Import lists keep their own **per-list** client id (see
[Import lists](#import-lists)) and are unaffected either way: a list that
works today keeps working with this field blank.

## Root folders

Absolute paths, must exist and be directories when added. Each shows
accessibility and free space. Deleting a root folder only removes the
registration — nothing on disk is touched. Items keep their absolute paths,
so re-adding the same root restores the association.

## Indexers

Torznab (torrent) and Newznab (usenet) share one implementation.

| Field | Meaning |
|---|---|
| Name | display name |
| URL | API root **without** `/api` — Monarr appends it (`https://api.drunkenslug.com`, or a Prowlarr feed like `http://prowlarr:9696/1`) |
| API key | the indexer's key |
| Protocol | `torrent` or `usenet` — decides which download client gets grabs |
| Categories | optional comma-separated Newznab category ids; empty = server defaults (book searches then pin 7000/7020/3030 automatically) |

**Test** performs a `t=caps` request. Prowlarr users don't need to add
feeds manually — point Prowlarr at the compat personalities and its app
sync pushes them in.

## Download clients

| Type | Protocol | Credentials |
|---|---|---|
| qBittorrent | torrent | username + password (WebUI); blank if auth bypassed |
| Transmission | torrent | username + password (RPC); blank if auth off |
| Deluge | torrent | web password only |
| SABnzbd | usenet | API key (in the password field) |
| NZBGet | usenet | username + password (defaults `nzbget`/`tegbzn6789`); **leave both blank if you've disabled auth** (empty `ControlPassword`) |
| nzbd (native) | usenet | a token in the password field with the username blank, or username + password |

Blank credentials mean "send no auth at all" — valid whenever the client
itself doesn't require it.

`Category` (default `monarr`) tags/labels downloads where the client
supports it. Grabs route by protocol: a torrent release goes to the first
enabled torrent client, usenet to the first enabled usenet client. The
queue is polled every 30 s; completed items import automatically unless the
client requires approval (below).

### nzbd, natively (`nzbd` vs `NZBGet`)

[nzbd](https://github.com/pjunod/nzbd) speaks NZBGet's API, so it works as
an `NZBGet` client and always has. The `nzbd (native)` type talks to its
own `/api/v1` instead, which buys three things the compat API cannot
express:

- **A transfer id on every download.** Monarr tags each grab `t-<id>-<hex>`
  and nzbd carries it on the job, into its history, and onto its completion
  event. Grep that one string in either application's log and the whole
  story comes back.
- **Post-processing detail.** The Activity trace says *"post-processing:
  par verify"* rather than sitting on "downloading" for the ten minutes a
  repair takes.
- **Live updates** (below), which need an event stream compat does not have.

Everything else behaves identically, and switching an existing client's
type is safe: in-flight downloads reconcile by handle either way.

### Live updates (per client, default off)

**Live updates** holds the client's event stream open, so a finished
download is imported the moment post-processing ends instead of up to 30
seconds later. Only the native `nzbd` type can stream today; the checkbox
only appears for it.

**Push never replaces the poll.** The 30 s sweep keeps running underneath,
and it is what notices a stream that died quietly. So the worst case for a
client set to Live is exactly the behavior it had before — latency, never
correctness. If the stream drops, Monarr reconnects with backoff and
resumes from where it left off; if the gap is too big to replay (nzbd
restarted, or Monarr was away longer than nzbd's buffer), it reconciles by
polling before trusting the stream again.

The client list shows `live` or `poll` per client, and the Activity trace
names the channel that delivered each step — *"…finished; payload at /x
(event 913)"* versus *"(poll)"*. That is the difference between knowing
push works and assuming it does.

**Approve imports** (per client, default off) holds every completed
download at *awaiting approval* instead of importing it — nothing touches
the library until you press **Import now** on the Activity page. Turn it on
with the checkbox on the client's row (or when adding it) for a client
whose completed files you want to eyeball first. The toggle round-trips
without re-entering the password.

### Completed downloads: where files land, and how Monarr finds them

The "download finished" folder is **configured in the client itself**, not
in Monarr — NZBGet's `DestDir` (plus per-category dirs), SABnzbd's
"Completed Download Folder", qBittorrent's save path. When a download
finishes, the client reports that path over its API and Monarr imports the
media files from it into the item's library folder (hardlink when both
sides share a filesystem, copy otherwise).

That reported path must be **openable by Monarr as-is**:

- Same host / same container namespace: nothing to do.
- Docker: mount the download folder into the Monarr container at the same
  path the client reports (identical volume mounts across containers).
- Client on a different host (or mounted differently): add a **remote
  path mapping** on the client — `remote` is the prefix the client
  reports, `local` is where Monarr sees the same files. Example: NZBGet on
  another box says `/data/completed/...`, the share is mounted in Monarr's
  container at `/pool/downloads/...` → map `/data/completed` →
  `/pool/downloads`.

### When an import can't proceed

A completed download that Monarr can't place is **not** silently dropped or
blocklisted — the release downloaded fine, so the problem is local and
fixable. The queue row goes to **failed** with the exact reason, and
expanding it on the Activity page shows the whole handoff: the path the
client reported, the path Monarr looked in after any mapping, and where it
stopped. Common cases:

- `payload missing` — the reported path isn't visible to Monarr. Fix the
  mount or add a remote path mapping (above), then press **Retry**.
- `no media files in …` — the path is visible but holds nothing importable
  (wrong folder, or an archive Monarr doesn't unpack). Point a **Manual
  import** at the right folder.
- `import stopped: N of M files placed …` — the destination could not take
  the bytes (a full volume, a mount gone read-only). The files that landed
  are in the library; the payload is deliberately left alone, and the
  `downloads.import-retry` task finishes the job by itself once there is
  room (every 15 minutes, up to six attempts). Free space is the only
  action needed. This is the one import failure Monarr retries on its own:
  everything else fails identically however many times it is repeated.

Every failed row offers **Retry** (re-run the import once the underlying
problem is fixed, using the same path), **Manual import** (browse to the
real files and pick the target title/copy yourself), **Blocklist** (declare
the release bad — blocklists it and searches a replacement), and **Remove**.
Nothing is auto-blocklisted on an import failure, so a config problem never
churns through replacements behind your back.

When the failures are no longer useful, **Clear failed** on the Failed
grouping dismisses all failed Activity rows after confirmation. This cleans
up the queue view only: it does not delete a payload or library file, change
a blocklist, or remove anything from a download client. The native app shows
the same action beneath Activity's filters when Failed is selected.

### Activity retention

How long finished and failed downloads — and the history events behind them
— are kept before the daily `activity.retention` sweep ages them out.
Default **30 days**; **0** keeps everything, which is a choice rather than
the absence of one.

Only terminal rows age out. Anything still moving or awaiting a decision is
never swept, whatever its age: a download that has been stuck for six weeks
is a thing to investigate, not litter.

### Seeing what happened to a release

`GET /api/v1/history?mediaItemId=&limit=&offset=` returns the per-release timeline — grabbed, the
client's outcome, the import result, and every stop in between — newest
first, filterable with `?mediaItemId=`. Monarr has recorded these events
since the beginning and, until now, showed them to no one, which is how
five consecutive failed grabs of one movie inside ten hours looked, from
the outside, like a quiet evening.

Event types: `grabbed`, `failed`, `imported`, `import_failed`,
`import_blocked` (ran out of disk part-way), `import_retried`,
`regrab_capped` (a replacement search that was deliberately not made),
`short_delivery`, `payload_removed`, `quality_mismatch`,
`implausible_file`, `file_removed`.

## Quality profiles

A profile is a **target**, an optional **floor**, and an **upgrades**
switch (ADR 0014). Three sentences are the whole model, and they are the
sentence the UI prints on every profile:

> Hunt the best release at or below the target's resolution. While what's
> on disk is below the target and upgrades are on, keep looking; once the
> target is met, stop. Never grab below the floor.

Five seeded profiles, editable under **Settings → Quality profiles**:

| ID | Name | Target | Floor |
|---|---|---|---|
| 1 | 1080p | WEB-DL 1080p | — |
| 2 | HD-1080p | WEB-DL 1080p | HDTV 1080p |
| 3 | 4K | WEB-DL 2160p | WEB-DL 2160p |
| 4 | Ebook | EPUB | — |
| 5 | Audiobook | M4B | — |

Ids are stable — items, copies and import lists reference profiles by id —
so an upgrade from 0.5.x rewrites these rows in place rather than
recreating them. Two names changed: **Any → 1080p** and **Ultra-HD → 4K**.
"Any" is retired because it never meant "anything": it stopped upgrading
at WEB-DL 1080p while still being willing to *grab* a 2160p remux for a
missing item. The 1080p profile now caps grabs at its target resolution
too, which is the behaviour its cutoff always claimed.

**What the target does:** it caps grabs by RESOLUTION and defines "done".
A better SOURCE at the target resolution is welcome — a 1080p remux
satisfies (and may be grabbed under) a WEB-DL 1080p target, because
refusing a better file at the resolution you asked for helps nobody. The
thing people actually fear is a surprise 4K download, and that is a
resolution problem.

**What the floor does:** it says what is not worth having at all. With no
floor, something beats nothing — a 480p copy of a missing film is
acceptable until something better turns up. With a floor, monarr waits.

Two exclusions apply regardless of target and floor, because they are not
questions of rank: a **screen capture** (CAM, telesync) is never grabbed
unless a profile explicitly targets one, and the three format families
(film/TV · ebook · audiobook) never satisfy each other — an M4B is not a
better EPUB, it answers a different question.

### Defaults for new items

Under the profile table, one default per media kind — **Movies**,
**Series**, **Books** — is what an item gets when you add it without
picking a profile. The table's **Default for** column shows the same
answer from the other direction, so you can see at a glance which profile
new titles land on.

Per kind rather than one global default, for two reasons: a book cannot
use a video profile at all (the format families never satisfy each other,
above), and wanting 4K films alongside 1080p television is the ordinary
case rather than an exotic one. The Books picker offers only ebook and
audiobook profiles; the film and TV pickers offer only video ones.

Unset, the defaults are the values that used to be hardcoded — **1080p**
for films and television, **Ebook** for books — so upgrading changes
nothing until you choose. Changing a default affects only what you add
afterwards; nothing already in the library moves. If the profile a default
points at is somehow gone, adds fall back to the built-in rather than
failing: a preference is never allowed to be the reason an add breaks.

The add screen names the default it is about to use ("Default — 1080p")
rather than saying only "(default)".

**Deleting a profile** is refused while any item, copy, or import list
references it; the row shows the count and the button is disabled. It is
refused for the same reason while any kind defaults to it — that
reference is real even though no row holds it — with the tooltip naming
the kinds. Point them elsewhere first.

## File permissions (`MONARR_FILE_MODE`, `MONARR_DIR_MODE`)

Monarr sets the mode on every file and folder it places, rather than
inheriting whatever the download client or the temp file left behind.

| Variable | Default | What it governs |
|---|---|---|
| `MONARR_FILE_MODE` | `0644` | Imported media files |
| `MONARR_DIR_MODE` | `0755` | Library folders monarr creates |

Both take an octal string; anything unparseable falls back to the default,
because a typo must never produce a file nobody can read.

**Why this is explicit rather than inherited:** the cross-filesystem copy path
creates its temp file mode `0600` — owner only. Before 0.6.2 every copied
import therefore landed as a file no media server could open and monarr's own
prober could not re-read, while reporting complete success. Hard-linked
imports had the same hazard from the other side: the file kept whatever mode
the download client's umask produced.

A hard link shares one inode with the source, so setting the mode is visible
to the seeding copy too. That is the intended trade: a seeding file that is
readable is strictly better than a library file that is not.

Monarr only ever chmods folders it *creates* — never one that already
existed, and never upward toward the root folder.

## Media probing (`MONARR_FFPROBE`)

Monarr reads resolution, codec, bit depth, HDR format, interlacing, audio
tracks and duration out of MKV and MP4 files itself, with no external
dependency — the stock image is distroless and has nothing on PATH
(ADR 0013). There is nothing to configure for the formats that matter.

For the long tail it does not deep-parse (AVI, TS, WMV), set
`MONARR_FFPROBE` to the absolute path of an `ffprobe` binary and monarr
will use it opportunistically. An `ffprobe` on `PATH` is picked up without
configuration. Absent both, those files keep the quality their filename
claims — exactly as every file did before 0.6.0 — and their provenance
says so.

```bash
# Only useful if you actually have these containers AND a custom image.
MONARR_FFPROBE=/usr/bin/ffprobe
```

Probing never reads a whole file: MKV is capped at 8 MiB of header reads
and MP4 at 32 MiB, once per file, re-run only when the file's size changes.
A sweep over a multi-TB NFS library is cheap by construction.

**Re-measuring.** A probe that *failed to read* a file retries on the next
scan — a failure is usually about something outside the file (a permission,
a mount that was not up) and those get fixed. A container monarr deliberately
does not parse (AVI, TS, WMV) is cached and not retried, because nothing about
the next scan makes it parseable. To force the issue, **Re-measure files** on
any item page ignores the cache entirely.

## Custom formats

Regex rules scored against release titles, case-insensitive RE2. Scores
sum; the total breaks ties between equal-quality releases in both
interactive search ordering and automation's best-pick. Negative scores
bury releases (e.g. `-BadGroup$` at −1000). Invalid patterns are rejected
at add time.

## Import lists

External sources synced every 12 h (`importlists.sync`); new entries are
added with the list's root folder, quality profile, and monitor flag.

- **TMDB Popular / Top Rated** — no extra config.
- **Trakt list** — a public list: Trakt username, list slug, and a Trakt
  API **client id** (free app registration; needed even for public data).
  This is per-list and separate from the global
  [Trakt client id](#trakt-client-id-optional) that Discover uses.

Already-in-library entries are skipped, never duplicated.

An import list is not the same tool as [Discover](usage.md#discover), and
the difference is who decides: a list **adds** everything it names, on a
timer, unattended. Discover shows you the same kind of data and adds
nothing until you click. Use a list for "always have every new A24 release";
use Discover for "what should I watch".

## Notifications

| Type | Settings | Fires on |
|---|---|---|
| Webhook | `url` | grab / import / failed (+health if enabled) — POSTs `{event,title,body,fields}` |
| Discord | `url` (channel webhook) | same, as an embed |
| Plex | `url`, `token` (X-Plex-Token) | imports only — triggers a library rescan |
| Jellyfin | `url`, `apiKey` | imports only — `Library/Refresh` |
| plurx | `url`, `apiKey` (a scoped `plx_` key) | imports only — a **targeted scan**, not a refresh |

One request per imported **directory**, not per file: a season pack is one
folder and a dozen files, and a dozen requests naming the same folder would
be a dozen chances for one to fail. Book imports are skipped entirely — plurx
has no book library kind — and the skip is recorded, because silence looks
identical to a bug when your audiobook never appears.

**plurx is different from the other two.** Plex and Jellyfin get a poke —
"something changed, sweep your library" — and then identify the new file by
searching for its filename, which is the step that puts the 2015 remake's
poster on the 1995 film. plurx is told the exact paths that landed and the
TMDB/IMDb ids Monarr already holds, so it indexes one folder and matches by
id. Nothing is left to guess at.

Its key is a **scoped** plurx key (`plx_…`, scope `scan:trigger`), never a
plurx admin token. That distinction is the reason scoped keys exist: a plurx
user token *is* that user, so an admin token stored here would also hand over
every secret in plurx's own settings — to Monarr, and to anything that can
read Monarr's database. Mint one in plurx (Settings, or the two curl lines in
plurx's `docs/CHEATSHEET.md`) and paste it here.

**Test** asks plurx to scan a path that cannot be under any library root and
counts the rejection as a pass: it proves the URL, the key and the scope
without starting a scan of anything real.

What the failures mean:

| Message | Cause |
|---|---|
| `does not have this path under any library root` (with plurx's roots listed) | The two containers disagree about mounts — Monarr says `/data/media/…`, plurx has `/media/…`. Fix the mapping on Monarr's side. |
| `rejected the key (401)` | Unknown, revoked, or a plurx *user* token — that route does not accept those. |
| `lacks the scan:trigger scope (403)` | Real key, wrong scopes. Mint a new one; scopes are fixed at creation. |
| `N of M paths not indexed` | Partial: some files of a season pack landed and some did not. The per-path reasons follow. |

**Deliveries are queued, not fired and forgotten.** A plurx notification is
written to a row first and delivered by a worker: first attempt immediately,
then retries at 5 s, 30 s and 2 m before it is marked failed. The row is the
point — the common way to miss a scan is a host reboot where both
applications come back and monarr's import finishes a few seconds before
plurx is listening, and an in-memory retry does not survive the restart that
causes it. Failures that retrying cannot fix (401, 403, 422, a 404 from a
plurx too old to have the route) stop on the first attempt instead of
spending the whole schedule postponing the message you need to read.

**Delivery log** on the notifier row shows every attempt: what plurx
answered (`scanned → plurx item 1201`, or `queued as sr-…` when plurx was
mid-scan), or why it failed and after how many tries. The terminal outcome is
also written onto the download's own handoff trace as a `notify_plurx` step —
which is where *Activity* sends you when something imported but never
appeared in plurx.

Notifiers can now be **edited in place** rather than removed and re-added.
That was worth fixing here: the delivery log hangs off the notifier id, and
changing a URL should not throw away the record of everything before it.

**Both directions.** The Connections card lists what Monarr reaches out to
*and* what reaches in — a `calls Monarr` row per other application, with what
it last called and when. plurx's whole side of the pipeline is inbound (it
pushes watch state and reads the calendar), so without those rows a plurx
that was configured perfectly and calling every few minutes appeared nowhere
at all. `calling` means heard from within the hour; `quiet` means it has
called since Monarr started but not lately. Browsers are excluded — a person
with a tab open is not a connection.

Health checks cover every connection separately — `client:<name>`,
`client:<name>:capacity`, `mediaserver:<name>` — because the actions differ.
"Not answering" means grabs are piling up; "up but the disk is full" means
grabs are being accepted and going nowhere; a revoked plurx key means imports
are succeeding and never appearing. One averaged line would tell you none of
that.

A client that *answers* but through which nothing has actually come is
reported too: warning after 5 minutes, error after 30. That is what a push
subscription dying quietly looks like, and reachability alone would call it
healthy forever.

For nzbd, `client:<name>:capacity` names the reason it is up and downloading
nothing — low disk, quota used up, blocked news servers, a paused queue — in
plain words.

*Not* reported: nzbd's `health_abort`. It reads like an incident and is not
one — nzbd sets it from `[post] health_action`, so it is true on any server
configured to park or delete unrepairable downloads, which is a good default
and permanently on. Reporting it produced a red badge that could never clear.

Chat notifiers and Plex/Jellyfin are deliberately never probed: testing a
Discord webhook posts a message, and the only "test" a Plex notifier has is a
full library rescan. Neither belongs on a one-minute timer. They are still
listed, saying they were not probed, so an absence is never mistaken for an
all-clear. plurx is probed because it has a public, side-effect-free
`GET /api/v1/server`.

## Watch state from plurx

`POST /api/v1/webhooks/plurx` receives "somebody finished this" from a paired
plurx (master plan §11.1). Nothing to configure on Monarr's side beyond the
API key plurx already needs — the switch is in **plurx's** settings, off by
default, because it is plurx's users' viewing history that travels.

What Monarr does with it:

- **Records it**, per plurx user, per episode. Deliberately thin: who, what,
  when. No position, no device, no rewatch history.
- **Searches what you are watching first.** The backlog search has a per-run
  cap, so on a large backlog the order decides what gets searched at all.
  Anything watched in the last 30 days goes to the front. Being three
  episodes into a series and waiting a week for an upgrade — while a film
  nobody has opened in two years is retried nightly — is the case this fixes.
- **Nothing else.** No unmonitoring, no cleanup, and no deletion path exists
  on either side.

Matching is by TMDB or IMDb id and never by title; a payload with no ids is
refused. A notification for something Monarr does not manage returns 200 with
`matched: false` rather than a 404 — plurx retries failures, and there is
nothing to retry about a film Monarr was never asked to look after.

## Native app appearance

**More → Appearance** stores Theme and Item size on that phone or tablet.
Theme defaults to **Auto**: it follows the device's light or dark setting and
uses dark if the operating system reports neither. **Light** and **Dark** hold
an explicit override until changed. Item size applies one **Small**, **Medium**,
or **Large** choice to both Library tiles and Discover results; Medium preserves
the layout from native app versions before v0.18.2.

## Security

- **API key** — generated on first start, shown here (**Reveal**). This is
  what Prowlarr/Jellyseerr/Bazarr use as `X-Api-Key` against `/sonarr` and
  `/radarr`, and what scripts can use against `/api/v1` (`X-Api-Key`
  header or `?apikey=`). The native iOS/Android app also uses this key and
  stores it in the device keychain/keystore. After revealing it, **Link a
  mobile app** can create a pairing QR containing the key and a phone-reachable
  server address. The app starts local Wi-Fi discovery automatically; use the
  QR when multicast discovery cannot cross a guest network, VLAN, or VPN.
  Show that code only to a trusted device. You can instead copy the key into
  the app's first-run connection screen. Use HTTPS or a trusted VPN outside
  your LAN, because plain HTTP exposes the header in transit.
- **Authentication** — off by default; the compat personalities are always
  key-gated regardless. Set a username + password, then **Save & enable
  auth**: `/api/v1` (and therefore the UI) now requires the API key or a
  session from the login page. Credentials are stored salted-and-hashed.
  Enabling is refused until credentials exist, so you can't lock yourself
  out; the API key always works as a bypass.

## System page

- **Health** — database, data dir writability, embedded UI, metadata key.
- **Tasks** — every scheduled job with last/next run and a **Run now**:

| Task | Interval | Does |
|---|---|---|
| `health.check` | 1 m | runs the health registry |
| `queue.refresh` | 30 s | polls clients, imports completed downloads |
| `rss.sync` | 15 m | RSS loop → auto-grab wanted matches |
| `backlog.search` | 12 h | active search for wanted items |
| `library.reconcile` | 12 h | disk scan |
| `importlists.sync` | 12 h | import list sync |
| `metadata.refresh` | 12 h | re-hydrates every item from its provider: new episodes for continuing series, poster/status/**rating** updates |
| `backup.run` | 24 h | SQLite `VACUUM INTO` snapshot |
| `db.wal-checkpoint` | 1 h | WAL checkpoint |

Intervals are jittered to avoid thundering herds.

- **Backups** — the daily snapshots (newest first, last 7 kept), written
  to `<data>/backups/`. Restore = stop Monarr, copy a snapshot over
  `<data>/monarr.db`, start.
- **Live events** — the internal event bus streamed over SSE; useful to
  watch scans/grabs/imports happen.

## /metrics

Set `MONARR_METRICS=true` to expose Prometheus text at `/metrics`: build
info, library counts by kind, active queue depth, wanted total, uptime.
Off by default.
