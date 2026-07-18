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

Blank credentials mean "send no auth at all" — valid whenever the client
itself doesn't require it.

`Category` (default `monarr`) tags/labels downloads where the client
supports it. Grabs route by protocol: a torrent release goes to the first
enabled torrent client, usenet to the first enabled usenet client. The
queue is polled every 30 s; completed items import automatically.

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

A failed import with `payload missing` means the reported path wasn't
visible — fix the mount or add a mapping.

## Quality profiles

Five seeded profiles (editing arrives with a later UI pass):

| ID | Name | Allowed (worst → best) | Cutoff |
|---|---|---|---|
| 1 | Any | 480p SD → 2160p Remux | WEB-DL 1080p |
| 2 | HD-1080p | 1080p HDTV → 1080p Remux | WEB-DL 1080p |
| 3 | Ultra-HD | 2160p WEB-DL → 2160p Remux | WEB-DL 2160p |
| 4 | Ebook | PDF → MOBI → AZW3 → EPUB | EPUB |
| 5 | Audiobook | MP3 → M4B | M4B |

Files at/above the cutoff stop being wanted; below it they're upgrade
candidates. Profiles are picked per item (movies/series default to Any,
books to Ebook) and changeable in bulk via the mass editor.

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

Already-in-library entries are skipped, never duplicated.

## Notifications

| Type | Settings | Fires on |
|---|---|---|
| Webhook | `url` | grab / import / failed (+health if enabled) — POSTs `{event,title,body,fields}` |
| Discord | `url` (channel webhook) | same, as an embed |
| Plex | `url`, `token` (X-Plex-Token) | imports only — triggers a library rescan |
| Jellyfin | `url`, `apiKey` | imports only — `Library/Refresh` |

## Security

- **API key** — generated on first start, shown here (**Reveal**). This is
  what Prowlarr/Jellyseerr/Bazarr use as `X-Api-Key` against `/sonarr` and
  `/radarr`, and what scripts can use against `/api/v1` (`X-Api-Key`
  header or `?apikey=`).
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
