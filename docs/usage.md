# Using Monarr

A walkthrough of the UI, in the order you'll meet it. Setup reference for
every knob lives in [settings.md](settings.md); deployment and storage
guidance in [deployment.md](deployment.md).

## Install & run

With compose (recommended — your customized compose file is gitignored,
pulls never touch it):

```sh
git clone https://github.com/pjunod/monarr.git && cd monarr/deploy
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
docker run -d --name monarr-discovery --restart unless-stopped \
  --network host monarr advertise --name monarr --port 7676
```

The standard image bundles the embedding executable and Cinema's Jellyfin
FFmpeg 8 package, including `ffprobe`, `ffmpeg` and their shared libraries. Build checks execute both media tools in the
actual distroless base. Native MKV/MP4 inspection remains in-process; other
formats can use the bundled probe without a custom image.

`/data` holds the database and backups; `/pool` holds your media library
*and* your download client's completed folder — the mount rules and a
worked pool layout are in the README's Docker section. After code updates:
`docker compose up -d --build` again (it rebuilds and swaps the container
only when the image changed). The tracked Compose template includes a
host-network `monarr-discovery` companion because multicast DNS cannot leave a
normal Docker bridge; the companion advertises port `7676` and does not read
the database or proxy traffic.

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

## Layouts and color schemes

Open **Display** at the bottom of the desktop navigation, or in **More** on a
phone. Layout, color scheme, and room brightness are separate choices: switch
one without losing the other two. **Classic** is the pre-existing Monarr
layout and the default, **Plex** is the stronger media rail, and **Theater**
uses a wide top navigation deck. The color catalogue matches plurx: Classic,
Terminal, noirr, Amber, Giallo, Silver, Void, VHS, Paper, and Tide, each with
light and dark appearances where the design supports both. Void and VHS stay
dark by design.

The choice is remembered per browser. Switching layouts restyles the current
page in place, so open forms, fetched data, and the current route remain where
they were.

## Adding media

### Describing a series or finding similar shows

On **Add media → Series**, choose **Describe what you want**, enter a
description such as `gay-themed series, no teen dramas`, and click **Find
series**. Applied theme, genre, original language, years, central-theme
requirement, teen exclusion, and library visibility remain visible and
editable. Supported themes are gay male, lesbian, LGBTQ+, coming of age,
found family, political drama, and space exploration. Multiple themes or
unsupported exclusions ask for refinement.

**More like this** on a title starts a fresh seed search. On a recommendation
it keeps the active description and filters. **Remove seed** keeps those
filters. **Title search** returns to ordinary title lookup. A description is
sent in a POST body and never added to the browser URL or persistent storage.

**Why this matches** identifies the provider field and fetch date supporting
each result. Required themes need explicit metadata evidence; local similarity
cannot establish a theme. **Central theme only** requires main-story synopsis
evidence. **Exclude known teen-focused stories** removes known teen/Kids
focus; unclassified shows may remain. Sparse metadata can yield few results.

The catalog search checks a bounded candidate set. Metadata ordering works
without a model. Enable optional local ranking in
[settings](settings.md#local-semantic-ranking-optional) for description and
seed similarity. Provider suggestions and unavailable similarity are labeled.
Preview and Add use the normal library workflow. Known owned titles can be
hidden; unknown ownership remains visible, and conflicting identities cannot
be added from these suggestions.

### Adding a named title

**+ Add media** (Library page) → pick Movie / Series / Ebook / Audiobook →
type a title. Movies and series search TMDB; books search Open Library and
show the author next to each result. Pick a root folder, a quality profile
(or leave the default — the picker names it, e.g. "Default — 1080p", and
you set it under Settings → Quality profiles), monitoring state, and
whether to **Search on add** (on by default: the best accepted release is
grabbed automatically right after adding, Sonarr-style). Hit **Add**.
The Add action stays disabled until a root that accepts that media kind is
available. The API also refuses search-on-add without a root, and every grab
checks the destination again before it contacts a download client. This is
why a missing setting cannot turn into a completed download that only then
fails with `item has no library folder assigned`.

**View details before adding.** Select a result's poster, title, synopsis,
or **View details**. The web UI opens a side drawer on desktop and a full
screen on phones. Read the complete synopsis, available genres, runtime
(per episode for series), and provider status. IMDb, TMDB, TVDB, and Open
Library links appear only when a known database ID is available and open
in another tab. Escape, the backdrop, Close, or browser Back returns to the
same search. Add options in the drawer share the page's root, profile,
monitoring, and search-on-add choices. Quick **Add** remains separate.

Native mobile labels the preview action **Read full synopsis** in Discover
and search. Opening it does not add anything. **Continue
to add** opens the existing options in the same modal; Back returns to the
preview with those choices intact. Quick **Add** skips the preview step,
but still shows the complete synopsis above the options before the final Add.
After a successful native add, the app opens the library item.

A provider outage leaves the original search information available with
Retry. Previewing never adds an item or changes the original Add identity.
Known library items offer **Open in library**; book ownership is specific
to the selected edition. Conflicting identities disable Add and Open until
resolved, while ambiguous local ownership retains readable metadata and
verified links. Previews are always available; there is no feature switch.

**Adding several things is one screen.** Adding does not navigate away:
the row turns into *added* with an **Open** link, a strip at the top keeps
a running list of everything added this visit, and your search text, tab,
root folder and profile all stay put. The next title is one click, and
**Go to library** takes you out when you're done.

- *Movies* arrive hydrated with year, overview, poster, runtime.
- *Series* arrive with every season and episode; specials (season 0) start
  unmonitored, like upstream.
- *Ebooks and audiobooks* share Open Library metadata (author,
  first-publish year, ISBN) but have independent defaults and same-family
  profile pickers. Audiobook searches use audiobook indexer categories and
  support MP3, WMA, AAC, OGG, Opus, M4A, M4B, FLAC, and WAV.

Choosing Ebook or Audiobook adds that edition, not a quality label. If the
same Open Library work already has the other edition, **Add ebook** or **Add
audiobook** attaches the missing edition to the existing book. The result row
names the editions already in the library instead of disabling both choices.
You can also add the missing edition from the book detail page.

Nothing touches the disk at add time — the item's folder is created on
first import.

**Every item gets a folder of its own.** The folder is `Title (Year)`
(`Author/Title` for books) — unless another library item already holds that
name, which happens whenever two different works share a title and a year
(there are two films called *Leviticus* from 2022). The second one is then
given `Leviticus (2022) {tmdb-…}`: the same name plus its provider id, in the
form Plex and Radarr use. Both stay in the library, each with its own files.
Choosing a folder by hand that another item already holds is refused with the
holder's name, rather than silently sharing it (ADR 0020). Adoption reads the
`{tmdb-…}` / `[tmdbid-…]` hint back, so a disambiguated folder is matched to
exactly the film it names.

Upgrading to 0.28.1 repairs libraries where this already happened: for each
folder claimed twice, the item that owns the files keeps it and the other is
pointed at its own `{tmdb-…}` folder. Only the library record changes;
nothing on disk is moved, and the `library-folders` warning under
**System → Health** clears.

## Discover

**Discover** (sidebar) is for the other half of the problem: adding
something when you do not already know its name. It is rows of posters —
*Trending this week*, *In theaters now*, *Coming soon*, *Popular*, *Top
rated*, *On the air* — read live from your metadata providers. Click a
poster for the overview and an **Add to library** button carrying the same
root folder / profile / monitor controls the Add page has.

The native add sheet preselects the first root folder that accepts the title's
media kind. You can choose another compatible root, but you cannot add the
title until one is selected; there is no implied server-default root because
an omitted destination would leave the item with nowhere to import downloads.

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

**Poster size** is the labelled **S / M / L** control in the page head, on
both Discover and the Library page. It is one preference — set it in either place
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

## Native iOS and Android app

Monarr's native companion connects to the server you already run; it does not
run the automation engine on the phone. On first launch, it immediately
searches the current Wi-Fi network and puts each verified server at the top of
the connection screen. QR and manual setup stay visible while that search runs:

1. **Automatic Wi-Fi discovery** browses the local DNS-SD service
   (`_monarr._tcp`). It uses the addresses and port announced by the server,
   including IPv4 and IPv6; it does not guess a subnet or scan an address
   range. Results appear as soon as they answer; **Search again** repeats the
   five-second browse when network conditions change.
2. **Scan pairing QR** reads the code shown by **Access → Mobile pairing →
   Show pairing QR**. You do not have to reveal the raw API key first. Enter
   an address the phone can reach before showing the code. The QR transfers
   that address and the API key.
3. **Enter it manually** accepts a LAN IP/hostname, a VPN address such as
   Tailscale, or an HTTPS reverse proxy. `localhost` means the phone itself. A
   bare direct host automatically uses Monarr's default port, `7676`; explicit
   ports and HTTPS reverse-proxy URLs are preserved. Put literal IPv6
   addresses in brackets when a port is present, for example
   `http://[fd12:3456::20]:7676`.

DNS-SD stays inside the network's multicast domain. Guest Wi-Fi, isolated
VLANs, and some VPNs can prevent advertisements from reaching the phone even
when the server itself is reachable. Docker installs must run the
host-network `monarr-discovery` companion included in the v0.18.5 Compose
template; a copied `docker-compose.yml` is gitignored, so add that service to
older copies yourself. Use the QR or manual address when multicast domains are
deliberately separated.

The app covers the daily mobile loop: browse and filter the library; inspect a
title; edit its monitoring, quality profile, inherited or per-item download
priority, root folder, and path; change season monitoring; and start a search.
**Search now** is the auto search: the best accepted release is grabbed in
the background. **Interactive search** is the same override the web offers —
every release the indexers returned, rejected ones included with their
reasons, a **Would be grabbed** view that narrows the list without gating
anything, and **Grab** on any row. Movies and books search the item (each
book edition and each quality copy has its own **Search**). On a series
there is no whole-series scope: **Interactive search** opens on the first
real season's pack, **Search pack** on a season row opens on that pack, a
copy's **Search** opens on the first season for that copy, and the season
and episode chips at the top of the search re-run it for another pack or a
single episode. A rejected release asks once before it is grabbed. An
incomplete result — one indexer slow or down — is labelled as such rather
than presented as the whole picture.
Movies and series can also keep
additional quality copies, each with its own name, profile, location, and
monitoring state. Copy removal keeps every file already on disk. Discover,
Wanted, downloads/imports, the calendar, and server health are native too.
**More → Open web interface** hands off to the browser for defining profiles
and root folders, configuring indexers, clients, and notifiers, or managing
login, pairing, and API keys under Access.

**More → Display** keeps four independent preferences on this device.
**Layout** offers Classic, Plex, and Theater; Classic preserves the previous
native navigation, Plex emphasizes the bottom bar on phones and pins a rail on
tablets, and Theater moves navigation into a top deck. **Color scheme** offers
the same ten schemes as the web UI. **Appearance** defaults to Auto, follows
the device's light or dark setting, and uses dark when the device reports no
preference; Void and VHS stay dark. **Item size** changes Library tiles and
Discover results between Small, Medium, and Large, with Medium preserving the
previous density.

The API key is stored in iOS Keychain or Android Keystore. Plain `http://` is
supported because many Monarr servers live only on a trusted LAN, but it does
not encrypt the key in transit. The pairing QR also contains the key, so show
it only to a trusted device. Use HTTPS or a trusted VPN outside that LAN.
Development, real-device testing, and store build commands live in the
[mobile app guide](../mobile/README.md).

## PWA fallback (install the web UI)

The UI is a PWA: open `http://<host>:7676` on your phone and install it —
**iOS Safari**: Share → *Add to Home Screen*; **Android Chrome**: menu →
*Add to Home screen* (or the install prompt). It launches full-screen
with its own icon, and below tablet width the whole app switches to a
phone layout: bottom tabs for Library / Wanted / Calendar / Activity,
with a **More** sheet holding Dashboard, System, Settings, Add, the
global search, and the Display controls. The selected Plex and Theater
layouts also adapt the phone navigation. The calendar becomes the agenda —
the same rich list the desktop view offers, full width: poster, episode,
air time, network, status. Wide tables scroll sideways, posters run three
across. Posters are
cached for snappy loads; live data always comes from the server, so
offline you get the shell and an error rather than stale numbers. New
Monarr builds update the installed app on the next launch.

## The library page

The **All** tab shows everything, grouped into Movies / TV / Ebooks /
Audiobooks (never interleaved); those tabs show flat grids. **Each
section carries its own controls** in its header bar: a title/author
text filter, a state filter (monitored, unmonitored, missing,
incomplete, complete), and a sort (title, year, recently added, rating —
books also sort by author) with an ascending/descending toggle. The same
per-section settings drive that section's flat tab, and everything but the
text filter is remembered per browser. Every card carries the rating star,
the color-coded completeness pill, and a ↓ badge while something is
downloading for it.

A work with both editions appears in both the Ebook and Audiobook sections,
with one shared detail page. The All count still counts the work once; the
edition tabs count the editions they contain.

**Poster size** (S / M / L) in the page head — smaller to fit more of a
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
- Unavailable folders are reported only for entries with recorded media.
  Titles awaiting their first download are not problems. File records are
  retained while a whole folder is unavailable, preserving the evidence for
  repair; individual vanished files in readable folders are pruned.
- In **Settings → Disk scan → Folders needing attention**, choose **Repair
  folders…** to reconnect moved folders, then **Apply all repairs** to scan
  every entered location. Restore inaccessible drives and use **Check again**.
  See [folder repair](settings.md#disk-scan--repairing-folders-with-recorded-media)
  for validation rules and recovery when the files themselves are gone.

Scans also run on a 12-hour schedule.

## Monitoring seasons and episodes

On a series page, each season row carries a checkbox. Unticking it stops
monarr wanting that season and cascades to every episode in it; each episode
has its own checkbox for finer control. The season name toggles the episode
table open and closed, and neither control interferes with the other.

## Getting releases

### Find by external ID

The Add search accepts ordinary text plus exact `tmdb:<digits>`,
`tvdb:<digits>`, `imdb:tt<digits>`, or a bare IMDb title ID. Exact input is
validated strictly and resolves one provider record rather than falling back
to a similarly named title. If a provider returns conflicting IDs, Monarr
shows an identity-conflict error and does not add or merge the result.

On a movie or series detail page, **Identity** lists the canonical title,
external IDs, aliases, country evidence, and provider snapshot health. Use
**Refresh identity** after correcting provider data, or add a manual alias for
a real release spelling. Manual aliases are evidence; they do not change
episode numbering. **Alternate names** is collapsible and shows the name count;
lists with more than three names start collapsed. Lists of one to three names
start expanded. Expand the list to inspect provider details or remove a manual
alias. The add-alias form stays visible when the list is collapsed.

Automatic is the default posture — the interactive search is the override
for when you want to pick a specific release yourself.

**Automatic** — three entry points, all using the decision engine (the
profile's target and floor, upgrade rules, custom-format scores) to pick
the best accepted release with no list to review. Once chosen, the profile's
download priority or the item's override travels with the grab to nzbd:

- **Search on add** (checkbox on the add form, on by default) fires the
  moment an item is added.
- **Changing an item's quality profile** fires one too. Asking for a
  different target is a request, not a note — without this the item joined
  the wanted list and waited up to twelve hours for the backlog loop.
- **Auto search** (button on every detail page) does the same on demand —
  for series it tries a pack when every episode is aired, selected, and
  still wanted, then searches individual episodes if no pack was grabbed.
  Ongoing or partially selected seasons search wanted aired episodes directly.
  Active downloads suppress overlapping searches for that copy.
- The **Wanted** page groups every title once, with its missing episodes,
  copies, or editions underneath. **Search all now** snapshots every eligible
  target across every title and page. Counted Missing and Upgrade actions do
  the same for one reason; a group action stays inside that title and the
  selected reason; a child action searches only that episode, copy, or
  edition.

**Interactive search** — **Interactive search** on movie detail pages and a
separate **Search** action on each book edition; on a series, each season has
**Search pack** and each episode a **Search** button. In the web UI,
search opens in a dialog inside the current viewport, including when launched
from an episode far down the page. The title and **Close** stay visible while
results scroll; **Escape** closes it and focus returns to the button you used.
Paging reveals the new results, and grab confirmations or errors are brought
into view. The detail page's **Edit** form and the settings' **New profile** and
**Edit profile** forms use the same dialog behavior.
The native app has the same search (see [Native iOS and Android app](#native-ios-and-android-app)). Every release the
indexers returned is shown — including rejected
ones, with the reason attached (`above target`, `below floor`,
`not an upgrade`, `target met`, `language not wanted`, `does not match …`).
A release advertising audio in anything other than plain English shows
what it advertises under its quality (`German`, `MULTi`). **Grab** sends
your pick to the right client, with no decision gate — interactive search
is the override, and it always wins.

**The loops** — anything monitored and missing (or below its profile
target, or in a language the profile does not accept) stays on the wanted
list, and two loops work it unattended:

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
- **Quality** — what is on disk against the profile: `at or above …`,
  `upgrading to …`, or — when the profile requires an audio language the
  file was measured not to carry — `upgrading · no English audio on disk
  (German only)`. The measured-facts pill beside it names each file's
  languages (ADR 0022).
- **Download priority** — the effective level nzbd receives, plus whether it
  comes from the profile or is an item override.
- **Ratings** — labeled chips per source: TMDB (/10) for movies & series
  and Open Library (/5) for books come free; add an OMDb key (Settings →
  Metadata → Extra ratings) and Rotten Tomatoes, IMDb, and Metacritic
  appear alongside. Percent sources render as percentages.
- **Links** — IMDb / TMDB / TVDB / Open Library pages, opening in a new
  tab.

**Seasons** and **Files** start collapsed and show their totals in the section
header. Expand Seasons to use the existing season/episode monitoring and search
controls. Each section keeps your choice while the item refreshes; visiting
another item starts its sections collapsed again.

Actions up top:

- **Auto search** — grab the best accepted release automatically.
- **Interactive search** (movies/books; per-episode and per-season on
  series) — the full candidate list with scores and rejection reasons.
  Release titles link to the release's page on the indexer (new tab).
- **Re-measure files** — read the files again and record what is actually in
  them. Use it after fixing a permission or a mount that made a probe fail;
  a routine scan skips files it has already measured at the same size.
- **Edit** — change monitoring, quality profile, download priority, root
  folder, or the folder path itself. Choose **Profile default** to inherit,
  or one of six explicit levels from Very low through Force. Changing the
  root recomputes the folder from the naming rules; **files on disk are never
  moved** by an edit.
- **Refresh metadata** — re-fetch from the provider right now: new
  episodes for a continuing series, updated poster/status/rating. The
  `metadata.refresh` task does this for the whole library every 12 h
  (run it once from System → Tasks after upgrading to backfill ratings).

Book lookups pace Open Library requests at one per second and retry
connection resets, interrupted responses, timeouts, and temporary server
errors up to three attempts. Retries wait 1 then 2 seconds, or longer when
Open Library sends `Retry-After`. A requested wait over 30 seconds stops
that lookup instead of holding up the library refresh. Permanent errors
such as a missing work (404) are not retried. Failed work lookups leave the
book's existing metadata intact; the scheduled task continues with other
items and reports its first failure. Use **Run now** to retry after the
provider recovers.

The same completeness pill, rating, and ↓ badge appear on every library
card, so the grid shows at a glance what's complete, what's partial, and
what's moving right now.

## The Files table, and getting rid of a bad file

Expand **Files** to see 25 files per page. **Files per page** offers 25, 50,
100, 200, 500, or All; the controls above and below the table move between
pages. Changing pages brings the top controls back into view. Removing the
last file on a page returns to the last remaining page. A new item starts on
page one with 25 files per page.

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
the client, rotating cleanup attempts so a blocked entry cannot starve later
ones. Enabling the setting after a year of grabbing
clears the year of grabs too — a few dozen at a time, so the client is never
handed hundreds of deletions at once. Run it on demand from System → Tasks.

**If the client will not delete it, Monarr does.** A download client that has
lost the job, or whose delete quietly does not take, used to end the story
there: Monarr asked once, was told no, and left the payload where it was
forever. It now removes the completed folder itself — the same folder it just
finished importing from — and records that it did. This only ever touches the
directory the client reported, and never a path at, above, or inside a root
folder: a path mapping pointing into the library is not permission to delete
the library.

### Completed folders — accounting survives an empty queue

Activity → **Download storage** inventories the whole Curator-owned working
tree independently of the queue. The deployed `/working/monarr` root contains
`completed`, also mounted at `/downloads`; both names refer to the same
files. Each completed payload is shown individually alongside files elsewhere
in the working tree. The `completed` container itself is omitted from the list
and entry count; its contents still contribute to the measured bytes. Older
cached scans also hide that container without waiting for another scan.
`downloads.inventory` runs on startup and every five minutes. It includes
hidden entries, loose files, archives, and nested files, grouped by payload below `completed` or by the first
path component elsewhere in each configured root. Symbolic links are listed but their
targets are not followed. Sizes are **logical bytes**, not allocated space or
a promise of how many bytes deletion would reclaim.

| Explanation | What to do |
|---|---|
| Active | A download or import still owns this path; let it finish |
| Awaiting import | Check Activity for approval, cancellation, or a stalled handoff |
| Failed | Review the failure and use **Review import** to recover media |
| Cleanup pending | An imported payload remains; **Retry imported cleanup** queues the existing cleanup task |
| Retained | Cleanup is disabled or its client was removed; files remain accounted for |
| Untracked / ambiguous | No current record, or multiple associations; review the files and historical receipts |
| Symlink | Target bytes are excluded; check the link separately |

**Scan download storage** queues another scan. **Review import** opens the
existing manual-import preview at that entry. It does not import or delete
anything merely by opening the panel. Unknown entries and historical-only
associations are never automatically deleted. Clearing Activity preserves
path, release, and state receipts independently, including through restarts.
A receipt identifies its payload folder or files beneath that folder. A receipt
for a shared storage root or the `completed` container does not identify every
child payload. Cleared history at those containers does not block cleanup of
unrelated imported payloads; live overlapping work still retains custody.
Existing cached associations are corrected by the next successful storage scan.
Files whose records were already erased before this upgrade appear as
untracked; the upgrade cannot reconstruct that lost history.

A full download progress bar does not prove a complete payload. PAR repair,
archive extraction, or a disk write can fail after most bytes arrive. A failed
season may contain usable episodes alongside missing or unfinished ones.
Monarr preserves any payload path reported with the failure so these files
remain associated with the failed download and available for manual review.
Native Runner failure details also include its `Failure:Files` explanation
when available, such as a failed move into failed-download storage. These
failures still use the existing blocklist and bounded replacement search;
retained media is not automatically declared complete or imported.

The **Storage entries** list starts collapsed; click its heading to expand or
collapse it. The heading shows the total entry count. Storage totals, scan
status, and deletion progress remain visible while the list is closed. Closing
and reopening the list preserves its filters, page, and selection.

The expanded list defaults to **25 entries per page**. Search paths or download titles,
filter by status, and sort by name or largest first. Names and parent paths wrap
within each row so size, explanation, and actions remain visible on narrow screens.
**Select page** selects eligible entries on the current page; **Select all**
selects all matching eligible entries across pages. Changing the search or status
filter clears selection. Changing pages preserves it.

**Delete selected…** reviews the selected paths, file count, and logical size
before permanent deletion. Each entry is checked independently; the result
reports successful deletions and individual failures. Progress and results survive
navigation within the app and changes between desktop and mobile layouts; keep
the browser tab open until the batch finishes. Confirmation opens in a focused
dialog, including when you select an entry near the bottom of the list.
Entries that change after
selection must be selected again. Active downloads, awaiting imports, storage
containers, and entries from incomplete scans cannot be selected for deletion.
**Review import** remains an individual action because each payload needs a
library destination.

**Delete files…** shows a confirmation for the selected entry. Confirming
permanently removes its files from Curator-owned storage, then updates the
inventory and history. Curator rechecks ownership, active work, and the
recursive metadata fingerprint before deleting through a directory handle.
Changed files require a new scan. Library and recovery paths, storage roots,
and active download/import paths cannot be deleted through this action.
Removing a symlink removes the link, never its target.

A scan error retains the last complete inventory and shows the error and its
last successful timestamp. The health check warns on entries needing review,
failed scans, or a snapshot older than 15 minutes. Each root scan is limited
to 100,000 entries; hitting that limit is reported as incomplete, not an empty
folder. Background scans have no fixed 30-second timeout, allowing slow network
storage to finish. They respect shutdown cancellation between filesystem calls;
an unresponsive mount can still delay a call. Scans are serialized so they cannot
accumulate workers. Deletion rechecks only the selected payload under its own
30-second deadline.

A successful client delete response no longer proves local files disappeared.
When the local path is known, cleanup verifies absence, retries the guarded
local fallback inside Curator-owned storage, and leaves unsuccessful cleanup
pending. This authority includes Runner downloads completed into that tree;
Runner-owned processing and recovery paths remain outside it. Paths previously marked
removed are surfaced for review, because the path may have been reused; they
are not deleted automatically. Disabled cleanup clients do not consume the
25-attempt batch. Library roots, completed roots, symlinks, and paths overlapping another
record are protected from local deletion.

## The same release, twice

Monarr will not grab a release it already has in flight. The check is at the
moment of the grab — not only in the automation passes that precede it — so
two overlapping passes, an instant re-search after a failure, and the button
in the UI all land on the same answer: the existing download's row, not a
second copy of 90 GB. Titles are compared with the separators folded, because
`True.Lies-HDS` and `True Lies HDS` are one release and indexers disagree
about punctuation.

Replacement is bounded too. When a download fails, Monarr searches for a
replacement immediately — that is what you want the first time and the second
time. After **three failed grabs for the same want inside twelve hours** it
stops, records `regrab_capped`, and leaves the item wanted. Five releases of
one movie in ten hours is not persistence; it is a loop with your bandwidth
as its budget. A manual grab is never capped.

## Interactive search

Every candidate is listed, including the ones monarr would decline, each with
its reasons — "found nothing" and "found two hundred and disliked all of them"
are different problems. A rejected release can still be grabbed by hand;
manual grabs are never gated.

Because a busy indexer returns hundreds of rows, the list pages (50 at a time
by default), filters by name, and offers a **Would be grabbed** view. Each
row's rejections collapse to the first reason with the rest one click away.

In a phone browser or installed PWA, each result stacks its release name,
labeled metadata, rejection details, and **Grab** action within the screen.
Pagination wraps to fit. Wider screens keep the column view, with horizontal
scrolling inside the results when the panel is too narrow for the table.

A release can also carry an amber **⚠ warning**: the profile would take it,
but its advertised size cannot hold what its name claims — a "2160p Remux"
listed at 500 MB. That is a caution for you, not a refusal. Unattended
searches (RSS and backlog) do decline these outright, because nobody is there
to weigh it and the cost of guessing wrong is a fake file that ends the hunt.

Release Search explains the identity decision for every row: matched external
ID, canonical title, alias, country convention, or the exact conflict that
rejected it. The accepted explanation is stored with the grab and remains
visible on its Activity row.

The native app runs the same search against the same endpoint and shows the
same rows — the list scrolls instead of paging, and a rejected release asks
once before **Grab anyway** sends it. The one thing the phone leaves out is
the custom-format list behind the score, which the browser shows as a
tooltip.

## Monitoring: series, seasons, episodes

The series **Monitored** switch pauses or resumes acquisition. Season
checkboxes select or clear every episode in that season; episode checkboxes
refine the selection within a monitored season. Unmonitored rows dim.

**Episode monitoring** on Add and Edit stores how future metadata additions
are selected. Choosing a mode in Edit and saving reapplies it to existing
checkboxes; **Keep selections** leaves them alone. Specials start unselected.

| Mode | What it selects |
|---|---|
| All episodes | Every regular season and episode, including future additions. |
| Latest and future seasons | The latest known season when applied, plus later seasons. Older selected seasons are not cleared by subsequent refreshes. |
| Future episodes | Episodes airing today or later, using the UTC date when the mode is applied. Newly discovered older episodes remain unselected. |
| New seasons | Seasons after the last season that began before today, plus later seasons. |
| Manual selection | Clears existing selections; new seasons remain unselected. Use season and episode checkboxes to choose what to acquire. |

Refresh preserves existing checkboxes. New episodes inherit the season's
selection, constrained by the Future episodes date when applicable. Episodes
with unknown air dates may be selected, but automatic searches wait for a
known date on or before today. Pausing a series does not change its policy:
seasons discovered while paused are ready when you resume.

Existing libraries keep their current checkboxes after upgrading and default
to All episodes for newly discovered seasons. To repair an already unchecked
season, select it or apply All episodes / Latest and future seasons in Edit,
then use **Auto search**. Metadata refresh runs every 12 hours; **Refresh
metadata** fetches newly announced seasons immediately. RSS checks releases
on its configured schedule and backlog search catches up older releases.

## When an import doesn't import

The manual import panel (Activity → Manual import) validates the selected
files, queues an Activity job, and closes immediately. The copy continues in
the background, so an 18 GB file never turns the panel into a progress modal.
Expand the Activity row for the per-file result and the exact reason anything
was declined — "no files imported from `<path>`" on its own is true and
useless.

The panel starts at the completed-download folder Monarr actually sees: the
local side of a configured remote-path mapping first, then the parent of a
recent completed payload, then `/pool/downloads` as the standard-deployment
fallback. The path field is also a filesystem browser: type an absolute path
to enumerate matching folders, choose a folder to descend, or use **Parent
folder** to go back up. **Filesystem root** starts navigation at `/`. After
**Scan**, check the exact files to import. A folder with several media files
starts with none selected, because silently importing a neighbouring release
into the chosen title is worse than asking for one deliberate click.

Common reasons, and what they mean:

| Reason | What to do |
|---|---|
| `… does not improve on the … already here (profile "X")` | Only automation is gated by the profile. A manual import is the override and goes ahead anyway. |
| `no known episodes for S20 [18 19 20]` | monarr has no episode records for those numbers — refresh the series metadata first. |
| `cannot determine episodes from "<name>"` | The filename carries no `S00E00` pattern monarr recognises. |
| `item has no library folder assigned` | This can remain on older rows created before the add/grab guard. Press **Assign folder**: the form opens with Monarr's default root and exact folder name filled in. Saving queues every affected failed import automatically. New search/add and grab requests are refused before downloading when no destination exists. |

A **manual** import is never blocked by the quality profile: you pointed at
the folder and pressed Import, and that is the decision. It still only
*replaces* an existing file when the new one actually outranks it — otherwise
both stay and you choose.

An audiobook payload containing several audio files is curated as one
edition. Monarr sorts the source paths, imports every compatible part, and
names them `Title - Author - 001.ext`, `002`, and so on. A single-file
audiobook keeps the Calibre-style `Title - Author.ext` name. Ebook and
audiobook editions can share that book folder, but keep separate profiles,
monitoring, wanted state, downloads, files, and upgrades. A rescan assigns
files in a shared folder to the edition identified by their extension.

### An import that ran out of disk

An import that stops because the destination cannot take the bytes — a full
volume, a mount that went away — is now **failed**, not "mostly fine". It used
to be neither: the copy failed per file, the loop carried on, and because some
files had landed the import reported success, cleaned up the payload, and said
nothing. A season pack would import its first half, the second half would stay
listed as missing, and the next backlog pass would grab the same pack again —
which is how a download folder reaches a terabyte.

What happens now:

- the import stops at the first out-of-space error rather than repeating it
  once per remaining file
- the download is marked failed, with `import stopped: N of M files placed`,
  and the files that did land stay in the library
- the payload is **not** cleaned up — the half that is missing still needs it
- a `downloads.import-retry` task retries every 15 minutes, up to six times,
  and finishes the job the moment there is room. Nobody has to notice.

The retry is deliberately narrow. Only environmental failures repeat — a full
disk, a read-only mount, a mount that vanished. An import that failed because
monarr could not parse the payload fails the same way forever, so it is left
for a person.

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

For TV shows with multiple measured files, **Measured details** starts
collapsed so long-running shows keep a compact quality row. Open it to see
the measurements, grouped by identical facts with a file count beside each
group. The list scrolls when long; the quality badge and upgrade status stay
visible above it. Counts cover measured files in the primary copy, not
episodes or additional quality copies. A single measured file stays inline.

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

A separate copy folder is claimed by that item as soon as you add it. Even
if an earlier scan had already listed the folder for adoption, it is removed
from that queue and will not become a second library item. Series identity is
also checked across TMDB and TVDB after metadata hydration, so the same show
cannot split into two cards merely because the two paths named it through
different providers.

## Activity, and keeping it finite

The Activity page leads with what is **moving**: the in-flight sections
(Downloading, Verifying, Repairing, Copying to library…) are always shown
whole, because a partly-hidden answer to "what is happening" is not an
answer.

Everything terminal is history, and history is collapsed:

- **Finished** and **Failed** are headings with a count. Open one and it
  loads 25 rows at a time — "Show 25 more" goes further back.
- **Filter by release title** narrows every group at once.
- **Clear finished** removes the finished rows. The *rows*: an imported
  download's row is a receipt, and the files are in your library under the
  library's own records. Nothing is deleted from disk, from the library, or
  from the download client. It asks once before it does it.
- **Clear failed** sits on the Failed grouping and removes every failed row
  after the same inline confirmation. It dismisses Activity records only:
  payloads, library files, blocklists, and download-client jobs stay as they
  are. In the native app, select the Failed filter to reveal the matching
  button beneath the filters.
- A single row can be dismissed with **Remove**, which has always been
  there and has always meant the row only (there is a separate option for
  telling the client to drop the download too).

Underneath, **Activity retention** (Settings → Library) ages out finished
and failed rows, and the events behind them, after a number of days —
30 by default, `0` to keep everything. A daily task does the sweep. It only
ever touches terminal rows: a download stuck in "grabbed" for six weeks is
something to look at, not something to hide, so it stays no matter how old
it is. Before this, nothing was ever deleted: every grab monarr had ever
made was still in the table, and the page rendered the most recent hundred
of them forever.

## Activity, calendar, wanted

- **Activity** shows the download queue with live progress; completed
  items are imported automatically within ~30 seconds (the `queue.refresh`
  task). Imported files are renamed to the library layout —
  `Movie (Year) [Quality].ext`, `Show - S01E02 - Title [Quality].ext`,
  `Author/Title/Title - Author.ext` — using hardlinks when the download and
  library share a filesystem. **Expand any row** (the ▸) to see the
  handoff, below.
- **Calendar** has two views, switched with the **Month · Agenda** toggle in
  the page head. The choice is remembered.
- **Wanted** shows one collapsed row per library title. Expanding a show lists
  its episodes in numeric season/episode order; additional copies and book
  editions stay separate targets. The Missing/Upgrade selector narrows both
  the children you see and a group search's scope. Text search matches title,
  detail, copy, media kind, reason, quality, and the raw target id, but a hit
  keeps every sibling allowed by the reason filter so the group's count and
  action never lie.
- Pagination is by title, not episode. **Titles per page** remembers
  25/50/100/200/500/all, while **Search all now** and the visible
  **Search all Missing/Upgrade** actions always span every title and page.
  A group action searches that complete title (or its selected reason), and
  **Search episode/item** names one exact target including its copy or
  edition discriminator.
- Explicit Wanted searches run as durable background work, one target per
  queue job. The progress panel survives reload, separates searched, skipped,
  failed, and grabbed counts, exposes per-target outcomes, and can cancel the
  remainder. Cancellation never removes downloads already submitted. The
  advanced delay is the minimum gap after one target finishes and before the
  next starts; it does not override an indexer's timeout or rate limit.
- Series actions on Wanted deliberately search individual episodes and reject
  season packs, because one pack recorded against one child could otherwise
  be grabbed again for its siblings. This can cost up to three queries per
  indexer per episode. Use the item's **Auto search** when season-pack breadth
  is what you want; that broader action is not a substitute for a
  Missing-only scope.

### The calendar's two views

**Month** is the grid. Every entry is a chip on its day, and the chip's
colour is its *state* — nothing else uses colour, so the legend under the
grid is the whole vocabulary:

| Chip | Means |
|---|---|
| green, filled | on disk |
| red, filled | it aired (or released) and you don't have it |
| outlined | still ahead — upcoming, not missing |
| faded | not monitored: still news, but nothing is hunting for it |

A small glyph, not a colour, marks the kind: **▸** movie, **▪** book,
nothing for the common case of an episode. A day with more than four
entries shows the first four and a **+N more** button, which opens that
day as full rows rather than stretching the week. ← / Today / → page the
month.

**Agenda** is a chronological list, opening pinned at today and scrolling
outward: sticky day headers (*Today*, *Tomorrow*, then weekday and date),
poster thumbnails, `S02E04 — Episode title · 34 min`, the network, the air
time, and a status pill. It keeps loading forward as you scroll; **Load
earlier** at the top walks backwards. On a phone the whole page is this
list — a seven-column grid at 390px is unreadable.

Two URL parameters, both optional: `?view=month|agenda` deep-links a view
(and beats the remembered choice), and `?date=YYYY-MM-DD` anchors the month
shown, or the day the agenda opens at.

### Where the times come from

Air times are looked up from **TVmaze**, which needs no key and no account
and is already part of the series metadata chain. A series refresh stores
the show's slot — the network's local air time, its timezone, the channel
name — and each entry's exact instant is worked out from that when the
calendar is read, so the daylight-saving change and a show moving nights
are both handled without anything being re-fetched. Times appear in *your*
local timezone.

**Streaming shows, movies and books stay date-only, and that is the source
being honest rather than a bug.** A Netflix original has no broadcast
instant to publish; showing it at midnight would put it first in every
evening's list and mean nothing. Where there is no stated schedule, Monarr
shows no time.

Times fill in on the normal metadata refresh cadence, so a library that has
just upgraded gets them over the following day — or immediately, if you run
**metadata.refresh** from the System page or hit **Refresh** on an item.

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

For nzbd, the admission request also carries the effective download priority.
Normal is `0`; High and Very high move the job ahead of normal work; Low and
Very low let other work go first. Force (`900`) is an operator exception that
can run through queue pauses and quota holds, but never through nzbd's disk
safety guard. Download clients without a native priority capability continue
to receive the grab normally and ignore this field.

Every row carries the controls for its state:

- **Import now** — on a download **awaiting approval** (from a client set to
  require approval), imports it right now.
- **Retry** — on a **failed** row, re-runs the import using the same path,
  for after you've fixed a mount or added a path mapping.
- **Restart import** — on an interrupted **importing** row with no live
  worker, queues the saved payload again. Monarr also recovers these rows on
  the next completed-download observation after a restart.
- **Cancel import** — stops a queued or live import, removes any unfinished
  destination temp file, keeps the completed payload, and returns the row to
  a restartable **downloaded** state.
- **Manual import** — browse Monarr to the real files (a folder or a single
  file), select exactly which scan results belong, pick the target title and
  copy, and queue the import.
  Use it when automation couldn't resolve the payload, or to pull in files
  you placed by hand. It's also on the top of the Activity page for imports
  not tied to any download.
- **Blocklist** — declare the release bad: blocklist it so it's never
  grabbed again and search for a replacement. This is the *only* way a
  handoff gets blocklisted — an import failure never does it on its own, so
  a mount problem never churns silently through replacements.
- **Remove** — drop the row (leaves the client and any files alone).

An **importing** row is only called live when a worker exists. A live row
shows bytes copied, total bytes, average rate, elapsed time, and the current
detail. A durable row left behind by a stopped process instead says
**interrupted · No importer is running**; a full progress bar is not used as
a substitute for status.

An accepted import waiting behind the two bounded workers sits under
**Waiting to copy**, says **queued**, and offers **Cancel import**. It is not
an interrupted job and pressing Retry again is neither necessary nor
accepted. The durable handoff trace records `Queued for import` before the
worker starts.

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
**Unmonitor** / apply a quality profile. A book profile can only update the
matching medium; applying an audiobook profile never converts or replaces an
ebook. Add or remove the other edition from the book's **Editions** panel.

## Connecting the ecosystem

Monarr impersonates Sonarr and Radarr for apps that speak their v3 API
(see [adr/0003](adr/0003-compat-personalities.md)):

| App | Point it at | Notes |
|---|---|---|
| Jellyseerr/Overseerr | `http://<host>:7676/sonarr` and `…/radarr` | requests create real Monarr items |
| Prowlarr | same URLs, as "Sonarr"/"Radarr" apps | its indexer sync lands in Monarr's indexer list |
| Bazarr | same URLs | series/episodes/files enumerate normally |

All three ask for an API key: **Access → API access → Reveal**. Anything a
consumer calls that the shim doesn't implement is logged
(`compat: unknown v3 request …`) — send that log line in an issue.

## Notifications

Settings → Notifications. Webhooks and Discord get grab/import/failure
events; Plex and Jellyfin entries are library-refresh pokes fired after
imports. **Test** delivers a test event immediately.

A **plurx** entry is not a poke: it receives the exact paths that landed and
the identity Monarr already knows, so plurx indexes one folder instead of
sweeping a library — and identifies it without guessing at the filename.
Movies and series carry their normal ids. Books carry Curator's title,
author, ebook/audiobook medium, stable work and edition ids, and a bounded
Open Library cover URL when available. Exact work ids relate editions; title
and author do not. The notifier holds a scoped `plx_` key, never a plurx
admin token. See `docs/settings.md` for setup and failure meanings.

plurx deliveries are queued and retried; the **Delivery log** on the notifier
row shows each one, and a delivery that ran out its schedule can be sent
again from there — **Retry** on the row, or **Retry all failed** for the lot
once plurx is healthy again.

## Troubleshooting quick hits

- **"payload missing" on import** — Monarr can't open the path the
  download client reported. Fix the container mounts so both see the same
  path, or set a **remote path mapping** on the client (Settings →
  Download clients — see `docs/settings.md`).
- **Search finds nothing** — check the indexer Test passes. With categories
  left empty, Monarr routes ebooks to 7000/7020 and audiobooks to 3030;
  explicitly configured indexer categories still override those defaults.
- **Everything rejected** — read the rejection reasons; usually the
  profile doesn't allow the found qualities, or the release title doesn't
  match (year off by >1, different title).
- **Wanted list looks stale** — it refreshes on library events; running a
  scan or any grab/import refreshes it, as does restarting.

## Runner payload cleanup

For native Runner clients, cleanup first requests deletion through Runner and
verifies that the local payload disappeared. Runner-owned source and recovery
trees stay pending when Runner cannot remove them. Curator may remove a verified
imported payload directly inside its own declared working trees, including
`/working/monarr/completed`; it never applies that fallback to Runner-owned
`/processing` or recovery storage.
Queue removal falls back to History only when the queue returns HTTP 404.

## Recover media retained by Runner

1. In Runner's Files view, inspect the folder. Adopt unknown files explicitly;
   adoption starts with Keep enabled.
2. Select regular media files and stage recovery. Runner copies and verifies
   them and holds the original. Temporary, archive and parity files cannot be
   selected as media.
3. In Curator Settings → Recovery, choose the handoff from the table (it is
   named by its source folder; a copy Runner is still making or failed to
   make cannot be chosen, and says why), search for the library title, map
   series files to `season:episodes` if the filenames do not say, then
   preview. Preview verifies each digest and probes its container. If the
   readiness strip says the recovery mount is missing, fix the mount first —
   nothing on this page can work without it.
4. Queue the import from that preview. A changed target requires another
   preview. Follow the durable import status until Runner receives the receipt.

Library placement now journals intent before publishing files. A same-name
replacement retains a rollback copy until library metadata, episode links and
its per-file recovery receipt commit in one transaction. SQLite uses FULL
synchronization for those receipts. Failed/partial recovery stays on hold;
ordinary payload cleanup never handles a recovery source or staging tree.

Use the Recovery activity list to cancel or inspect work after closing the page.
A cancelled import may have committed earlier files; those remain in the library
with their receipts. Runner releases the claim only after the worker acknowledges
that it stopped, then keeps the source for review.

Recovery inspection uses only Curator's bounded native parser. The UI reports
“Media recognized; completeness not proven.” An intact copy can faithfully copy a
bad source. Review episode mappings and concrete destinations before importing.
A stale library revision requires a fresh preview, including after a concurrent
ordinary import or metadata edit.

Back up Curator's database and library in one coordinated maintenance window.
After restoring either, keep Runner sources held while reviewing recovered
placement intentions and outbox records. Do not restore old metadata while
pre-restore workers continue writing to the library.

Recovery selection is sealed in Runner at staging time. Curator imports every
file in that handoff together; stage separate handoffs for alternate movie
versions. Overlapping episode assignments or destinations require a new preview.
Recognized obfuscated video keeps its source filename and receives a native
container extension at its library destination. Cancellation rolls back any
published placement that has not yet committed library metadata.

## Runner holds and retained recovery plans

Activity shows a Runner storage or review hold separately from transferred
bytes. A 100% transfer still needs verified post-processing and import.
**Resume same job** calls the existing Runner job's resume action. The active
acquisition remains in the database until a newer authoritative control fact
resolves the corresponding hold; late completion/failure events cannot erase it.
Season and episode grabs check held overlap at final admission. Progress keeps
updating between control transitions. If the resume event is missed, terminal
history carries the resolving revision and permits the normal completion path;
a legacy completion without that evidence still retains the hold.

`POST /api/v1/import/recovery/retained/plan` accepts up to 1,000 candidate
files and returns per-file identity, digest, intended episode/copy destination,
existing library digests, duplicates, capacity forecast and blocked reason.
Candidate evidence includes an expected SHA-256 and complete-coverage claim.
Known partials, active parent paths and pending placement acknowledgements
remain blocked. This endpoint only observes files; its result grants no lease,
import permission or cleanup authority. Use the existing recovery preview and
placement/receipt workflow for separately authorized recovery.

## Automatic season acquisition

Series automatic search queues a season/copy comparison. It collects season
results and target-quality single alternatives for missing episodes before
submitting an ordinary plan. Missing work precedes upgrade-only work; existing
upgrade queries use season results and a seven-day deep-search cadence.

The planner maximizes attainable eligible outcomes up to your profile target.
An adequate pack does not add better-source or custom-format singles on top.
A below-target pack may pair with higher-class singles. Full pack bytes count,
including protected payload. Reported zero torrent seeders affect equivalent
choices before advertised size; unknown seeders break ties after bytes.

Activity → **Why chosen** opens the recorded target, providers, full transfer
cost, gaps and import permissions inside the viewport. Identity match is shown
separately. Legacy and manual rows say their selection was not recorded. Planned
rows reserve episodes without claiming that a client job already exists.

Uncertain submissions keep episode reservations while client inventory is
reconciled. After thirty minutes the broad season claim can settle so other
holes proceed. Seven days of complete successful unchanged-client inventories,
with no gap over six hours, permits a retry with a persistent **Possible
duplicate** warning. Absence is not proof that the original was never accepted.
A late original cannot publish automatically after its authority is superseded.
Holds and user cancellation do not expire under this policy.

Torrent retirement uses observed bytes, connected seeds and availability:
48 observed hours without progress and poor availability, or seven observed
days regardless of seeds. Pauses/outages accrue no time. Confirmed retirement
allows replacement while unsafe partial bytes retain explicit cleanup custody.
A healthy or paused transfer is preserved when its broad plan settles at 48 hours.

Explicit removal cancels the plan's remaining unsubmitted intents. Submitted or
uncertain rows retain a visible operator cancellation reservation; deliberate
review is required to release custody. Automatic replacement and pack fallback
cannot override that fence. A client with bounded history cannot supply complete
absence evidence; its uncertainty remains reserved instead of silently retrying.

**Review reservation** opens the unresolved row's custody review in Activity.
After checking the client, explicitly acknowledge possible duplicates to release
that reservation. The original identity remains superseded and cannot publish
later. This action does not add a replacement; use a deliberate Search afterward.
