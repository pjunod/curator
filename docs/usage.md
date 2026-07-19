# Using Monarr

A walkthrough of the UI, in the order you'll meet it. Setup reference for
every knob lives in [settings.md](settings.md); deployment and storage
guidance in [deployment.md](deployment.md).

## Install & run

With compose (recommended — your customized compose file is gitignored,
pulls never touch it):

```sh
git clone https://github.com/monarr-media/monarr.git && cd monarr/deploy
cp docker-compose.example.yml docker-compose.yml   # yours to edit
cp .env.example .env                               # optional: paths + user
docker compose up -d --build
```

Or plain Docker, from the repo root (the build context is the repo; the
Dockerfile lives in `deploy/`):

```sh
docker build -f deploy/Dockerfile -t monarr .
docker run -d --name monarr -p 7676:7676 --user 1000:1000 \
  -v /srv/monarr:/data -v /srv/pool:/pool monarr
```

`/data` holds the database and backups; `/pool` holds your media library
*and* your download client's completed folder — the mount rules and a
worked pool layout are in the README's Docker section. After code updates:
`docker compose up -d --build` again (it rebuilds and swaps the container
only when the image changed).

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
show the author next to each result. Pick a root folder, a quality profile
(or leave the kind default), monitoring state, and whether to **Search on
add** (on by default: the best accepted release is grabbed automatically
right after adding, Sonarr-style). Hit **Add**.

- *Movies* arrive hydrated with year, overview, poster, runtime.
- *Series* arrive with every season and episode; specials (season 0) start
  unmonitored, like upstream.
- *Books* arrive with author, first-publish year, and ISBN, and default to
  the **Ebook** quality profile (switch an item to **Audiobook** via the
  mass editor if you want M4B/MP3 instead).

Nothing touches the disk at add time — the item's folder is created on
first import.

## The library page

The **All** tab shows everything, grouped into Movies / Series / Books
sections (never interleaved); the kind tabs show flat grids. **Each
section carries its own controls** in its header bar: a title/author
text filter, a state filter (monitored, unmonitored, missing,
incomplete, complete), and a sort (title, year, recently added, rating —
books also sort by author) with an ascending/descending toggle. The same
per-kind settings drive that kind's flat tab, and everything but the
text filter is remembered per browser. Every card carries the rating star,
the color-coded completeness pill, and a ↓ badge while something is
downloading for it.

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

Automatic is the default posture — the interactive search is the override
for when you want to pick a specific release yourself.

**Automatic** — three entry points, all using the decision engine (profile
allowed-set, cutoff, upgrade rules, custom-format scores) to pick the best
accepted release with no list to review:

- **Search on add** (checkbox on the add form, on by default) fires the
  moment an item is added.
- **Auto search** (button on every detail page) does the same on demand —
  for series it searches per-season packs.
- The **Wanted** page lists everything still missing or below cutoff, shows
  when the loops last ran, and has **Search all now**.

**Interactive search** — **Interactive search** on movie/book detail pages;
on a series, each season has **Search pack** and each episode a **Search**
button. Every release the indexers returned is shown — including rejected
ones, with the reason attached (`quality not allowed`, `not an upgrade`,
`does not match …`). **Grab** sends your pick to the right client.

**The loops** — anything monitored and missing (or below its profile
cutoff) stays on the wanted list, and two loops work it unattended:

- **RSS sync** (every ~15 min): pulls each indexer's newest releases and
  grabs whatever matches a wanted item and passes the decision engine.
- **Backlog search** (every 12 h, or run `backlog.search` from the System
  page): actively searches for wanted items and grabs the single best
  accepted release each.

Failed downloads (client-reported failures or import errors) are
**blocklisted** — never grabbed again — and a replacement search fires
immediately.

## The item page

Click anything in the library to get its page. The fact grid answers the
usual questions at a glance:

- **Location** — the item's folder as a highlighted path chip (assigned
  at add time; created on first import).
- **Status** — monitored/unmonitored, a color-coded completeness pill
  (green = everything wanted is on disk, yellow = partial, red =
  nothing; series count monitored episodes aired to date), and a
  **↓ downloading** pill whenever a grab for this item is in flight.
- **Profile** — the quality profile driving grabs and upgrades.
- **Ratings** — labeled chips per source: TMDB (/10) for movies & series
  and Open Library (/5) for books come free; add an OMDb key (Settings →
  Metadata → Extra ratings) and Rotten Tomatoes, IMDb, and Metacritic
  appear alongside. Percent sources render as percentages.
- **Links** — IMDb / TMDB / TVDB / Open Library pages, opening in a new
  tab.

Actions up top:

- **Auto search** — grab the best accepted release automatically.
- **Interactive search** (movies/books; per-episode and per-season on
  series) — the full candidate list with scores and rejection reasons.
  Release titles link to the release's page on the indexer (new tab).
- **Edit** — change monitoring, quality profile, root folder, or the
  folder path itself. Changing the root recomputes the folder from the
  naming rules; **files on disk are never moved** by an edit.
- **Refresh metadata** — re-fetch from the provider right now: new
  episodes for a continuing series, updated poster/status/rating. The
  `metadata.refresh` task does this for the whole library every 12 h
  (run it once from System → Tasks after upgrading to backfill ratings).

The same completeness pill, rating, and ↓ badge appear on every library
card, so the grid shows at a glance what's complete, what's partial, and
what's moving right now.

## Monitoring: series, seasons, episodes

Monitoring decides what the automation hunts, at three levels that all
AND together: the series toggle (Edit / mass editor), a checkbox on
every **season** header (cascades to its episodes), and a checkbox on
every **episode** row (mixed states inside a season are fine — keep just
the finale). Unmonitored rows dim. At add time, the Series tab offers
**Seasons: all / latest only / none** — add a 20-season show with
"latest only" and untick or tick the rest afterwards. Metadata refresh
respects all of it: a newly announced episode in an unmonitored season
arrives unmonitored.

## Quality copies (the same thing at two qualities)

The item page's **Quality copies** panel keeps a movie or series at more
than one quality at once — the main copy at 4K plus a 720p copy for
someone else. Each copy has its own quality profile and is a first-class
automation target: it shows in Wanted under its label, both loops hunt
it, it imports and upgrades independently, and upgrading one copy never
touches another copy's files. A copy either lives in its **own folder**
under a root you pick (a separate library for another person or device)
or **shares the item's folder** — filenames carry the quality, and
Jellyfin/Plex group same-folder versions as one entry. Removing a copy
drops its records only; files on disk stay.

## Activity, calendar, wanted

- **Activity** shows the download queue with live progress; completed
  items are imported automatically within ~30 seconds (the `queue.refresh`
  task). Imported files are renamed to the library layout —
  `Movie (Year) [Quality].ext`, `Show - S01E02 - Title [Quality].ext`,
  `Author/Title/Title - Author.ext` — using hardlinks when the download and
  library share a filesystem.
- **Calendar** is a month grid: episode air dates and movie/book release
  dates land on their days, filled when the file is on disk, outlined when
  it isn't. Page with ← / Today / →.
- **Wanted** lists everything monitored that's missing or below cutoff,
  shows when the RSS and backlog loops last ran / run next, and offers
  **Search all now** plus a per-item **Search** button that grabs the best
  accepted release for just that entry.

## Finding things fast

The search box at the bottom of the sidebar finds **library items by
title or author** and **pages & settings sections by name** ("indexers",
"notifications", "root folders", …) — Enter jumps to the first hit, and
any query can fall through to "Add …" to search the metadata providers.
Next to it, the **Auto / Light / Dark** picker overrides the theme (Auto
follows the OS preference; the choice is remembered per browser).

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
  path, or set a **remote path mapping** on the client (Settings →
  Download clients — see `docs/settings.md`).
- **Search finds nothing** — check the indexer Test passes, and remember
  book searches need book categories (7000s/3030) supported by the indexer.
- **Everything rejected** — read the rejection reasons; usually the
  profile doesn't allow the found qualities, or the release title doesn't
  match (year off by >1, different title).
- **Wanted list looks stale** — it refreshes on library events; running a
  scan or any grab/import refreshes it, as does restarting.
