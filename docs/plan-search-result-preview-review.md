# Review — monarr `docs/plan-search-result-preview.md`

**Verdict: APPROVE WITH CHANGES.** The product decision is right and I would not
reopen it: drawer on desktop, full screen on phones, Add kept as a separate
one-click action, render the search payload immediately and enrich after
selection, known identity only, add policy owned by the existing flow. Two
must-fix items, both in §5: the contract was written against a checkout that is
ten commits behind `main`, and most of what §5.2–§5.3 proposes to build already
landed there on 2026-09-17; and the enrichment-merge rule as written would break
the Add flow it promises to preserve. The rest is should-fix or nits.

**Verified against:** `origin/main` = `6e17670` (PR #27 "release identity",
merged 2026-09-17 23:38 −0400, already fetched into Paul's clone) and Paul's
working tree at `0da1c75`, where the proposal, its renderings and
`plan-release-identity.md` are untracked. `git fetch` from here failed for lack
of GitHub credentials, so the remote may have moved since that fetch. Every code
claim below was read from source at both revisions; nothing was executed. The
seven PNG renderings were inspected; the "interactive versions" §2 refers to
are not in the repo or the kit folder, so they were not.

## Must fix before build

### M1 — §5.1 was verified at `0da1c75`; `main` changed all ten cited files and already contains most of §5.2–§5.3

`git diff --stat 0da1c75 6e17670` touches every file in the §5.1 table
(+979/−72 across them). Three rows are now wrong, and the consequence is that
§5.3 asks the builder to duplicate a seam that PR #27 shipped:

| §5.1 says | On `main` `6e17670` |
|---|---|
| SearchResult has "no IMDb ID" | `imdbId?: string` and `hydrationSource?: string` are on the web and native `SearchResult`, and `resultKey` and the Add mutation already use both (`web/src/pages/AddMedia.tsx:19,139-142`). TVmaze search results carry the IMDb id straight from `/search/shows` (`tvmaze/client.go:233`). TMDB search results still don't — TMDB's search endpoint has no external ids. |
| "no preview endpoint" | `GET /metadata/search?kind=series&query=tvdb:414217` (or `tmdb:550`, `imdb:tt…`) is routed by `Library.Search → identityinput.ParseInput → ResolveExternal` to an exact-ID lookup with no title search (`library.go:216-303`). The AddMedia placeholder already advertises it. |
| "Introduce a lightweight preview capability alongside the existing hydration ports … TMDB needs the show record with external IDs, TVmaze its show lookup" | That is `ports.ExternalLookupProvider.LookupExternal`. TMDB implements it with `GetMovie` (one `/movie/{id}` request, IMDb id included) and `getSeriesRecord` (one `/tv/{id}?append_to_response=external_ids`, **no** season requests — `tmdb/client.go:519`). TVmaze implements it with one `/lookup/shows` request (`tvmaze/client.go:310`). Both check the returned ids against the requested one and return `RemoteIdentityConflict` on a mismatch — stricter than the proposal asks for. |

What is actually missing on `main` for this feature, and is therefore the real
§7.1 scope: (a) the lookup path returns `ports.SearchResult`, which has no
genres, status or runtime — `getSeriesRecord` drops them even though `tvResp`
carries `Status`, `Genres` and `EpisodeRunTime`, and TVmaze's `show` has
`Status`, `Genres`, `AverageRuntime`; (b) a response DTO that carries them;
(c) the endpoint. Rewrite §5.3 as "extend the existing exact-ID lookups to
return facts" rather than "introduce a capability alongside". Whether that is a
new `PreviewProvider` port or a widened `LookupExternal` result is the
builder's call; a new port that the two adapters implement by reusing
`getSeriesRecord`/`GetMovie`/`lookup/shows` is the smaller diff.

Decision for Paul, since both are defensible: keep the proposed
`GET /metadata/preview` (recommended — a preview DTO is not a `SearchResult`,
and it must not pass through `ResolveExternal`'s `addable()` filter or its
local-ambiguity 409, see S2), or add optional facts to `SearchResult` and let
the client call the search endpoint with `tmdb:N`. Either way §5.1 needs
re-verifying at the branch point, and the builder must branch from `main`, not
from Paul's checkout where the file lives.

### M2 — The enrichment-merge rule breaks Add's identity contract and the list's keys

§5.2: "Other enriched fields merge only for the same canonical key." Read
literally, a TMDB series result (tmdbId only) previewed via TMDB acquires
`tvdbId` and `imdbId`, and if the SearchResult object is what gets merged into,
two things go wrong:

1. `AddRequest` says "Exactly one of TMDBID/TVDBID/OLID identifies the item"
   (`library.go:437`), and the web mutation sends `tmdbId: r.tmdbId || undefined,
   tvdbId: r.tvdbId, imdbId: r.imdbId, hydrationSource: r.hydrationSource`. Today
   `hydrateAdd` routes on `hydrationSource`, so the add would still hydrate from
   TMDB — but the request violates the documented contract and any future
   validation of it turns the drawer's Add into a 400.
2. `resultKey` is `kind-olid-tmdbId-tvdbId-imdbId` (`AddMedia.tsx:19`). The
   enriched object keys differently from the list row, so `pendingKey` and
   `addedByKey` miss: the row never shows "Adding…" and never flips to
   "added · Open" after a drawer Add — exactly the state §3.3 says must stay
   intact.

Fix: preview state is a separate object keyed by the *original* `resultKey`;
enriched ids are used to build links and facts only; `add.mutate` always
receives the original `SearchResult`. State that in §5.2 and pin it with a test
(drawer Add on a TMDB series → request body has no `tvdbId`; list row shows
"added"). Same rule on native: `AddSheet` gets the `SearchResult` from the
list, never the preview.

## Should fix

**S1 — Paragraph breaks are promised and TVmaze cannot deliver them today.**
§3.2: "all text supplied by the provider, with paragraph breaks." TVmaze
summaries are HTML (`<p>…</p><p>…</p>`) and `plainText` replaces every tag with
the empty string (`tvmaze/client.go:511`), so paragraphs arrive as
"…route home.As they retrace…". Either `plainText` emits `\n\n` for `</p>` and
`<br>` (the item page benefits too; add a fixture) or §3.2 drops the promise.
TMDB overviews are plain text and occasionally contain their own newlines, so
the renderer should honour `\n\n` regardless.

**S2 — The error and ownership states need the codes `main` already has, and two rows are missing.**
`libraryErr` already emits `invalid_external_id` 400, `provider_unavailable`
503, `unsupported_hydration` 422, `identity_conflict` 409, `already_exists` 409
(`library_handlers.go:35-62`). §5.2 should name those instead of "the API's
provider-error conventions", and §4 needs two rows: *identity conflict*
(provider's record disagrees with the requested id, or two local items share it
— show the result content, no links from the conflicting ids, no Add) and
*previewable but not addable* (`unsupported_hydration`: a TVmaze show with no
TVDB id — read and link, Add disabled with the reason). Note the local-ambiguity
409 in `ResolveExternal` is a reason not to route the preview through it
unchanged: a preview should still render when the library is contradictory.

**S3 — "Open in library" and cross-provider ownership both want a local lookup in the preview.**
`SearchMetadata` decides `inLibrary` per id namespace (`library_handlers.go:817-830`):
a TMDB result for a show that was added through the chain (TVDB-keyed, no TMDB
id) reads as not owned, and no search result ever carries a library id, so §4's
"offer Open only if a real local library ID is available" can never be true
from search data alone. `ResolveExternal` already does
`db.FindMediaItemsByExternalID` first. Do the same in the preview lookup and
return `libraryItemId?` (omit it on ambiguity rather than fail). That gives the
in-library state its Open link, fixes the cross-namespace miss, and lets the
"added elsewhere → 409 already_exists" reconciliation land somewhere — the 409
body carries no item id today, so consider adding one there too.

**S4 — Native step state: keep one `Modal`, and keep `AddSheet` mounted across steps.**
`AddSheet` is a `Modal presentationStyle="pageSheet"` that owns root/profile/
monitored/searchNow/monitor in `useState` and fetches roots+profiles on mount
(`DiscoverScreen.tsx:152-219`). "Returning to the preview and then to options
retains those choices" only holds if the options step is a view switched inside
the one modal, not a second `Modal` (nested modals on iOS are unreliable anyway)
and not an unmount/remount. Android Back is `onRequestClose`; make it
step-aware (options → preview → dismiss). The proposal's "single modal with two
steps" is the right call — say the retention mechanism explicitly.

**S5 — The web trigger cannot be a `<button>` wrapping the poster/title/synopsis.**
`<button>` permits phrasing content only; the synopsis is a `<p>` and React
warns on `<p>` inside `<button>`. Make "View details" the single semantic
button (accessible name "View details for Northbound, 2024" as §3.1 says) and
extend its hit area over the card with the stretched-link pattern
(`position: relative` on the row, `::after` overlay on the button, Add above it
in stacking order). That also satisfies "Add is a sibling, never nested".

**S6 — The Back/history entry is nearly free: `AddMedia` already reads `kind` and `q` from the route search params.**
Push `preview=<kind>:<provider>:<id>` as a search param on open (push, not
replace) and clear it on close; browser Back then closes the drawer without a
router subscription, and the URL restores the same search on reload. That is
not a "shareable preview URL" commitment — it is just how Back works.

**S7 — §7.4 lists the gate incompletely and cites verification nobody can inspect.**
The Makefile has `test-mobile` (= `npm run check`) which the list omits; write
the list as the Makefile targets (`lint`, `test`, `test-web`, `test-mobile`,
`test-e2e`) plus `npm --prefix web run typecheck`. And the "interactive
versions" that §2 says exercise selection, close, add and back are not in
`docs/renderings/media-preview/` or anywhere in the checkout or the kit — commit
them or delete the paragraph; a builder can't consult what isn't there.

**S8 — Caching claim.** "Reuse existing provider response caches" is true for
TMDB only (`tmdb/client.go:40-93`, TTL cache on every GET); TVmaze has none.
Fine, but say which.

## Nits

- Web series rendering: the page behind the drawer shows root folder
  `/media/movies` while the footer says `/media/tv`.
- Mobile preview has both "‹ Results" and "Close" doing the same thing; keep
  Close (the options step legitimately has "‹ Details" + Close). Only the
  preview-originated options variant is rendered; the direct-Add variant reads
  "‹ Results" per §3.1 — say so under the image.
- Status strings are raw provider values: TMDB series say "Returning Series",
  TVmaze says "Running"; both providers say "Ended"/"Canceled". Decide whether
  to map them or show them raw, and label runtime "per episode" for series
  (TMDB `episode_run_time` is frequently empty → omit, as §3.2 says).
- `source` in the DTO: `main` already distinguishes `source` (who found the
  mapping) from `hydrationSource` (who Add will persist). The "Metadata from X"
  label is a third thing (who served the preview); keep the name distinct so
  nobody feeds it into Add.
- Paul's working-tree `README.md` adds rows for the two sibling plans but not
  this one.
- §5.2 says series lookup key is "TMDB when the selected result has one,
  otherwise TVDB". Correct on `main`: TMDB series results carry only `tmdbId`,
  chain results carry `tvdbId` (+ `imdbId`). No result carries only an IMDb id,
  so IMDb needn't be a lookup key.

## §8 answers

Drawer, not dialog: ~460 px minus padding gives 55–65 characters a line at the
app's 15 px body size, which is the measure long text wants, and the results
stay visible for comparison. "View details →" as small link text under the
synopsis is not noisy and is the right visible affordance; with S5 the whole
card is the hit area. The web one-step Add is clear because the footer
summarises the options that will be used; native's "Continue to add / Choose
folder, quality and monitoring next" is equally clear. Links above the synopsis
are right — they are identity evidence and belong next to year and kind. The
states the review asks about (long synopsis, no artwork, large text, in library)
are specified in §4 but none is rendered; the unshipped interactive mockups
were supposed to cover them (S7).

## Claims checked and found correct

- Web results clamp to two lines (`.clamp`, `styles.css:761`), Add is the only
  action, and the page keeps per-visit `added` state and an added-items strip.
- `SearchCard` truncates the overview to 2/3/4 lines by item size and has
  `onAdd` but no preview callback; the card is a `View`, not a `Pressable`.
- `DiscoverScreen`: search and browse share `SearchCard`; Add opens
  `AddSheet`; success calls `onAdded(id)` and `App.tsx:100` opens the new item.
- Search handler sends `res.Overview` untruncated and computes ownership
  server-side.
- `ports.SeriesProvider` is keyed on TVDB; `TMDB.GetSeries` walks every
  season (`/tv/{id}/season/{n}` per season, `tmdb/client.go:411`);
  `TVmaze.GetSeriesByTVDB` fetches `/shows/{id}/episodes`.
- `MediaDetail.tsx:443-452` already builds IMDb/TMDB/TVDB/Open Library links
  from known ids and opens them with `target="_blank" rel="noreferrer"`.
- Web defaults `searchNow` to true; native `AddSheet` defaults it to false.
- TVmaze `plainText` guarantees the overview is never markup (HTML stripped
  and unescaped), so "do not render provider HTML" is already the server's
  contract.
- `BookProvider.GetBook(ctx, olid)` exists for the book preview path.
- All twenty relative links in the proposal resolve in the working tree.
