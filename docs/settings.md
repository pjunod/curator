# Settings reference

Everything on the Settings and System pages, field by field. Process-level
configuration (port, data dir, log level) is environment-based and lives in
the [README](../README.md#configuration).

## Display preferences

**Display** at the bottom of the desktop navigation (or in the phone's
**More** sheet) keeps three browser-local choices:

- **Layout** changes structure without changing routes or data. **Classic** is
  the exact navigation layout Monarr shipped before this selector and remains
  the default. **Plex** strengthens the pinned media rail and opens a wider
  working canvas. **Theater** moves navigation into a broad top deck. All three
  adapt to phones and tablets.
- **Color scheme** selects Classic, Terminal, noirr, Amber, Giallo, Silver,
  Void, VHS, Paper, or Tide. Status colors retain their meaning in every
  scheme. Void and VHS are intentionally midnight-only.
- **Appearance** independently selects Auto (follow the device), Light, or
  Dark. Changing appearance never changes layout or color scheme.

These values live in the browser, not the server account, so a desktop and a
phone can use different combinations. Unknown preferences written by a newer
build fall back safely to Classic.

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

### Disk scan — repairing folders with recorded media

**Folders needing attention** lists entries with previously recorded media
whose assigned folder is missing, inaccessible, or no longer a directory.
A title awaiting its first download has no folder to repair and stays out of
this list. Existing scan reports are filtered on read, so upgrading removes
those false alarms without another scan.

Choose **Repair folders…**, browse to each title's existing folder, then
**Apply all repairs**. The action applies every entered path, including
other pages, and scans the selected folders to reconnect file records.
Individual failures remain visible and do not stop the other repairs.
Folders must contain media and sit under a compatible registered root;
another title's folder, an entire root, and empty folders are rejected.
Files are never created, moved, or deleted by repair. Files with matching
relative paths and sizes keep their identities, copy assignments, measured
quality, release provenance, and episode links.

If a drive or mount was unavailable, restore access and press **Check
again**. If the files were deleted, restore a backup or open the title to
search for a replacement. **Remove entry** is only for titles you no longer
want tracked; it leaves files alone.

## Indexers

Torznab (torrent) and Newznab (usenet) share one implementation.

Monarr reads and caches each indexer's capabilities, then uses only the TVDB,
IMDb, or TMDB query forms it advertises. Capability or protocol errors are
reported separately from an ordinary empty result. Unknown capabilities fall
back to one canonical-title query instead of fanning out guesses.

## Release identity readiness

There is no feature switch and incomplete metadata does not disable manual
work. Each movie or series detail page shows the canonical identity, external
IDs, aliases, country evidence, provider snapshot time, and last refresh error.
That panel is the advisory readiness check:

- external IDs and a recent successful snapshot support the safest matching;
- aliases expand bounded fallback searches and explain regional names;
- a missing or failed snapshot means automatic matching has less evidence;
- **Refresh identity** retries enrichment immediately;
- manual aliases can be added or removed without changing the canonical title.

Explicit contradictions remain automatic rejections even when readiness is
incomplete. A person can still choose a release with a manual grab, which is
recorded as such in Activity.

| Field | Meaning |
|---|---|
| Name | display name |
| URL | API root **without** `/api` — Monarr appends it (`https://api.drunkenslug.com`, or a Prowlarr feed like `http://prowlarr:9696/1`) |
| API key | the indexer's key |
| Protocol | `torrent` or `usenet` — decides which download client gets grabs |
| Categories | optional comma-separated Newznab category ids; empty = server defaults (ebook searches use 7000/7020; audiobook searches use 3030) |

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

Activity → **Manual import** starts at the completed folder Monarr sees. A
remote-path mapping's local prefix is authoritative; without one, Monarr uses
the parent of a recent completed payload, then `/pool/downloads` as the
standard-deployment fallback. This is a convenience default, not a second
client setting. The field enumerates matching directories as you type and
offers a **Parent folder** row; **Filesystem root** descends from `/`. You can
navigate the filesystem Monarr sees without copying names from a separate
directory listing.

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
  import** at the right folder, scan it, and select the exact files to use.
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

An **importing** row with a live worker shows its bytes, total, rate, elapsed
time, and current detail. If Monarr stopped after persisting `importing`, the
worker disappeared but the row survived; that row is labelled **interrupted**
instead of pretending to be busy forever. **Restart import** queues it again,
and **Cancel import** stops queued/live work while keeping the source payload
for a later restart. The normal client reconciliation also restarts an
interrupted row automatically after Monarr comes back up.

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

A profile is a **target**, an optional **floor**, an **upgrades** switch,
and a **download priority** (ADR 0014). The first three decide what Monarr
wants; priority decides which accepted grab nzbd works on first.

> Hunt the best release at or below the target's resolution. While what's
> on disk is below the target and upgrades are on, keep looking; once the
> target is met, stop. Never grab below the floor.

Five seeded profiles, editable under **Settings → Quality profiles**:

| ID | Name | Target | Floor | Download priority |
|---|---|---|---|---|
| 1 | 1080p | WEB-DL 1080p | — | Normal |
| 2 | HD-1080p | WEB-DL 1080p | HDTV 1080p | Normal |
| 3 | 4K | WEB-DL 2160p | WEB-DL 2160p | Normal |
| 4 | Ebook | EPUB | — | Normal |
| 5 | Audiobook | M4B | — | Normal |

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

For books, the edition medium is stored independently from its profile. A
profile chooses preferred formats **within** an ebook or audiobook edition;
it cannot turn one into the other. One Open Library work may keep both
editions, each with its own profile, monitoring, search, files, and upgrades.

Audiobook profiles can target **MP3, WMA, AAC, OGG, Opus, M4A, M4B, FLAC,
or WAV**. M4B remains the seeded target because it commonly carries one
book plus chapter markers; multipart releases in any supported audio format
are imported as numbered parts in lexical source order. Ebook profiles keep
the PDF, MOBI, AZW3, and EPUB vocabulary.

**What download priority does:** every grab sent to nzbd carries the
profile's level unless the item has its own override. Higher levels are
scheduled before lower ones; they do not change which release Monarr picks.
The six levels are **Very low** (`-100`) · **Low** (`-50`) · **Normal**
(`0`) · **High** (`50`) · **Very high** (`100`) · **Force** (`900`). Force
also runs through nzbd's queue pauses and quota holds, so use it for an
intentional exception rather than as the everyday high setting. Disk safety
still wins.

Set a profile to High, then make it the default for Movies or Series, to
prioritize that whole kind. For a new-release show or one film, open the item,
choose **Edit → Download priority**, and set an override. **Profile default**
clears that override; future changes to the profile then apply again. An item
override applies to its primary and additional quality copies.

### Defaults for new items

Under the profile table, separate defaults for **Movies**, **Series**,
**Ebooks**, and **Audiobooks** are what an item gets when you add it without
picking a profile. The table's **Default for** column shows the same
answer from the other direction, so you can see at a glance which profile
new titles land on.

Per family rather than one global default, for two reasons: a book cannot
use a video profile at all (the format families never satisfy each other,
above), and wanting 4K films alongside 1080p television is the ordinary
case rather than an exotic one. The Ebook and Audiobook pickers each offer
only profiles from their own format family; the film and TV pickers offer
only video ones.

When the other edition is added to a book already in the library, its own
family default is used unless you explicitly choose another matching profile.
Changing either default affects only editions added afterwards.

Unset, the defaults are the values that used to be hardcoded — **1080p**
for films and television, **Ebook** for ebooks, and **Audiobook** for
audiobooks — so upgrading changes
nothing until you choose. Changing a default affects only what you add
afterwards; nothing already in the library moves. If the profile a default
points at is somehow gone, adds fall back to the built-in rather than
failing: a preference is never allowed to be the reason an add breaks.

The add screen names the default it is about to use ("Default — 1080p")
rather than saying only "(default)". Download priority can be changed from
the item page after the title is added.

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
be a dozen chances for one to fail. Book imports use the same targeted path
and carry `hint:"book"` plus Curator's exact title, author, persisted
ebook/audiobook medium, stable work id, stable edition id, and an Open Library
cover URL when available. Cinema verifies the files, but it does not have to
repeat Curator's provider match. Editions are related by an exact work id;
title and author never become identity.

**plurx is different from the other two.** Plex and Jellyfin get a poke —
"something changed, sweep your library" — and then identify the new file by
searching for its filename, which is the step that puts the 2015 remake's
poster on the 1995 film. plurx is told the exact paths that landed and the
identity Curator already holds, so it indexes one folder and does not repeat
a fuzzy match. Movies and series use TMDB/IMDb fields. Books use the separate
work/edition contract above; nothing invents a movie id for them.

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

A client whose reachability test answers but whose queue-status poll has not
completed successfully is reported too: warning after 5 minutes, error after
30. The Detail column names the stalled operation, its age, and the last
error when one exists; without an upstream error it points you to the running
`queue.refresh` task. Reachability and queue polling are different facts, and
the page says which one failed.

A library copy no longer waits inside `queue.refresh`: importer-owned rows
are skipped until their worker finishes. A multi-gigabyte import therefore
cannot freeze the client's contact clock and falsely degrade nzbd while nzbd
is answering normally.

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

## Native app display preferences

**More → Display** stores Layout, Color scheme, Appearance, and Item size on
that phone or tablet. **Classic** preserves the native layout from before the
selector; **Plex** uses stronger pinned navigation; **Theater** puts primary
navigation in a top deck. The ten color schemes match the web UI. Appearance
defaults to **Auto**, follows the device's light or dark setting, and uses dark
if the operating system reports neither; **Void** and **VHS** always stay dark.
Item size applies one **Small**, **Medium**, or **Large** density to both
Library tiles and Discover results, with Medium preserving the previous
density.

## Access

Access has its own navigation tab because browser users, mobile pairing, and
integration credentials are all ways into the same server. General media and
automation configuration stays under Settings.

### User login

Authentication is off by default; the compatibility personalities are always
key-gated regardless. Set a username and password, then select **Save & enable
auth**. `/api/v1` and the web interface then require either the API key or a
session from the login page. Credentials are stored salted-and-hashed.
Enabling is refused until credentials exist, so you cannot lock yourself out;
the API key always works as a bypass.

### Mobile pairing

**Access → Mobile pairing** defaults to the browser's current server address;
replace that address when the phone uses a different LAN name, IP address,
reverse proxy, or VPN route. Select **Show pairing QR**, then use **Scan
pairing QR** in the native app. The code contains both the address and
Monarr's API key, so showing it is an explicit action and it should only be
scanned by a device you trust. The raw key stays hidden in the separate API
access section.

Automatic Wi-Fi discovery is normally the shorter path. The QR is the
deterministic fallback when multicast cannot cross a guest network, VLAN, or
VPN. A Docker install needs the v0.18.5-or-newer Compose template's
host-network `monarr-discovery` companion because the main container's bridge
cannot reach the physical LAN; the companion advertises only the host and
port and never reads the pairing key.

### API access

The API key is generated on first start and shown only after **Reveal**. This
is what Prowlarr/Jellyseerr/Bazarr use as `X-Api-Key` against `/sonarr` and
`/radarr`, and what scripts can use against `/api/v1` (`X-Api-Key` header or
`?apikey=`). The native iOS/Android app also uses this key and stores it in the
device keychain/keystore. You can copy the revealed key into the app's manual
connection form instead of using Mobile pairing. Use HTTPS or a trusted VPN
outside your LAN, because plain HTTP exposes the header in transit.

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

## Runner payload cleanup

For native Runner clients, cleanup waits for Runner to confirm deletion.
A pending operation, retention/recovery hold, authentication failure or
unavailable Runner keeps the payload pending for the next hourly sweep.
Curator never bypasses Runner by deleting its mounted directory directly.
Queue removal falls back to History only when the queue returns HTTP 404.
