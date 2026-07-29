# ADR 0015 — Discovery is a read-only browse surface: TMDB always, Trakt when keyed, nothing stored

- **Status:** Accepted — built 2026-07-29 (v0.10.0; poster-size control v0.11.0)
- **Date:** 2026-07-29
- **Relates to:** ADR [0009](0009-root-folder-kinds.md) (a root folder carries
  a kind, which is what an add from a discover row has to respect), ADR
  [0011](0011-series-metadata-provider.md) (the provider *chain* — this
  decision deliberately does not use it), ADR
  [0004](0004-sqlite-only.md) (why the cache is memory and not a table)

## Context

Monarr can find anything you can already name. `GET /metadata/search` takes a
query and returns candidates; the Add page is a search box. Every path into
the library starts with the user knowing what they want.

That leaves an obvious hole, and it is the one the user named: *"so you don't
necessarily have to even leave it if you are looking for more media."* The
answer to "what should I watch" currently lives in a browser tab — TMDB,
Trakt, JustWatch, a subreddit — and the round trip back into Monarr is
copy-paste-a-title. Every comparable tool (Overseerr, Jellyseerr, Plex's own
Discover) closed this hole years ago, and closing it is cheap here because the
metadata provider is already wired, keyed, rate-limited and cached.

The existing seam is narrow but real: `tmdb.Client.DiscoverMovies(ctx, kind)`
with `kind ∈ {popular, top_rated}`, movies only, no paging, reachable only
through import lists (Phase 5). Import lists answer a different question —
*add all of these to my library on a timer* — and answering "show me what's
good" by making the user create an auto-adding list is the wrong shape. A
discover row is looked at; an import list is obeyed.

## Decision

**A read-only Discover surface, assembled from curated provider lists, that
stores nothing and adds nothing on its own.**

Four parts.

### 1. A `DiscoverProvider` port, catalogue-first

```go
type DiscoverList struct {
	ID     string           // stable slug: "tmdb-trending-movies"
	Title  string           // row heading: "Trending this week"
	Blurb  string           // one line under it, saying what the row means
	Kind   domain.MediaKind // movie | series
	Source string           // "tmdb" | "trakt"
}

type DiscoverProvider interface {
	Name() string
	Lists() []DiscoverList
	Configured(ctx context.Context) bool
	Discover(ctx context.Context, listID string, page int) ([]SearchResult, error)
}
```

The provider publishes its own catalogue rather than the app hardcoding row
ids. That is what makes a second provider additive: Trakt's five rows appear
because Trakt's `Lists()` names them, not because the UI learned about Trakt.
`Configured` exists so the catalogue can be honest about a provider whose key
is absent — the alternative is offering a row that always errors.

The port returns the existing `ports.SearchResult`, not a new type. A discover
row *is* a search result list with a different producer, and reusing it means
the Add page's card, the `inLibrary` marking and the add mutation all work
unchanged.

### 2. TMDB is the baseline, Trakt is optional and additive

Nine TMDB rows, five Trakt rows. TMDB needs no new configuration — the key is
already required for the library to work at all, so Discover is populated for
every existing install on upgrade with no setup step. Trakt needs a free
client id in Settings; when it is absent, its five rows are simply not in the
catalogue and the page does not mention it.

They are kept because they answer different questions, and the blurbs say so:
TMDB's "trending" is *what people are looking up*, Trakt's is *what people are
watching right now* (Trakt's numbers come from scrobbles — actual playback).
"Most anticipated" and "Box office" have no TMDB equivalent at all.

**We did not use the ADR 0011 provider chain here.** The chain exists to
resolve *one* series identity when providers disagree. Discovery has no
identity to resolve and no disagreement to settle — two providers offering
different rows is the feature, not a conflict. Merging them would mean
inventing a cross-source popularity ranking nobody could check.

### 3. Trakt rows are hydrated for artwork, one shallow call per item

Trakt's API returns ids, titles and years; artwork is not in the free tier.
A row of thirty untitled grey boxes is not a browse surface, so results
missing a poster are hydrated through TMDB by id.

The hydrator is `Summary(ctx, kind, tmdbID)` — a **new, deliberately shallow**
TMDB method returning poster, overview, year and nothing else. It is not
`GetSeries`, which hydrates every season and every episode: a thirty-item
Trakt series row through `GetSeries` is thirty series plus a hundred-odd
season fetches, which would blow the rate limiter for a picture. Hydration
runs at a concurrency of six, and an item whose hydration fails keeps its
title and loses its poster rather than failing the row.

### 4. The cache is in memory, keyed by list and page, and serves stale on error

No table, no migration. A row is cached for **30 minutes** — trending data
moves daily, so a 5-minute TTL (the TMDB client's own) would spend requests
re-fetching numbers that had not changed, and an hour would make the "this
week" rows feel wrong on the day they roll over.

When an upstream fetch fails and a stale entry exists, **the stale entry is
served** and the failure is logged. This is the deliberate part: a Discover
page that goes blank because TMDB had a bad thirty seconds is worse than one
showing yesterday's trending list. A stale row is still true, just less
fresh; an empty page is a bug report. Stale is only served for **24 hours**,
after which the error surfaces — past a day the row is no longer "less
fresh", it is wrong, and pretending otherwise hides an outage.

Cold start costs one upstream call per row actually looked at, because the UI
loads rows lazily as they approach the viewport rather than fetching fourteen
at once.

## Consequences

- Discovery is **additive and reversible**: no schema change, no new job, no
  background traffic. Nothing runs unless a browser is on the page. The
  feature can be deleted by removing a route.
- **`inLibrary` is answered by a new cheap query** (`TmdbIDsByKind`) rather
  than the full `Library.List` the metadata-search handler uses. Fourteen
  rows × a full library hydration per page load was the one place this
  feature could have become expensive, and it is the one place we spent a
  query on.
- **Adding from a row obeys the same rules as adding from search** — root
  folder filtered by kind (ADR 0009), profile defaulted from settings,
  `searchNow` off unless monitored. The Discover modal is a different door
  into `library.Service.Add`, never a second policy.
- **A row can lie about freshness for up to 24 hours** during an upstream
  outage. Accepted, and bounded; the alternative failure (a blank page) is
  worse and less legible.
- **Trakt's client id joins TMDB's and OMDb's in `app_meta`**, masked in the
  settings response like the others. Import lists keep their per-list client
  id: those predate the setting, and a list that already works must not stop
  working because a global field is empty.
- **No personalization.** See below.

## Alternatives considered

- **A "because you watched X" row from the plurx watch webhook.** The data is
  already stored (migration 26, per-user, replacing on rewatch). Rejected for
  now on honesty grounds: recommendations from a handful of watch events are
  noise wearing the costume of insight, and the row would be indistinguishable
  from "trending" until months of history accumulated. Revisit when the watch
  table has enough rows to evaluate against — the trigger is a real one, not a
  gesture.
- **Model discover rows as read-only import lists.** Tempting because the
  vocabulary (`tmdb-popular`, `tmdb-top`) already exists. Rejected: import
  lists carry a root folder, a profile, a monitored flag and a sync job,
  because their job is to *add things*. A discover row has none of those and
  wants paging, which lists have no concept of. Sharing the type would mean
  four columns that must be ignored and one behaviour that must be suppressed.
- **Cache rows in SQLite so they survive a restart.** A migration, a cleanup
  job, and a staleness policy in two places, to save one upstream call per row
  after a restart that happens weekly at most. Not worth it. Revisit if
  someone runs Monarr somewhere TMDB is slow or metered.
- **One `/discover` endpoint returning every row fully populated.** One
  request instead of fifteen, but it forces every row to be fetched whether or
  not it is looked at, and it makes the slowest upstream row the latency of
  the whole page. Per-row endpoints let the UI load what is on screen and let
  one row fail alone.
