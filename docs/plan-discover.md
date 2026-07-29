# Plan — Discover, a browse surface inside Monarr

**Status:** built 2026-07-29 (v0.10.0; poster size v0.11.0) · **Written:** 2026-07-29 ·
**Decision:** ADR [0015](adr/0015-discovery.md)

Monarr can find anything you can name and nothing you cannot. This plan adds
the other half: rows of *what is good right now*, so the answer to "what
should I watch" stops living in a browser tab.

How to work from this document: milestones in order, each ending in a
runnable acceptance check. Every identifier in §2 was copied from the code on
2026-07-29 with the file it came from — re-verify against that file before
building on it, in case it moved. §6 is the non-goals list; if a step seems to
require doing something in it, stop and flag it instead of improvising.

---

## 1. Objective — fourteen rows, no new state, one door into Add

A `/discover` page of horizontally scrolling poster rows, each row a curated
list from a metadata provider, each poster addable without leaving the page.

Three properties decide whether this was built correctly:

1. **It costs nothing when nobody is looking.** No job, no table, no
   background traffic. Every request originates in a browser that is on the
   page, and rows fetch as they approach the viewport.
2. **A bad upstream degrades, it does not blank.** A row that cannot be
   refreshed serves its last good contents for up to 24 hours (ADR 0015 §4).
3. **Adding from a row is the same add as adding from search.** Same service
   call, same kind-filtered root folders (ADR 0009), same profile defaults.
   Not a second policy that drifts.

---

## 2. Contract — what exists today, verbatim

### 2.1 The seam that already exists (`internal/adapters/tmdb/client.go:409`)

```go
// DiscoverMovies serves the import lists (Phase 5): kind is "popular" or
// "top_rated"; results carry TMDB ids ready for library adds.
func (c *Client) DiscoverMovies(ctx context.Context, kind string) ([]ports.SearchResult, error)
```

Movies only, no paging, no TV, no trending. It has exactly one caller,
`internal/app/importlist/importlist.go:69`, and that caller must keep working
unchanged (§6).

### 2.2 The result type being reused (`internal/ports/metadata.go:21`)

```go
type SearchResult struct {
	Kind   domain.MediaKind
	TMDBID int64
	TVDBID int64
	Source string
	OLID, Author, Title string
	AltTitles []string
	Year       int
	Overview   string
	PosterPath string
}
```

No popularity, votes or backdrop fields, and none are added — a row is
already ordered by the provider, so a score column would be decoration the UI
does not read.

### 2.3 The TMDB request path being reused (`client.go:70`)

`func (c *Client) get(ctx, path string, params url.Values, out any) error` —
key from `KeyFunc` per call, `""` → `ports.ErrProviderNotConfigured`, JWT keys
(`eyJ` prefix) sent as `Authorization: Bearer`, others as `api_key=`;
`rate.NewLimiter(10, 10)`; a URL-keyed in-memory response cache at **5 min**;
8 MiB body cap. Discover adds paths through this function and inherits all of
it.

### 2.4 The settings pattern being copied (`internal/api/library_handlers.go:21`)

```go
const TMDBKeySetting = "tmdb_api_key"
const OMDBKeySetting = "omdb_api_key"   // same file
```

Read via `s.readSetting(ctx, k)`, returned masked as
`{configured bool, hint string}` by `GetSettings`, written by
`UpdateSettings`. `trakt_client_id` joins them with no schema change —
`app_meta` is a key-value table.

### 2.5 The "already in the library" precedent (`library_handlers.go:559`)

`SearchMetadata` loads the whole library for the kind via
`library.Service.List` and builds TMDB/TVDB/OLID maps. Correct for one search;
wrong for fourteen rows per page load, which is why §3.3 adds a narrow query.

---

## 3. The build

### 3.1 M1 — the port (`internal/ports/discover.go`)

`DiscoverList` and `DiscoverProvider` exactly as in ADR 0015 §1, plus
`ErrUnknownList`. The `ImportListProvider` row in
[`internal/ports/README.md`](../internal/ports/README.md) is the reserved slot
this lands next to; add a `DiscoverProvider` row rather than claiming that one
— they are different jobs (§6).

**Acceptance:** `go test ./internal/ -run TestArchitecture` still passes
(ports may import domain and stdlib only).

### 3.2 M2 — the two providers

**TMDB** (`internal/adapters/tmdb/discover.go`), nine rows:

| List id | Row title | Kind | Upstream |
|---|---|---|---|
| `tmdb-trending-movies` | Trending this week | movie | `/trending/movie/week` |
| `tmdb-now-playing` | In theaters now | movie | `/movie/now_playing` |
| `tmdb-upcoming` | Coming soon | movie | `/movie/upcoming` |
| `tmdb-popular-movies` | Popular movies | movie | `/movie/popular` |
| `tmdb-top-movies` | Top rated movies | movie | `/movie/top_rated` |
| `tmdb-trending-series` | Trending this week | series | `/trending/tv/week` |
| `tmdb-on-the-air` | On the air | series | `/tv/on_the_air` |
| `tmdb-popular-series` | Popular shows | series | `/tv/popular` |
| `tmdb-top-series` | Top rated shows | series | `/tv/top_rated` |

Movie-shaped responses (`title`/`release_date`) and TV-shaped ones
(`name`/`first_air_date`) decode through the existing `searchMovieResp` /
`searchTVResp`. `DiscoverMovies` (§2.1) is re-expressed over the same helper
so there is one code path, and keeps its signature.

Also new here: `Summary(ctx, kind, tmdbID) (ports.SearchResult, error)` — the
shallow hydrator of ADR 0015 §3. One request, no seasons.

**Trakt** (`internal/adapters/trakt/`), five rows:

| List id | Row title | Kind | Upstream |
|---|---|---|---|
| `trakt-trending-movies` | Being watched right now | movie | `/movies/trending` |
| `trakt-trending-series` | Being watched right now | series | `/shows/trending` |
| `trakt-anticipated-movies` | Most anticipated | movie | `/movies/anticipated` |
| `trakt-anticipated-series` | Most anticipated | series | `/shows/anticipated` |
| `trakt-boxoffice` | Box office | movie | `/movies/boxoffice` |

All five return the entity wrapped in an envelope (`{watchers, movie:{…}}`),
which is why `/movies/popular` — which returns bare movie objects — is not in
the table. The adapter gains what it never had: a `KeyFunc` (so the client id
can come from settings and change without a restart), a rate limiter, and the
same URL-keyed response cache TMDB has. `ListItems` keeps its behaviour for
import lists via `NewStatic`.

**Acceptance:** `go test ./internal/adapters/tmdb/ ./internal/adapters/trakt/`
green, with httptest fixtures for one movie-shaped row, one TV-shaped row and
one Trakt envelope.

### 3.3 M3 — the service (`internal/app/discover/discover.go`)

```go
func New(log *slog.Logger, hydrate Hydrator, providers ...ports.DiscoverProvider) *Service
func (s *Service) Lists(ctx context.Context) []ports.DiscoverList
func (s *Service) Configured(ctx context.Context) bool
func (s *Service) Items(ctx context.Context, listID string, page int) ([]ports.SearchResult, error)
```

`Lists` concatenates the catalogues of providers whose `Configured` is true.
`Items` resolves the owning provider by list id, serves from a
`map[listID#page]` cache on a 30-minute TTL, hydrates missing posters through
`Hydrator` at concurrency 6, and on upstream failure serves a stale entry up
to 24 h old before returning the error (ADR 0015 §4).

The service does **not** mark results against the library; the handler does,
following `SearchMetadata`'s precedent, so the service stays a fetch-and-cache
with no storage dependency. What this milestone adds for the handler to use is
one narrow query:

```sql
-- name: TmdbIDsByKind :many
SELECT tmdb_id FROM media_items WHERE kind = ? AND tmdb_id != 0;
```

surfaced as `library.Service.KnownTMDBIDs(ctx, kind) (map[int64]struct{}, error)`.

**Acceptance:** `go test ./internal/app/discover/` covering the four cache
behaviours that matter — fresh hit, expiry, stale-served-on-error, and stale
expired past 24 h returning the error.

### 3.4 M4 — the API

Two paths in [`internal/api/openapi.yaml`](../internal/api/openapi.yaml):

- `GET /discover/lists` → `DiscoverList[]` (id, title, blurb, kind, source)
- `GET /discover/items?list=<id>&page=<n>` → `SearchResult[]` with `inLibrary`
  set; `400` on an unknown list, `503` when the provider is unconfigured

Handlers in a new `internal/api/discover_handlers.go`; `Deps` gains
`Discover *discover.Service`. `Settings` gains
`traktClientIdConfigured`/`traktClientIdHint` on the way out and
`traktClientId` on the way in, copying OMDb exactly (§2.4).

Then `make gen` — `apigen.ServerInterface` will not compile until the handlers
exist, which is the point.

**Acceptance:** `make gen && git diff --exit-code -- internal/api/gen` clean
after committing the regenerated file; `curl /api/v1/discover/lists` returns
the nine TMDB rows on an install with a key and no Trakt id.

### 3.5 M5 — the page (`web/src/pages/Discover.tsx`)

Rows stacked in `.lib-section` containers, each row a horizontally scrolling
`.discover-row` with scroll-snap and desktop arrow buttons. Kind tabs
(All · Movies · Shows) filter which rows render, matching the Library page's
`.tabs` idiom. A row fetches only once it is within 400 px of the viewport
(`useInView`, one `IntersectionObserver`).

Clicking a poster opens a modal on the existing `.modal-backdrop`/`.modal`
pattern: poster, year, overview, and either **Add** with root-folder/profile/
monitor controls prefilled from settings defaults, or a link to the item when
it is already in the library.

Registration is four places, all in `web/src/router.tsx` plus one:
route, sidebar `<nav>`, the mobile **More** sheet, and the `PAGES` array in
`web/src/globalsearch.tsx`.

**Acceptance:** `make test-web` green, `npm run typecheck` clean.

### 3.5a Follow-up — poster size, shared with the Library page

Added the same day, on feedback that the default was right but unadjustable.
An **S / M / L** control — labelled *Poster size*, because three letters
with no subject tell a reader nothing — in the page head of Discover *and*
the Library page,
carried as `data-card-size` on `<html>` and read by CSS through a `--card-w`
custom property — the theme picker's mechanism, for the theme picker's
reason: two unrelated components that must agree on a number are cheaper as
one attribute than as shared state, and CSS then resizes the grid without
re-rendering it. A pre-paint script in `index.html` applies it before first
paint, or the whole library grid reflows a frame after it draws.

The two surfaces consume the token differently and that is not a bug: a strip
card is exactly `--card-w`, while a grid column is `minmax(--card-w, 1fr)`
under `auto-fill` and so gets stretched to divide the row. M resolves to the
150px both pages already used, so an install that never touches the control
is unchanged.

**Acceptance:** `zz-cardsize.spec.ts` measures real geometry at each size on
both pages, across a reload, and asserts the strip card equals the resolved
token while the grid card is at least it.

### 3.6 M6 — e2e (`test/e2e/tests/zz-discover.spec.ts`)

`test/e2e/fake-tmdb.mjs` gains the nine upstream routes. The spec sets its own
TMDB key in `beforeAll` (the `smoke.spec.ts` pattern) so it does not depend on
spec ordering for setup, and is named `zz-` because it adds a library item and
earlier specs count `.poster-card`s on the library page.

Covers: the nav link navigates; rows render with headings; the kind tabs
filter; a poster opens the modal; **Add** puts the item in the library and the
card comes back marked.

**Acceptance:** `make test-e2e` green, full suite, not just the new spec.

---

## 4. Order of work

M1 → M2 → M3 → M4 → M5 → M6, then the gate in §5. M2's two providers are
independent of each other and M5 can start against a hand-written fixture as
soon as M4's shapes are in the spec.

## 5. The gate

Per [CLAUDE.md](../CLAUDE.md), nothing ships until all four pass on the code
being delivered:

```bash
make lint        # golangci-lint + gofmt
make test        # go test ./...
make test-web    # vitest
make test-e2e    # playwright, full suite
```

E2E is mildly flaky under load — re-run a lone failure before believing it.

Two failures found here were **already red on `main`** and had nothing to do
with this work; both were repaired, and both are test-or-CSS only:
`smoke.spec.ts` kept two copies of the static health-check list and one had
never learned about `imports`, and `phase9-layout.spec.ts` was correctly
catching a real regression from the Activity rewrite (68575a3) that starved
the release column to ~60 px. Result: **90/90**.

## 6. Non-goals — do not do these

- **Do not change `DiscoverMovies`' signature or the `ImportListType` enum.**
  Import lists are a different feature with a different job (ADR 0015,
  alternatives). Re-expressing `DiscoverMovies` over a shared helper is fine;
  changing what it returns is not.
- **Do not add a table, a migration or a scheduled job.** The cache is memory
  (ADR 0015 §4). If a step seems to need persistence, stop and flag it.
- **Do not add popularity/vote/backdrop fields to `ports.SearchResult`.** The
  provider's ordering is the ranking; a number the UI does not render is a
  field that will drift.
- **Do not build a recommendation row off the plurx watch data.** Explicitly
  deferred with a stated trigger (ADR 0015, alternatives).
- **Do not merge TMDB and Trakt rows into one ranked list.** Two sources
  offering different rows is the feature; a blended ranking is unverifiable.
- **Do not hydrate series artwork through `GetSeries`.** It fetches every
  season. `Summary` exists for this and only this.
- **Do not add books.** Open Library has no popularity surface worth the row;
  a sparse books strip next to a full TMDB one reads as broken.
