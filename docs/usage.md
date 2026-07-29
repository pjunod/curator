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
(or leave the default — the picker names it, e.g. "Default — 1080p", and
you set it under Settings → Quality profiles), monitoring state, and
whether to **Search on add** (on by default: the best accepted release is
grabbed automatically right after adding, Sonarr-style). Hit **Add**.

**Adding several things is one screen.** Adding does not navigate away:
the row turns into *added* with an **Open** link, a strip at the top keeps
a running list of everything added this visit, and your search text, tab,
root folder and profile all stay put. The next title is one click, and
**Go to library** takes you out when you're done.

- *Movies* arrive hydrated with year, overview, poster, runtime.
- *Series* arrive with every season and episode; specials (season 0) start
  unmonitored, like upstream.
- *Books* arrive with author, first-publish year, and ISBN, and default to
  the **Ebook** quality profile (change the Books default under Settings →
  Quality profiles, or switch an item to **Audiobook** via the mass editor
  if you want M4B/MP3 instead).

Nothing touches the disk at add time — the item's folder is created on
first import.

## Discover

**Discover** (sidebar) is for the other half of the problem: adding
something when you do not already know its name. It is rows of posters —
*Trending this week*, *In theaters now*, *Coming soon*, *Popular*, *Top
rated*, *On the air* — read live from your metadata providers. Click a
poster for the overview and an **Add to library** button carrying the same
root folder / profile / monitor controls the Add page has.

**How to read a row.** Every row carries one line saying what it actually
measures, because the words do not mean the same thing everywhere: TMDB's
*trending* counts how many people looked a title up this week, Trakt's
counts how many have it **playing right now**. A title already in your
library is marked *in library* on the card and offers a link to it instead
of an Add button.

**Nine rows come free**, off the TMDB key the library already needs. A free
[Trakt client id](settings.md#trakt-client-id-optional) adds five more that
TMDB cannot answer — being watched right now (movies and shows), most
anticipated (movies and shows), and last weekend's box office. The **All /
Movies / Shows** tabs filter which rows are on screen. Books have no row:
Open Library has no popularity data worth showing.

**Poster size** is the **S / M / L** control in the page head, on both
Discover and the Library page. It is one preference — set it in either place
and the other follows — remembered in the browser, so it is per-device rather
than per-account. **M** is what both pages were before the control existed, so
leaving it alone changes nothing.

**What it costs:** nothing while you are not looking at it. Rows fetch only
as you scroll to them, are cached for 30 minutes, and no background job
runs. If a provider is briefly unreachable a row keeps showing its last
contents for up to a day rather than going blank; past that the error is
shown, because by then the row would be wrong rather than merely stale.

Discover never adds anything by itself. For that, see
[Import lists](settings.md#import-lists) — same kind of data, opposite job.

## On your phone (install as an app)

The UI is a PWA: open `http://<host>:7676` on your phone and install it —
**iOS Safari**: Share → *Add to Home Screen*; **Android Chrome**: menu →
*Add to Home screen* (or the install prompt). It launches full-screen
with its own icon, and below tablet width the whole app switches to a
phone layout: bottom tabs for Library / Wanted / Calendar / Activity,
with a **More** sheet holding Dashboard, System, Settings, Add, the
global search, and the theme picker. The calendar becomes an agenda,
wide tables scroll sideways, posters run three across. Posters are
cached for snappy loads; live data always comes from the server, so
offline you get the shell and an error rather than stale numbers. New
Monarr builds update the installed app on the next launch.

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

**S / M / L** in the page head sets poster size — smaller to fit more of a
large library on one screen, larger for the artwork. It is the same
preference [Discover](#discover) uses, so the two pages never disagree about
how big a poster is, and **M** is the size everything was before the control
existed.

## Adopting an existing library

Monarr expects humans to touch the filesystem. Point a root folder at your
existing collection and run **Scan disk** (Library page):

- Files inside known item folders are linked (episodes matched by
  `S01E02`-style names; book formats read from the extension).
- Every video file is **measured**: monarr reads the container headers and
  records resolution, codec, bit depth, HDR format, audio tracks and
  bitrate. A file called `A Good Day to Die Hard.mkv` gets a real quality,
  because the bytes were always there to answer the question (ADR 0013).
  Measuring is queued per file, so a large library fills in over the
  minutes after the scan rather than blocking it.
- Folders nobody claims are listed as **unmatched**, each with a
  **Match…** button that pre-fills the add search.
- Items whose folders vanished are reported; their file records are pruned
  so the items become *wanted* again.

Scans also run on a 12-hour schedule.

## Monitoring seasons and episodes

On a series page, each season row carries a checkbox. Unticking it stops
monarr wanting that season and cascades to every episode in it; each episode
has its own checkbox for finer control. The season name toggles the episode
table open and closed, and neither control interferes with the other.

## Getting releases

Automatic is the default posture — the interactive search is the override
for when you want to pick a specific release yourself.

**Automatic** — three entry points, all using the decision engine (the
profile's target and floor, upgrade rules, custom-format scores) to pick
the best accepted release with no list to review:

- **Search on add** (checkbox on the add form, on by default) fires the
  moment an item is added.
- **Changing an item's quality profile** fires one too. Asking for a
  different target is a request, not a note — without this the item joined
  the wanted list and waited up to twelve hours for the backlog loop.
- **Auto search** (button on every detail page) does the same on demand —
  for series it searches per-season packs.
- The **Wanted** page lists everything still missing or below cutoff, shows
  when the loops last ran, and has **Search all now**.

**Interactive search** — **Interactive search** on movie/book detail pages;
on a series, each season has **Search pack** and each episode a **Search**
button. Every release the indexers returned is shown — including rejected
ones, with the reason attached (`above target`, `below floor`,
`not an upgrade`, `target met`, `does not match …`). **Grab** sends your
pick to the right client, with no decision gate — interactive search is
the override, and it always wins.

**The loops** — anything monitored and missing (or below its profile
target) stays on the wanted list, and two loops work it unattended:

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
- **Re-measure files** — read the files again and record what is actually in
  them. Use it after fixing a permission or a mount that made a probe fail;
  a routine scan skips files it has already measured at the same size.
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

## The Files table, and getting rid of a bad file

Every file lists what it measures as and **how we know** — measured from the
bytes, taken from the filename, taken from the release name, set by hand, or
unreadable.

There is one more badge, and it is the only one that changes what monarr does:
**does not add up**. It means the file was read and what it says about itself
cannot be true — a 2160p file at 725 kbps, a declared TrueHD track the whole
file has no room for, or a 90-second "movie". The reason prints next to the
path, with the arithmetic.

One of those reasons is worth reading closely, because it tells you where to
go looking. **"The file is cut short"** means the container states its own
finished length and the file on disk is smaller: 500 MB arrived out of a
declared 60 GB. That is not a bad release, it is a transfer that stopped, and
blocklisting it would punish a release that was fine. Check the download
client, the disk, and any size caps.

A cut-short file caught at import is **refused** rather than placed — the one
refusal in the import path, because it is the one case that is proof rather
than judgement. The download is surfaced as failed with the numbers on it and
can be retried once the underlying problem is fixed; the release is not
blocklisted. Placing the stump instead would put the item straight back to
wanted, so the next search would grab, land another stump, and go round again.
You will still see this badge on files that landed before the check existed —
"Re-measure files" re-grades them.

Every other reason means the opposite: the file is complete and is simply not
what it claims. Those are worth blocklisting. The distinction matters because
from every other angle the two are identical — a fake is usually built from a
real release's header, so it has the same tracks, the same chapters and the
same stated runtime. The declared length is the only thing that separates
them. A file in this state stays exactly where it is and
stops counting toward the item being complete, so monarr keeps looking for a
real copy instead of sitting satisfied on a fake one.

This is deliberately different from **unreadable**, which means monarr could
not parse the file at all. Not knowing is a reason to leave a file alone —
replacing something you cannot read means possibly deleting something perfect.
Knowing it is wrong is the opposite.

Two buttons on each row:

- **Delete** — removes the file from disk and from the library. Use it for a
  duplicate, or a copy you simply do not want.
- **Delete & blocklist** — removes the file, tells automation never to grab
  that release again, and starts searching for a replacement. Use it when a
  release turned out to be a fake, a mislabel, or the wrong cut.

The two are separate because they are separate statements: a file deleted to
free space should not poison a release that was fine. **Delete & blocklist**
is disabled for a file monarr did not grab — an adopted file has no source
release, so there is nothing to blocklist, and the button says so rather than
half-working.

## Cleaning up after a download

Once a payload has been imported, the copy in the download client's completed
folder is doing nothing but occupying a disk. **Clean up after import**, on
each download client in Settings → Acquisition, deletes it.

Deleting is safe whichever way the file was imported. When Monarr can hardlink
— the library and the download folder on one filesystem — the client's copy is
just a second name for the same bytes, and removing that name leaves the
library's. When it cannot, and it copied, the client's copy is a genuine
duplicate and removing it frees real space. That second case is the one that
gets expensive: a fast local disk for downloads and a big array for the library
is the normal arrangement, and it means every grab is stored twice.

The default differs by client type, because the right answer does:

- **Usenet** (nzbd, SABnzbd, NZBGet) defaults **on**. There is no obligation
  once the download finishes; the payload is scratch space.
- **Torrents** default **off**. The client is still seeding, and Monarr cannot
  yet tell "finished seeding" from "seeding happily", so throwing that data
  away stays your decision.

Turning it on also collects what has already piled up. An hourly
`downloads.cleanup` task sweeps imported downloads whose payload is still on
the client, oldest first, so enabling the setting after a year of grabbing
clears the year of grabs too — a few dozen at a time, so the client is never
handed hundreds of deletions at once. Run it on demand from System → Tasks.

## Interactive search

Every candidate is listed, including the ones monarr would decline, each with
its reasons — "found nothing" and "found two hundred and disliked all of them"
are different problems. A rejected release can still be grabbed by hand;
manual grabs are never gated.

Because a busy indexer returns hundreds of rows, the list pages (50 at a time
by default), filters by name, and offers a **Would be grabbed** view. Each
row's rejections collapse to the first reason with the rest one click away.

A release can also carry an amber **⚠ warning**: the profile would take it,
but its advertised size cannot hold what its name claims — a "2160p Remux"
listed at 500 MB. That is a caution for you, not a refusal. Unattended
searches (RSS and backlog) do decline these outright, because nobody is there
to weigh it and the cost of guessing wrong is a fake file that ends the hunt.

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

## When an import doesn't import

The manual import panel (Activity → Manual import) now reports what happened
to **every** file, including the ones it declined and why — "no files
imported from `<path>`" on its own is true and useless.

Common reasons, and what they mean:

| Reason | What to do |
|---|---|
| `… does not improve on the … already here (profile "X")` | Only automation is gated by the profile. A manual import is the override and goes ahead anyway. |
| `no known episodes for S20 [18 19 20]` | monarr has no episode records for those numbers — refresh the series metadata first. |
| `cannot determine episodes from "<name>"` | The filename carries no `S00E00` pattern monarr recognises. |
| `item has no library folder assigned` | Set a root folder / path on the item. |

A **manual** import is never blocked by the quality profile: you pointed at
the folder and pressed Import, and that is the decision. It still only
*replaces* an existing file when the new one actually outranks it — otherwise
both stay and you choose.

## Reading the quality row

The item page's **Quality** row says three things: what is on disk, the
facts monarr measured, and whether it is still hunting.

| What you see | What it means |
|---|---|
| `on disk  WEB-DL 1080p` | The weakest quality among this item's files — the one that decides whether it is still being hunted. |
| `1080p · HEVC · HDR10 · TrueHD · 23 Mbps` | Measured from the file itself, not from its name. |
| `upgrading to WEB-DL 1080p` | Below the profile's target, upgrades on, still looking. |
| `at or above WEB-DL 1080p` | Target met. Nothing better will be sought. |
| `at target (source unverified)` | The resolution matches the target but monarr could only *guess* at the source. It stops rather than replace a possibly-perfect file on a guess. Interactive search still grabs whatever you pick. |
| `below … · upgrades off` | Below target and staying there: this profile has upgrades switched off. |
| `on disk — monarr could not read this file` | The file exists and could not be parsed. Unknown is **not** missing: monarr will not hunt a replacement for a file it simply failed to read. |

The **Files** table names the same thing per file, with a **How we know**
badge:

- **measured** — read out of the file's own headers. Resolution measured
  this way is fact; nothing overrides it.
- **from filename** / **from release** — the name claimed it and nothing
  measured contradicted the claim. Names are hints now, not authority.
- **set by hand** — a human said so.
- **unreadable** — on disk, quality unknown.

Resolution is always measured when a probe succeeded. The **source**
(WEB-DL vs Bluray vs remux) is inferred from the measurements — lossless
audio, bitrate bands, interlacing, muxer signature — and cross-checked
against the name. A low-confidence inference is shown but never triggers
a replacement, because being wrong about a source should cost an odd badge
and never somebody's 17 GB file.

When a grabbed release turns out not to be what it claimed, the import
still proceeds — the file is what it is — and a `quality_mismatch` event
is recorded against the release with the measured truth beside the claim.

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
  library share a filesystem. **Expand any row** (the ▸) to see the
  handoff, below.
- **Calendar** is a month grid: episode air dates and movie/book release
  dates land on their days, filled when the file is on disk, outlined when
  it isn't. Page with ← / Today / →.
- **Wanted** lists everything monitored that's missing or below cutoff,
  shows when the RSS and backlog loops last ran / run next, and offers
  **Search all now** plus a per-item **Search** button that grabs the best
  accepted release for just that entry.

## The handoff: download to library

The stretch between "the download client finished" and "the files are in
the library" is a black box in most tools — when something doesn't show up,
you can't tell why. Monarr lays it out. Expand a row on the **Activity**
page (the ▸) to see the handoff step by step, each with a timestamp:
**grabbed** → **downloading** → **downloaded** → **importing** →
**imported**. Alongside the trace it shows the two paths that matter: what
the download client *reported*, and where Monarr *looked* after any remote
path mapping. When an import stops, the trace ends at **failed** with the
exact reason — no guessing.

Every row carries the controls for its state:

- **Import now** — on a download **awaiting approval** (from a client set to
  require approval), imports it right now.
- **Retry** — on a **failed** row, re-runs the import using the same path,
  for after you've fixed a mount or added a path mapping.
- **Manual import** — browse Monarr to the real files (a folder or a single
  file), preview what it found, pick the target title and copy, and import.
  Use it when automation couldn't resolve the payload, or to pull in files
  you placed by hand. It's also on the top of the Activity page for imports
  not tied to any download.
- **Blocklist** — declare the release bad: blocklist it so it's never
  grabbed again and search for a replacement. This is the *only* way a
  handoff gets blocklisted — an import failure never does it on its own, so
  a mount problem never churns silently through replacements.
- **Remove** — drop the row (leaves the client and any files alone).

**Require approval per client** (Settings → Download clients) flips a
client from hands-free to hold-for-approval: its completed downloads park at
*awaiting approval* until you press **Import now**. Off by default.

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

A **plurx** entry is not a poke: it receives the exact paths that landed and
the TMDB/IMDb ids Monarr already knows, so plurx indexes one folder instead
of sweeping a library — and identifies it by id rather than by guessing at
the filename. It holds a scoped `plx_` key, never a plurx admin token. See
`docs/settings.md` for the setup and what each failure means.

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
