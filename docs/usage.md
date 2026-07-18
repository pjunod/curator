# Using Monarr

A walkthrough of the UI, in the order you'll meet it. Setup reference for
every knob lives in [settings.md](settings.md); deployment and storage
guidance in [deployment.md](deployment.md).

## First run

Open `http://<host>:7676`. Three things make the app functional, all under
**Settings**:

1. **TMDB API key** — free from
   [themoviedb.org](https://www.themoviedb.org/settings/api) (a v3 key or a
   v4 read-access token both work). Powers movie/series search and metadata.
   Books need no key — Open Library is keyless.
2. **Root folders** — where your library lives, e.g. `/pool/media/movies`.
   Add one per collection type. A healthy root shows green with free space.
3. **An indexer and a download client** — see
   [settings.md](settings.md#indexers). Use the **Test** buttons before
   adding.

The **System** page shows health checks; anything missing from the list
above appears there as a warning.

## Adding media

**+ Add media** (Library page) → pick the Movie / Series / Book tab →
type a title. Movies and series search TMDB; books search Open Library and
show the author next to each result. Pick a root folder and monitoring
state, hit **Add**.

- *Movies* arrive hydrated with year, overview, poster, runtime.
- *Series* arrive with every season and episode; specials (season 0) start
  unmonitored, like upstream.
- *Books* arrive with author, first-publish year, and ISBN, and default to
  the **Ebook** quality profile (switch an item to **Audiobook** via the
  mass editor if you want M4B/MP3 instead).

Nothing touches the disk at add time — the item's folder is created on
first import.

## Adopting an existing library

Monarr expects humans to touch the filesystem. Point a root folder at your
existing collection and run **Scan disk** (Library page):

- Files inside known item folders are linked (episodes matched by
  `S01E02`-style names, quality read from the file name; book formats read
  from the extension).
- Folders nobody claims are listed as **unmatched**, each with a
  **Match…** button that pre-fills the add search.
- Items whose folders vanished are reported; their file records are pruned
  so the items become *wanted* again.

Scans also run on a 12-hour schedule.

## Getting releases

Two ways: do it yourself, or let the automation do it.

**Interactive search** — on any movie or book detail page, **Search
releases**; on a series, each season has **Search pack** and each episode a
**Search** button. Every release the indexers returned is shown — including
rejected ones, with the reason attached (`quality not allowed`,
`not an upgrade`, `does not match …`). Custom-format scores appear under
the quality column. **Grab** sends a release to the right download client
by protocol (torrent vs usenet).

**Automation** — anything monitored and missing (or below its profile
cutoff) is on the **wanted** list. Two loops work it:

- **RSS sync** (every ~15 min): pulls each indexer's newest releases and
  grabs whatever matches a wanted item and passes the decision engine.
- **Backlog search** (every 12 h, or run `backlog.search` from the System
  page): actively searches for wanted items and grabs the single best
  accepted release each.

Failed downloads (client-reported failures or import errors) are
**blocklisted** — never grabbed again — and a replacement search fires
immediately.

## Activity, calendar, wanted

- **Activity** shows the download queue with live progress; completed
  items are imported automatically within ~30 seconds (the `queue.refresh`
  task). Imported files are renamed to the library layout —
  `Movie (Year) [Quality].ext`, `Show - S01E02 - Title [Quality].ext`,
  `Author/Title/Title - Author.ext` — using hardlinks when the download and
  library share a filesystem.
- **Calendar** is an agenda of upcoming/recent episode air dates and
  movie/book release dates, with on-disk checkmarks.
- **Wanted** is exposed at `GET /api/v1/wanted` (UI page coming later; the
  System page's task list shows when the loops last ran).

## Upgrades

Every profile has a cutoff. Files below it stay wanted; when a better
release appears (RSS or search), it's grabbed, imported, and the old file
is replaced on disk. A release that isn't strictly better is rejected as
`not an upgrade`.

## The mass editor

Library page → **Edit** → click items to select → bulk **Monitor** /
**Unmonitor** / apply a quality profile. This is also the way to flip a
book between the Ebook and Audiobook profiles.

## Connecting the ecosystem

Monarr impersonates Sonarr and Radarr for apps that speak their v3 API
(see [adr/0003](adr/0003-compat-personalities.md)):

| App | Point it at | Notes |
|---|---|---|
| Jellyseerr/Overseerr | `http://<host>:7676/sonarr` and `…/radarr` | requests create real Monarr items |
| Prowlarr | same URLs, as "Sonarr"/"Radarr" apps | its indexer sync lands in Monarr's indexer list |
| Bazarr | same URLs | series/episodes/files enumerate normally |

All three ask for an API key: **Settings → Security → Reveal**. Anything a
consumer calls that the shim doesn't implement is logged
(`compat: unknown v3 request …`) — send that log line in an issue.

## Notifications

Settings → Notifications. Webhooks and Discord get grab/import/failure
events; Plex and Jellyfin entries are library-refresh pokes fired after
imports. **Test** delivers a test event immediately.

## Troubleshooting quick hits

- **"payload missing" on import** — Monarr can't open the path the
  download client reported. Fix the container mounts so both see the same
  path (see the README's Docker section).
- **Search finds nothing** — check the indexer Test passes, and remember
  book searches need book categories (7000s/3030) supported by the indexer.
- **Everything rejected** — read the rejection reasons; usually the
  profile doesn't allow the found qualities, or the release title doesn't
  match (year off by >1, different title).
- **Wanted list looks stale** — it refreshes on library events; running a
  scan or any grab/import refreshes it, as does restarting.
