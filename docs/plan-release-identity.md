# Release identity — aliases, country qualifiers, and direct ID search

**Status:** implementation in progress on `codex/release-identity`; no behavior is merged ·
**Written:** 2026-09-17 · **Baseline:** `0da1c75` ·
**Adversarial revision:** 2026-09-17

Companion to [Architecture](architecture.md) and
[ADR 0011](adr/0011-series-metadata-provider.md) (series providers and episode
numbering). This document specifies how to identify a work despite differing
release names, find it by an external ID, and explain the result. Read
§3–§7 before implementing the milestones in §11. Source references were
checked against the baseline above; re-verify signatures at build time.

Standing instruction: keep the selected episode provider and its numbering
unchanged. If identity resolution requires merging different series or
translating episode numbers, report that conflict instead of inventing a
mapping. The historical “neither is built yet” status in ADR 0011 is stale:
the TVmaze adapter and parts of the provider chain exist in this baseline.

## 1. The failure and the required result

The reported release is:

```text
Have.I.Got.News.for.You.US.S05E01.1080p.WEB.h264-EDITH
```

The selected library episode is `Have I Got News for You S05E01`. The
[matcher](../internal/domain/matcher/matcher.go) compares normalized titles
for equality and retains `US`, so it rejects this release before checking
episode coverage. The UI reports only “does not match”, concealing the
actual difference.

Once metadata establishes that the library entry is the US series, the
qualified and unqualified names must resolve to that same series. If the
library entry is the UK series, the release must remain rejected. The
screenshot alone does not establish which provider record was added.

The supplied external review reports these TVmaze identities. Capture them
as provider fixtures at implementation time; this revision could inspect the
repository but could not independently refetch TVmaze through the web tool.

| Version | TVmaze | TVDB | IMDb | Canonical title |
|---|---|---|---|---|
| UK | 1600 | 74281 | tt0098820 | Have I Got News for You |
| US | 78874 | 453187 | tt33096993 | Have I Got News for You |

The review also observed US aliases `Have I Got News for You US` and
`Have I Got News For You U. S.`. The US alias's market was GB, a concrete
example of why alias market is not origin. The screenshot regression must
contain **both** works: qualified US releases select the US item, and
unqualified UK releases remain matchable under §6.2. The `414217` /
`tt16867040` examples elsewhere are the separate Cunk on Earth lookup fixture.

The implementation includes all three ID workflows:

| Workflow | Required behavior |
|---|---|
| Find media to add | Enter `tvdb:414217`, `imdb:tt16867040`, or a bare IMDb title ID; resolve the exact work without a text search. |
| Find downloads | Query indexers using supported TVDB/IMDb IDs, with bounded title and alias fallbacks. Include existing TMDB identities where supported. |
| Identify a returned release | Use the release's reported IDs when its title does not match; still validate media kind, episode coverage, and movie year. |
| Handle country variants | Accept known aliases and evidence-backed optional qualifiers, including bracketed qualifiers inside a title. |
| Explain a decision | Show the matched alias/ID or the exact title, country, ID, year, or episode conflict. |

## 2. Existing contracts and missing connections

These are existing interfaces, copied in abbreviated form where indicated.
They are not the proposed interfaces in §4.

```go
// internal/domain/media.go
type ExternalIDs struct {
    TMDB int64
    IMDB string
    TVDB int64
    ISBN13 string
    OLID string
    ASIN string
}

// internal/ports/metadata.go
type AltTitleProvider interface {
    AlternativeTitles(context.Context, domain.MediaKind, int64) ([]string, error)
}
type SeriesProvider interface {
    Name() string
    SearchSeries(context.Context, string) ([]SearchResult, error)
    GetSeriesByTVDB(context.Context, int64) (domain.MediaItem, error)
}

// internal/domain/wantable.go; BookType omitted here
type SearchQuery struct {
    Q string
    Season int  // existing 0 = unset convention
    Episode int // existing 0 = unset convention
    Kind MediaKind
}
func PlanSearch(w Wantable) []SearchQuery

// internal/domain/matcher/matcher.go
func TitleMatches(parsed parser.Parsed, title string, year int) bool
func Match(p parser.Parsed, candidates []domain.Wantable) []Result
```

| Component | Baseline behavior | Required change |
|---|---|---|
| [MediaItem](../internal/domain/media.go) | Stores TMDB/TVDB/IMDb IDs; no persisted aliases or origin countries. | Add identity metadata with provenance. |
| [Wantables and planner](../internal/domain/wantable.go) | Video targets carry one title; queries are title strings. | Carry identity and structured query alternatives. |
| [TMDB adapter](../internal/adapters/tmdb/client.go) | Fetches alternative titles for adoption; has `FindSeriesByTVDB`. | Generalize exact external lookup and identity enrichment. |
| [TVmaze adapter](../internal/adapters/tvmaze/client.go) | Searches names and hydrates by TVDB ID. | Add IMDb lookup and AKA enrichment. |
| [Library service](../internal/app/library/library.go) | Text search merges sources by normalized title/year; add checks TMDB/TVDB identities. | Resolve ID syntax; deduplicate only with verified identity; include IMDb. |
| [Adoption](../internal/app/library/adopt.go) | Uses temporary alternative titles when proposing folder matches. | Reuse enrichment without weakening adoption confidence rules. |
| [Indexer port](../internal/ports/indexer.go) | Releases discard external IDs; indexers expose Search/RSS/Test only. | Preserve result IDs and expose parsed capabilities. |
| [Torznab adapter](../internal/adapters/torznab/torznab.go) | Shared Newznab/Torznab adapter; TV mode depends on `Season > 0`; movies use generic search; caps checked only as text. | Select modes from kind/capabilities; distinguish season zero from unset. |
| [Interactive search](../internal/app/acquisition/acquisition.go) | Fans out flat queries; matches one target; generic rejection. | Execute query tiers and return structured match evidence. |
| [Automation](../internal/app/acquisition/automation.go) | RSS and `searchAndGrabBest` have separate matching paths. | Use the same identity evaluator as interactive search. |
| [Wanted cache](../internal/app/acquisition/wanted.go) | Contains monitored, missing/upgradable targets. | Carry refreshed identity; maintain a separate all-library ambiguity index. |
| [Sonarr compatibility](../internal/compat/sonarr.go) | `tvdb:` lookup exists, injected directly from TMDB in main. | Use shared resolver for TVDB/IMDb and preserve response contracts. |
| [Radarr compatibility](../internal/compat/radarr.go) | `tmdb:` lookup plus text search. | Add IMDb via the shared resolver. |
| [Native API](../internal/api/library_handlers.go) | `/metadata/search` treats query as text; results lack IMDb. | Preserve endpoint, add typed query behavior and ID fields. |

## 3. Decisions and boundaries

1. **Identity belongs to a work, not a spelling.** Canonical title, aliases,
   country qualifiers, and external IDs are different evidence for one
   item. Keep aliases on the item so every episode and quality copy agrees.
2. **Queries and results are separate evidence.** Sending `tvdbid=X` does
   not prove every returned release belongs to X. Only IDs actually present
   in the returned item may establish an ID match.
3. **Country qualifiers need context.** Parse recognizable markers, retain
   the original title, and remove them only during an evidence-backed
   comparison. Do not change `NormalizeTitle` to delete country words.
4. **Explicit contradictions block automatic matching.** A matching title
   cannot override a conflicting returned ID; a matching ID cannot override
   a confirmed regional conflict or a wrong episode. This is deliberately
   stricter than some Sonarr paths; §12 identifies the precedent precisely.
5. **Unknown is not a conflict.** A missing ID, missing country, or absent
   year is incomplete evidence. Never fill missing release IDs from the
   query or infer a country's identity from the user's locale.
6. **Metadata lookup does not change episode ordering.** Aliases cannot
   resolve differences in provider season grouping. Preserve the item's
   source and existing season/episode keys, as ADR 0011 requires.
7. **Matching is pure.** Fetch, cache, and persist identity outside the
   matcher. RSS evaluation must not make metadata requests per release.

**Non-goals:** no fuzzy title-distance acceptance, no deletion of arbitrary
country words, no new paid TVDB adapter or hosted metadata proxy, no XEM
episode-number translation, no change to book matching, no file renaming,
and no automatic merging of existing library rows. Preserve explicit manual
grab behavior; this work improves automatic decisions and explanations.

## 4. Persisted identity and enrichment

### 4.1 Proposed domain and provider types

Add the following contracts, with JSON tags in the actual implementation.
Names below are proposed and must be applied consistently across callers.

```go
// domain: independent of ports and storage
type TitleAlias struct {
    Title string
    Source string         // tmdb | tvmaze | manual
    SourceID string       // source's native work ID
    Language string       // optional provider language code
    MarketCountry string  // optional release/translation market; NOT origin
    Scope string          // work | season | unsupported_numbering
    Role string           // original | alternate | historical | manual
    Searchable bool       // matching eligible even when false
}
type CountryEvidence struct {
    Code string           // uppercase ISO code; GB is canonical for UK
    Source string         // tmdb | tvmaze | manual
    Basis string          // origin | network | title_qualifier | manual
}
type MediaIdentity struct {
    IDs ExternalIDs
    Title string
    Year int
    Aliases []TitleAlias
    Countries []CountryEvidence
}
type ExternalRef struct {
    Provider string       // tvdb | imdb | tmdb
    Value string          // validated decimal or canonical tt-prefixed ID
}

// ports: optional capabilities, implemented by applicable providers
type ExternalLookupProvider interface {
    LookupExternal(context.Context, domain.MediaKind, domain.ExternalRef) ([]SearchResult, error)
}
type IdentityMetadata struct {
    Aliases []domain.TitleAlias
    Countries []domain.CountryEvidence
}
type IdentityMetadataProvider interface {
    IdentityMetadata(context.Context, domain.MediaKind, domain.ExternalIDs) (IdentityMetadata, error)
}

// ports: adapters wrap errors; callers classify with errors.As/Is
type RemoteError struct {
    Category string       // not_found | auth | rate_limit | unsupported_query |
                          // unsupported_hydration | transport | invalid_response |
                          // identity_conflict
    HTTPStatus int
    ProtocolCode string   // Newznab XML code, even under HTTP 200
    RetryAt time.Time     // zero if absent; honor Retry-After dates or seconds
    ExpectedIDs domain.ExternalIDs // populated for identity_conflict
    ActualIDs domain.ExternalIDs   // raw provider values, before requested IDs
    Cause error           // implement Error and Unwrap; redact URLs/credentials
}
```

Adapters report contradictions as `ports.RemoteError` with category
`identity_conflict`, expected IDs, and raw received IDs. The application
maps that to its typed `IdentityConflictError`, adding affected local item
IDs where known; adapters must never import an application error type.
These conflicts are not retryable transport errors. Provider and indexer
adapters must classify actual HTTP
status and protocol error fields, never formatted message text. Include
HTTP-200 XML authentication/unsupported-query responses in adapter fixtures.
Unconfigured credentials use the existing `ErrProviderNotConfigured`.

Keep the stored `MediaItem.Title`, `Year`, and `IDs` fields as the canonical
values. Add alias/country collections; derive `MediaIdentity` from them
instead of storing two independently editable copies. Add `Identity` to
Movie/Episode/Season wantables, with one construction helper used by
`targetCopy` and `wantedEpisodes`. Retain `Title`/`Year` temporarily for
existing callers, populated by that helper. Remove duplication only after
all call sites migrate; books keep their existing contract.

External IDs on `SearchResult` must include IMDb. Exact lookup results must
carry at least one usable add key: TMDB ID, or TVDB ID for series. IMDb is a
lookup key; resolve it to one of these existing hydration routes. A TVmaze
result with neither a usable TMDB ID nor a TVDB ID is not addable in this
milestone; return the §5.2 unsupported-hydration outcome rather than a broken Add
button. Supporting TVmaze-native identity would require a separate change
to ADR 0011 and the add/refresh routes.

### 4.2 Storage contract

Add a new forward migration using the next unclaimed number across **all
branches**, following the [migration rules](../internal/infra/sqlite/migrations/README.md).
The baseline directory ends at 0029; this document does not reserve 0030.
The proposed schema is:

```sql
CREATE TABLE media_aliases (
    id INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    normalized_title TEXT NOT NULL,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL DEFAULT '',
    market_country TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT 'work'
        CHECK (scope IN ('work', 'season', 'unsupported_numbering')),
    role TEXT NOT NULL DEFAULT 'alternate'
        CHECK (role IN ('original', 'alternate', 'historical', 'manual')),
    searchable INTEGER NOT NULL DEFAULT 0 CHECK (searchable IN (0, 1)),
    UNIQUE (media_item_id, normalized_title, source, source_id, scope,
            role, language, market_country)
) STRICT;
CREATE INDEX idx_media_aliases_normalized ON media_aliases(normalized_title);

CREATE TABLE media_identity_revision (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO media_identity_revision(id, revision) VALUES (1, 0);

CREATE TABLE media_identity_sources (
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    countries TEXT NOT NULL DEFAULT '[]', -- JSON CountryEvidence array
    fetched_at INTEGER NOT NULL DEFAULT 0, -- last successful fetch, Unix ms
    attempted_at INTEGER NOT NULL DEFAULT 0,
    retry_after INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (media_item_id, source)
) STRICT;

CREATE INDEX idx_media_items_kind_imdb
    ON media_items(kind, imdb_id) WHERE imdb_id != '';
CREATE INDEX idx_media_items_kind_tvdb
    ON media_items(kind, tvdb_id) WHERE tvdb_id != 0;

ALTER TABLE downloads ADD COLUMN match_evidence TEXT NOT NULL DEFAULT '{}';
```

Check whether equivalent indexes have landed before adding them. IMDb and
TVDB lookup indexes are initially non-unique: an existing duplicate must
produce an inspectable ambiguity, not prevent startup. New adds perform
identity checks and insertion inside one write transaction. Same-kind
requests whose IDs resolve to different existing rows return a conflict;
they never silently merge rows. Retain the existing unique TMDB index.

Add sqlc queries and wrappers in
[SQLite library storage](../internal/infra/sqlite/library.go) for loading
identity in batches, replacing one provider's alias set, preserving manual
aliases, and finding **all** rows by an external ID. Updating aliases,
country evidence, and successful-fetch time is one transaction. Extend
schema verification and round-trip tests. Normalized alias keys are derived
data; a future normalization change must rebuild them explicitly.

For every provider alias and canonical title, reject empty normalized keys
from title indexes and title equality comparisons. The baseline ASCII
normalizer can turn distinct non-Latin names into `""`; two empty keys must
never establish identity. Retain such original strings for display and ID
matching, but mark title matching unavailable. Unicode normalization is a
separate project; include fixtures for two unrelated all-CJK names here.

### 4.3 Enrichment and refresh behavior

Fetch on add, metadata refresh, and a deduplicated background backfill.
Use a proposed `library.identity.refresh` job with payload
`{"itemId":123}` and dedupe key `library.identity.refresh:123`, registered
beside the existing [library jobs](../internal/app/library/jobs.go).

Defaults are implementation constants: metadata cache age **7 days**, failed
fetch retry delay **1 hour**, at most **2 concurrent enrichment jobs** per
process. Existing provider rate limiters and HTTP deadlines still apply.
An explicit Refresh requests enrichment even when fresh, but respects an
active upstream `Retry-After`. These values bound backfill traffic and can
be adjusted later from measurements; do not add settings UI in this phase.

On first upgrade, enqueue items in batches of **100** using keyset paging;
resume by selecting missing/stale source state rather than a volatile
cursor. Do not perform network access in the migration or block startup.
With no queue configured, add/explicit refresh may enrich inline within the
request deadline; defer bulk backfill until normal refresh or a queue is
available. Failures retain last successful data and do not fail an
otherwise valid add. A successful empty alias list clears that provider's
old aliases. Manual aliases survive all provider refreshes.

Treat all endpoints contributing to one provider snapshot as a unit: an
AKA/alternative-title request that fails is not a successful empty result.
Publish replacement data only after every required endpoint succeeds and
the provider record's IDs still agree. Keep `historical` aliases separately
from the replaceable `original`/`alternate` set, so the next refresh does
not erase an earlier canonical-name change. Preserve distinct role/market
records in storage while deduplicating normalized names in matcher indexes.
Manual aliases use `source=manual`, `role=manual`, `source_id=''`, and empty
language/market fields; their duplicate rule remains deterministic.

Implement one sweep at startup and every **1 hour** thereafter. The sweep
selects only video items with usable provider identities, stale/missing
successful data, and `retry_after <= now`; skip manual-only items. Failed
or unconfigured providers set a retry time and cannot monopolize the first
batch. Enqueue at most **100 new jobs per sweep**, priority **80** so scans
and interactive work win. Coalesced jobs do not consume that insertion
budget; advance keyset paging past them. Workers record failures and finish
that attempt; the sweep owns the next retry, avoiding an independent queue
retry loop that ignores `retry_after`. Explicit Refresh can enqueue a
higher-priority attempt while still respecting upstream retry timing.

After persisting a provider failure and its `retry_after`, the identity job
handler **returns nil** so the generic queue completes that attempt. Return
an error only when failure-state persistence itself fails or execution is
cancelled before a durable outcome; the ordinary queue then handles that
infrastructure failure. Invalid job payloads remain permanent queue errors.
Test the real handler/queue boundary, not just the refresh helper.

Sources and rules:

- TMDB: original title/name plus movie/TV alternative-title endpoints.
  Retain origin-country data separately from alternative-title market
  codes. Do not label an imported title's release country as the work's
  origin.
- TVmaze: resolve the same show by TVDB/IMDb, then fetch `/shows/{id}/akas`.
  Retain network country as weaker evidence, never as a universal origin
  fact. AKA country describes a naming market, not a regional remake.
- Enrich across providers only through verified shared IDs. An identical
  title and year cannot establish that two records share aliases.
- Accept work-level aliases only. The Cunk umbrella/season example in
  ADR 0011 remains a regression fixture; a season name must not turn into a
  series-wide alias. Unclear cross-provider grouping stays unresolved.
- Preserve the previous canonical title as a provider-sourced alias when
  the same verified provider record is renamed.
- Provider aliases default to matching-only. Mark the original title and
  manual search aliases searchable; select additional aliases for query
  expansion only by the bounded policy in §7. Never issue one query per
  translated title.

**Scope classification is explicit, not inferred from a name.** Normal
work-level alias endpoints supply provider-asserted `work` aliases by
default. Override that scope when the response carries explicit narrower
scope or a fixture-backed grouping-exception registry identifies the tuple
`provider + native work ID + normalized alias`. Add the registry as domain
data alongside matcher fixtures, not a scattered title special case. Seed
the documented TMDB `tv/79063` / `Cunk on Earth` programme alias as
`unsupported_numbering`; add other aliases only with verified fixtures.

Retain excluded aliases for display and diagnostics, but exclude them from
positive title/country matching and query expansion. A release matching
an excluded alias for that umbrella item yields `numbering_scope_conflict`
even if an indexer supplies the umbrella item's ID. A manual alias cannot
override this conflict; the existing explicit manual-grab path remains an
operator decision. This catches known or explicitly described grouping
differences, not every undocumented upstream metadata error. Do not claim
that a bare alternate-title list proves episode-order equivalence.

Invalidate both the wanted cache and the all-library identity index after
any identity edit, refresh, add, or delete. Increment the singleton identity
revision in the same transaction as each effective identity change. Publish
a proposed `library.identity.changed` event for immediate local invalidation;
the existing in-process event bus alone does not notify another node.

Read the durable revision at the start of each search/RSS operation. Rebuild
the immutable index and its revision from one consistent read transaction
when the revision differs. Before an automatic grab, recheck the revision
and re-evaluate locally if it changed; this does not issue new indexer
queries. Add a two-service-instance test proving one instance sees another's
alias/ID changes. Cache refresh failures must not silently authorize a grab
using a known-stale identity snapshot.

Include provider snapshot status/timestamps in the immutable index and
invalidate its revision when a failed or successful enrichment changes
original/remake convention eligibility (§6.2). Recompute age-based freshness
at each operation's supplied clock time even when the revision is unchanged;
a seven-day-old snapshot can become stale without a database write.

## 5. Direct ID lookup in Add Media and compatibility APIs

### 5.1 Input grammar and errors

Implement one pure parser in a new domain identity package. Native API,
web/mobile behavior, and compatibility handlers share this interpretation.

| Input after trimming | Meaning |
|---|---|
| `tvdb:414217` or `tvdbid:414217` | Exact TVDB series lookup. |
| `imdb:tt16867040` or `imdbid:tt16867040` | Exact IMDb title lookup within the selected movie/series kind. |
| `tt16867040` | Exact IMDb title lookup. |
| `imdb:16867040` | Explicit numeric IMDb form; normalize to `tt16867040`. |
| `tmdb:550` | Exact TMDB lookup in the selected kind; preserves compat functionality. |
| `1917` | Ordinary title text; unqualified numbers are not IDs. |
| `tvdb:abc`, `imdb:`, `tvdb:-1` | Recognized prefix with invalid value: validation error; no text fallback. |

Prefixes and `tt` are case-insensitive. Trim whitespace around the prefix
and value, reject whitespace inside the value, zero, negative numbers,
non-digits, and integer overflow. Numeric TMDB/TVDB IDs must fit positive
int64. IMDb title IDs accept 7–12 digits, preserving leading zeros;
explicit numeric IMDb values of 1–6 digits are left-padded to 7. Values over
12 digits are rejected until the contract is deliberately revised. A bare
malformed `tt`-like word remains text; prefixed malformed IDs are errors.
Reject TVDB lookup with movie/book kind. ID syntax applies to video; book
search remains unchanged except that an explicit video-ID request produces
a clear kind error. Do not fetch pasted arbitrary URLs in this milestone.

Keep `GET /metadata/search?kind=series&query=tvdb%3A414217`. Exact lookup
returns the normal candidate array with `imdbId`, `tvdbId`, `tmdbId`, source,
and in-library status. A verified not-found is `200 []`; invalid input is
`400`; unavailable credentials/upstream failure is `503`; contradictory
identity is `409`; resolved but unsupported hydration is `422` after the
fallback rules in §5.2. Extend the OpenAPI error responses and `libraryErr`
mapping. The client must distinguish “not found” from “provider unavailable”.

Add a stable `code` to the native API Error schema while retaining its
existing message field. Both `ErrAlreadyExists` and identity contradictions
use HTTP 409, so emit `already_exists` versus `identity_conflict` explicitly.
Also define `invalid_external_id`, `provider_unavailable`, and
`unsupported_hydration` for the corresponding 400/503/422 cases. Web/mobile
branch on these codes, never status alone or message text. Keep compatibility
personality error shapes intact unless their own contract supports a code.

### 5.2 Resolution order and identity checks

Add `library.ResolveExternal(ctx, kind, ref)` and make `Search` call it
before the text-search branch. First inspect existing library IDs so an
already-added item remains discoverable offline. If multiple same-kind rows
claim the ID, return an identity conflict listing the affected item IDs.

For a missing local item:

1. TVDB: consult configured series providers implementing exact lookup,
   in chain order; TVmaze is available without a key. Fall back to TMDB
   `/find/{id}?external_source=tvdb_id` and its **TV results** only.
2. IMDb series: TVmaze `/lookup/shows?imdb=...`, then TMDB `/find` with
   `external_source=imdb_id`; consume only TV results. A future configured
   TVDB adapter may participate through the same port.
3. IMDb movie: TMDB `/find` movie results only. Episode/person results must
   not be promoted to movies or series.
4. TMDB: hydrate directly using the selected kind. Numeric IDs in different
   kind namespaces are unrelated.

An exact endpoint establishes a mapping, but hydration must not contradict
the requested ID. Validate the provider's raw returned external IDs before
assigning any requested ID to the result. In particular,
`tvmaze.GetSeriesByTVDB` currently writes the requested value into `IDs.TVDB`;
checking only the resulting object would conceal a contradictory
`externals.thetvdb`. A conflict stops resolution; do not paper over it using a
lower provider. A genuine not-found proceeds to the next provider. For a
new series resolved by TVDB/IMDb, a timeout, 429, or authentication failure
at a configured higher-priority hydration provider returns `503`; do not
offer a lower-provider Add whose episode ordering would be permanently
chosen because of a temporary outage. Optional providers with no configured
credentials are skipped before resolution. An explicit `tmdb:` lookup is
an intentional TMDB selection and does not depend on TVmaze availability.
Existing local-library ID hits remain available offline. Carry failure
context in logs; never report operational failure as authoritative not-found.

`unsupported_hydration` is also a recoverable provider outcome: if TVmaze
finds the IMDb show but supplies no usable add key, still try TMDB's exact
lookup. Return a native `422` unsupported-hydration error only when all
providers were exhausted without an addable result and without a remaining
operational failure; operational failure takes precedence as `503`.
Multiple same-kind records for one supposedly exact lookup are a `409`
identity conflict, never “take the first”.

This changes **new Sonarr/Jellyseerr TVDB-based adds**: the baseline resolves
through TMDB and adds by TMDB ID; the shared resolver will choose the first
usable configured series provider (TVmaze in the keyless setup), then TMDB
on a definitive miss or unsupported hydration. State this in release notes
and pin the selected provider and episode list in compatibility tests.
Existing items retain their recorded provider and numbering.

Resolve to an existing supported add key and hydrate through its established
provider route. Recheck all IDs during Add in a write transaction: searching
and adding can be separated by minutes, and another client may add meanwhile.
Do not change an existing item's episode provider because lookup found
another ID for it.

**Required routing change:** the baseline `Add` and `RefreshItem` choose
TMDB whenever a TMDB ID exists. Add a `hydrationSource` discriminator to
video lookup results and add requests, validate it against a configured
provider, and persist it in the item's existing `Source`. Keep display
`source` separate when describing which provider discovered an external
mapping. Refresh routes by the persisted hydration source, never by ID
presence; a TVmaze series enriched with a TMDB ID must still get its episodes
from TVmaze. A temporarily unavailable source fails/defer-refresh without
switching sources. Source migration is outside this plan.

Legacy native add requests without `hydrationSource` retain their existing
routing. Sonarr's server-side resolver passes the newly selected source
explicitly, including for its existing client payloads.
For stored rows with blank source, derive the baseline route once in the
migration, following [migration 0017](../internal/infra/sqlite/migrations/0017_manual_entries.sql)
(TMDB when a TMDB ID exists; otherwise the established TVmaze
route for TVDB-only rows); preserve nonblank source/manual values. Do not
write new cross-provider IDs until that source is fixed. Native and compat
lookup-to-add tests must carry the chosen route through this boundary.

The migration is not enough: TMDB hydration currently leaves `Source` blank.
Stamp the selected source on **every** insertion path, including legacy
native adds, Sonarr/Radarr compatibility, import lists, and adoption. Make
TMDB adapter hydration return `Source="tmdb"` and enforce a nonblank selected
video source in the shared add boundary; preserve explicit manual records.
Test all insertion paths so tomorrow's adds cannot recreate blank routing.

Add a dedicated transactional `UpdateMediaIdentity` storage operation:
`UpdateMediaItemMetadata` does not currently write `tmdb_id`. It must check
same-kind identity collisions, preserve `Source`, update verified
TMDB/TVDB/IMDb IDs, and increment the identity revision atomically with
associated identity changes. Missing upstream IDs preserve stored values;
contradictory nonzero values abort. Test a TVmaze item's persisted TMDB
enrichment across restart and its next TVmaze-routed refresh.

Hydrate and validate upstream data **before** opening the write transaction.
Inside one short immediate SQLite transaction, collect all same-kind ID
matches, reject disagreements, and insert the item/identity/episodes if
absent. Do not hold a database write lock across provider requests. An
existing row matching one ID but contradicting another is a conflict, not
an already-added success. Return the existing-item outcome only when every
supplied known ID is compatible; preserve the endpoint's existing HTTP
already-added convention.

Replace `appendSeriesChain`'s title/year identity merge with verified shared
ID deduplication. Collapse records only when at least one shared nonzero ID
agrees and no known same-namespace ID conflicts. Keep same-name results with
insufficient identity as separate candidates, labeled by year/source/IDs.
Do not copy a TVDB ID onto a TMDB result just because its title/year agrees.
Make `inLibrary` check every verified ID, not whichever one is first present.

### 5.3 User-facing surfaces

- [Add Media](../web/src/pages/AddMedia.tsx): placeholder “Title, TVDB ID,
  or IMDb ID”; helper example `tvdb:414217` and `imdb:tt16867040` for series.
  Movie mode shows IMDb/TMDB examples. Keep kind selection explicit.
- [Global search](../web/src/globalsearch.tsx): recognize the same syntax
  for exact existing-library ID hits, and forward it unchanged to Add Media.
  Known `tvdb:` input selects series. Global search has no video-kind
  selector: a nonlocal IMDb ID offers explicit “Find movie” and “Find series”
  actions. Add Media retains its own selected kind; do not silently assume
  the sidebar already has one.
- Mobile Add/search and [API client](../mobile/src/api.ts): same backend
  query syntax, ID labels, validation errors, and already-added behavior.
  Update [mobile types](../mobile/src/types.ts) as well as generated web/API
  contracts; an optional new response field must not break older clients.
- Route Sonarr `tvdb:`/`imdb:` and Radarr `tmdb:`/`imdb:` lookups through the
  same resolver. Replace the TMDB-specific `ResolveTVDB` wiring in
  [main](../cmd/monarr/main.go). Keep personality DTOs, authentication, and
  established not-found/error conventions pinned by compatibility tests;
  do not blindly copy native HTTP semantics into those adapters.

## 6. One matching policy for interactive search and automation

### 6.1 Country extraction without damaging titles

Build a separate `TitleVariants` helper. It receives the title segment with
its brackets intact. Each variant carries the original title, base title,
candidate country, and the rule that produced it. It proposes an
interpretation; it does not decide identity.

The baseline parser's `cleanTitle` already removes brackets, and its leading
bracket rule treats `[US]` as a release group. Add `Parsed.RawTitle` (the
title segment before cleanup) and retain a separate leading bracket token
when parsing could confuse a country with a group. The identity evaluator
can then consider a qualified title without changing existing anime group
or absolute-episode behavior. Do not recover this information from the
already-cleaned `Parsed.Title`, or country markers inside brackets will be
lost before matching starts. Add fixtures for `[US] Show S01E01` and
`[SubsPlease] Show - 15 [1080p]` together.

Initial recognized codes: `US`/`USA` → `US`, `UK`/`GB` → `GB`, and `AU`,
`CA`, `NZ`. Match them case-insensitively at token boundaries. Recognize
`U.S.`, `U. S.`, and the parsed token pair `U S` as `US` only in the same
qualifier positions; punctuation cleanup must not lose this equivalence.
Do not remove the letters `u` and `s` independently from ordinary titles.
Recognize full country names in brackets using a tested explicit map. Additional
codes can be added with fixtures; do not interpret every two-letter word
as a country. Countryless titles remain literal, including `Us`,
`This Is Us`, `The Last of Us`, and `Made in America`.

| Form | Candidate interpretation |
|---|---|
| `Have I Got News for You US` | Trailing unbracketed qualifier, conditional on target evidence. |
| `Have I Got News for You (US)` | Explicit bracketed qualifier. |
| `Have I Got News For You U. S.` | Same conditional edge qualifier as `US`. |
| `Have I Got News [US] for You` | Explicit bracketed qualifier within the title; preserve other words in order. |
| `US Have I Got News for You` | Leading unbracketed qualifier, conditional on target evidence. |
| `Have I Got US News for You` | Literal title unless it is an explicit known alias. |
| `The Last of Us` | Literal match wins; do not globally remove the final word. |
| `Show (US) (UK)` | Conflicting qualifiers; no automatic country match. |

Always attempt full literal canonical/alias matching before treating an
ambiguous unbracketed word as a qualifier. Explicit bracketed regional
conflicts still block a literal alias when verified metadata contradicts it.
Unbracketed qualifiers are confirmed only by a known qualified alias or
authoritative regional evidence and an equal base title. Confirmation of
the qualifier does not imply agreement with that region.

Accept a qualifier-elided comparison when the base titles agree and the
qualifier is supported by a known alias, explicit title qualifier, manual
evidence, or a single unambiguous origin country. Network country alone is
insufficient; coproduction country lists are insufficient to choose a remake.
Never use AKA market country as origin. With unknown country and no alias,
the ID fallback can identify the work, but the country-only path cannot.

Country interpretation and agreement are separate checks. Once removing an
edge token produces a known base title with authoritative regional evidence,
compare the token to that evidence even when they disagree. For example,
`Show US` against the confirmed GB work `Show` is `country_conflict`,
including when an indexer supplies that GB work's matching ID. Requiring a
compatible country before recognizing the token would let this conflict
bypass the guard. A full literal title such as `The Last of Us` still takes
precedence over speculative token removal.

**Year qualifiers use the same variant boundary.** For series, extract a
trailing `(YYYY)`/`[YYYY]` from canonical titles and aliases as a year
qualifier, preserving their display text. Release parsing must find the
series-title year in the segment **before** the episode/season marker and
exclude it from `Parsed.Title`, regardless of later episode-title years.
Both `Show.2024.S01E01` and `Show.2024.S01E01.Summer.of.1999` must produce
title `Show` and `SeriesTitleYear=2024`. `Doctor Who (2005)` must therefore
compare as title `Doctor Who` plus year 2005 against
`Doctor.Who.2005.S01E01`; apply the year filters in §6.2. A full literal
numeric title and movie parsing keep their existing rules. For series,
trailing episode-title years never populate the series identity year.
For structurally recognized series releases, bypass the parser's global
last-year scan; if `Parsed.Year` remains populated for backward compatibility,
set it from `SeriesTitleYear` only. Preserve `RawTitle` before removing that
qualifier. Never remove the only token from a numeric series title.

Removing only a recognized metadata year qualifier preserves its evidence
tier (canonical or alias); removing a country qualifier uses the lower
qualifier-elided tier. A release's explicit series year must agree with any
known metadata title qualifier and premiere year. If those stored years
contradict each other, report a metadata conflict instead of guessing which
to trust. Update the parser golden corpus for both shapes above.

### 6.2 Identity, ambiguity, and coverage are separate steps

Proposed matcher entry points:

```go
type ReleaseEvidence struct {
    Parsed parser.Parsed
    IDs domain.ExternalIDs // returned indexer attributes only
    IDIssues []domain.IdentityIssue
}
// Define this shared evidence type in domain, usable by ports and matcher.
type IdentityIssue struct {
    Code string            // malformed_id | conflicting_ids
    Provider string        // tvdb | imdb | tmdb
    Values []string        // bounded values, retained for diagnostics
}
type MatchDecision struct {
    Matched bool
    Method string          // title | alias | country | convention | id
    Code string            // empty on success; codes in §8
    Reason string
    MatchedTitle string
    MatchedID *domain.ExternalRef
    Warnings []string
}
func Evaluate(e ReleaseEvidence, w domain.Wantable, index IdentityIndex) MatchDecision
```

`IdentityIndex` is an immutable domain structure built from **all video
library items**, including unmonitored and already-complete items. It maps
normalized titles/aliases and external IDs to distinct media-item IDs.
Episode targets and quality copies of one item count as one identity.
Return all covered wantables only after one work is selected. A targeted
interactive search does not make an ambiguous name unique.

Rank surviving title evidence as follows, after hard conflicts and year/kind
filters. Retain each work's best tier; do not count several aliases for one
work as several candidates:

| Rank | Evidence |
|---|---|
| 1 | Full canonical literal title, including the metadata-year variant in §6.1. |
| 2 | Full manual work alias. |
| 3 | Full provider work alias, including original and historical titles. |
| 4 | Verified country-qualifier-elided match. |

Ambiguity applies only to ties in the highest nonempty tier. For example,
item A's provider alias equal to item B's canonical name does not stop B's
normal grabs. A compatible exact returned ID may select a lower-tier work;
title rank never overrides contradictory IDs, country, scope, or explicit
year evidence. Recompute alias-shadowing diagnostics with the identity index
and expose them on Media Detail and health, even if no release was searched.
Show both affected items and the shadowed provider/manual alias; a lower
tier still remains eligible when the higher tier is eliminated by valid
year or ID evidence.

**Unqualified original/remake tie-break.** After the tier rule, if exactly
two series tie on the same full canonical base title and the release has no
country qualifier, use country-alias asymmetry: when exactly one of the two
owns a supported country-qualified alias of that base, prefer the other
for the unqualified release. A qualified release still selects the work
owning that qualifier. This keeps the unqualified UK and qualified US
Have I Got News for You naming convention usable with both in the library.

Apply this convention only with successful, nonstale, complete alias
snapshots for both items' selected providers, no never-completed or later
failed required enrichment, no country/scope conflict, and at least one
exact known qualified alias on the qualified side. A missing alias fetch is not proof
of alias absence. If both/neither have qualifiers, more than two series
tie, or snapshot completeness is unknown, retain `ambiguous_identity`.
Never choose the only monitored item or the only item with a matching
episode. Check coverage only after choosing the identity; if coverage then
fails, do not switch to the sibling.

Record `method=convention`, both item IDs, the qualified alias, and snapshot
timestamps in the decision/history. Explain “Unqualified title assigned to
the original under the known regional naming convention.” This is a naming
convention, not an ID guarantee: an unqualified remake release with no ID
can remain indistinguishable. An exact returned ID overrides the convention;
make the convention visible in both item diagnostics and candidate rows.

Decision order:

1. Validate supplied evidence and media kind. Compare same-namespace IDs
   whenever both sides know them; any contradiction blocks this pairing.
   Conflicting repeated attributes also block it. Unknown namespaces or
   missing values do not create a match.
2. Collect canonical, alias, and country-variant candidates within the
   requested media kind, excluding empty normalized keys and blocked-scope
   aliases. A blocked alias for the candidate is a numbering conflict,
   checked before ID rescue. Apply movie year tolerance (existing ±1 when
   both years are known) **before** deciding
   whether titles are ambiguous. Apply an explicit `SeriesTitleYear` filter
   here for series too, using the rule in step 6. Thus `Heat.1995` may select
   the 1995 movie even when a 1986 movie has the same name. Preserve eliminated candidates
   as rejection evidence. Confirm country conflicts using §6.1.
3. Collect exact ID candidates independently, so a different title can
   still find the right item. If a returned ID identifies another known
   library item, it cannot be ignored in favor of the searched item. If
   different returned IDs resolve to different works, reject as conflict.
4. Resolve one work. A compatible exact ID can disambiguate title tiers or
   identify an otherwise unmatched name. It cannot override a confirmed
   country/ID/scope/year conflict. Otherwise select the highest title-evidence
   tier and apply the narrowly defined original/remake tie-break above.
   Remaining same-tier ties mean `ambiguous_identity`; monitoring and
   episode availability never break an identity tie.
5. Check coverage: exact requested season/episode, existing multi-episode
   and season-pack rules, and existing anime absolute-number behavior.
   IDs do not invent coverage or map episode numbering between providers.
   Daily/date-named episode coverage is not implemented by the baseline
   matcher and remains out of scope here: return `coverage_unsupported`
   for a parsed daily release, even if its series ID matches. Add a fixture
   and visible explanation; do not claim all news/panel release forms work.
6. Apply the same movie-year check to ID candidates; an ID cannot bypass
   it. Apply existing quality/profile/blocklist/size rules in their existing
   callers. Series premiere year is not episode year. Add a separately
   parsed `SeriesTitleYear` for an explicit year in the title segment before
   the episode marker; ignore years in episode titles or trailing release
   tags. When present, require exact premiere-year agreement for title/alias
   disambiguation and ID candidates with a known premiere year. Without
   such a marker, keep the existing series year behavior.

When the same work has both literal and ID evidence, report `title`/`alias`
as the primary method and retain corroborating IDs. If the naming convention
was needed, report `convention` even though both canonical titles matched.
When only an ID made the pairing possible, report `id`. Persist the explanation; a later alias
refresh must not rewrite why a historical grab happened.

The all-library index only knows local collisions. Unknown external
same-name works remain a limit of title matching, as they are today. Do not
claim uniqueness across the world's catalog or query that catalog per RSS
item. Country-only matching requires the positive evidence specified above.

### 6.3 Integration and download provenance

Replace the three video matching paths in `SearchCopy`, `SyncRSS`, and
`searchAndGrabBest` with a shared evaluator. Keep book calls to the existing
matcher intact. Build/reuse the identity index once per operation, not once
per release or episode; cache it with the invalidation rules in §4.3.

Extend `ports.Release` with returned IDs, ID parse issues, and an opaque
indexer GUID. Normalize `imdbid` values with or without `tt` at the adapter
boundary. Ignore missing/zero IDs; retain a diagnostic for malformed values.
Never infer IDs by scraping arbitrary digits out of titles or download URLs.
Malformed ID values provide no positive evidence; other valid title/alias
evidence may still match with a warning. Two distinct valid values for the
same ID attribute are a conflict, not merely a malformed-value warning.

For automatic grabs, carry server-computed match evidence through
`autoGrab`, queue insertion, history, and retry. For manual grabs, preserve
the user's explicit target selection and existing override behavior. Record
`manual` provenance instead of trusting a client-supplied “matched by ID”
claim. The client must not be able to convert unverified IDs into server
evidence by echoing response fields.

Use the proposed `downloads.match_evidence` JSON column to store a versioned
snapshot: `version:1`, method, supplied IDs, target IDs, original/parsed
title, matched alias, country interpretation, and warnings. Store the same
evidence in grab history. Old queue rows with `{}` retain existing behavior.
Review [download storage](../internal/infra/sqlite/acquisition.go) and
[import](../internal/app/acquisition/import.go) so ID-matched downloads keep
their selected item and episode links through restart/import. Do not loosen
file episode parsing or destination/quality checks.

## 7. Capability-aware indexer search with bounded fallbacks

### 7.1 Capabilities and wire contracts

Add an optional `ports.IndexerCapabilitiesProvider` interface with
`Capabilities(context.Context) (IndexerCapabilities, error)`. Model generic,
TV, and movie search availability and supported parameters separately.
Parse `t=caps` XML structurally. Explicit `available="no"` forbids that
mode; absent/malformed capabilities mean unknown, not universal support.

Cache capabilities in a service shared across adapter instances, keyed by
indexer ID and configuration revision. A cache inside the short-lived
`torznab.Client` alone will not survive Monarr's factory calls. Proposed
TTL: **24 hours**, with concurrent-request coalescing. Invalidate on endpoint,
credential, or category edits; Test forces refresh. On refresh failure use
the last valid snapshot for the **same configuration revision**. Never reuse
old endpoint capabilities after an edit. Without a valid snapshot, permit
one legacy generic text probe and report degraded capabilities; it counts
toward the search budget. A valid snapshot declaring generic search disabled
forbids that probe and any generic fallback. If no supported mode remains,
report `unsupported_query`. Do not persist or log API keys in diagnostics.

The refresh-failure fallback above applies only to transport, invalid-response,
or unsupported-caps errors. Authentication and rate-limit errors from the
**caps request itself** stop this indexer operation, including RSS: do not
use stale capabilities or issue a generic probe after those errors. Honor
`RetryAt`. With entirely unknown capabilities, perform exactly one generic
canonical-title probe for a targeted search; no ID/alias expansion is allowed
until a valid capability snapshot exists. A successful probe does not prove
support for other modes or parameters.

Add explicit structured ID/query modes to `domain.SearchQuery`. Represent
season/episode presence separately from numeric values (`*int` or presence
flags), because season 0 is valid. Select `tvsearch` by kind and capability,
not by `Season > 0`. For movies use `t=movie` only when advertised; retain
generic `t=search` only when advertised or capabilities are unknown as above.
RSS retains the empty generic query when allowed. With explicitly disabled
generic search, skip RSS on that indexer and report unsupported RSS in its
health/diagnostics; targeted TV/movie searches may still work. Do not turn
RSS into an expensive loop over every library ID.

Illustrative requests, excluding configured API key and category parameters:

```text
/api?t=tvsearch&tvdbid=414217&season=1&ep=1
/api?t=tvsearch&imdbid=tt16867040&season=1&ep=1
/api?t=movie&imdbid=0137523
/api?t=movie&tmdbid=550
/api?t=search&q=Have.I.Got.News.for.You.US.S05E01
```

Send only advertised parameters. Use `tt`-prefixed IMDb values for TV
requests as Sonarr does. For `t=movie`, strip exactly the `tt` prefix,
preserving leading zeros, as
[Radarr's request generator](https://github.com/Radarr/Radarr/blob/develop/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs)
does. Pin both wire forms in adapter fixtures. Keep internal IDs canonical
regardless of wire form.
Never send a combined title and ID unless that exact combination is
documented by the endpoint. Initial implementation sends one ID per request;
aggregated-ID extensions are deferred.

If TV mode lacks season/episode parameters but supports IDs, send the ID
query without unsupported filters and enforce episode coverage locally.
For text queries, use title plus supported separate season/episode filters;
include `SxxEyy` in `q` when those filters are unavailable. Full-season
queries omit `ep`; specials explicitly send season 0 where supported.
Generic mode preserves the complete existing text query.

### 7.2 Search tiers and stopping rules

Change the planner/executor contract to represent ordered alternatives;
the current flat fan-out cannot express fallback. Keep media-kind planning
in domain code, capability filtering in the adapter/application boundary,
and all network access outside the matcher.

Proposed domain contract (retain the existing book planner separately):

```go
type SearchPlan struct {
    IDs []SearchQuery     // ordered known-ID alternatives, before caps filter
    Titles []SearchQuery  // canonical then ordered eligible aliases
}
func PlanVideoSearch(w Wantable) SearchPlan
```

Extend `SearchQuery` with `ID *ExternalRef` and explicit season/episode
presence; exactly one of ID or Q is set for targeted video queries. The
executor filters unsupported alternatives before applying the mode budgets
below. Thus an unadvertised TVDB query
does not consume the IMDb slot. The adapter serializes one selected query
and never performs hidden fallback requests of its own.

For an interactive search on each indexer, try:

1. Preferred supported ID: TVDB for series; IMDb for movies.
2. Next supported known ID: IMDb for series; TMDB for movies. A TV TMDB
   query may be used if advertised and the series has a verified TMDB ID.
3. Canonical title.
4. At most **two** distinct alternate title queries: manual searchable
   aliases first, then original-language title, then a known country-qualified
   alias. Sort within each priority by normalized title for deterministic
   behavior. Deduplicate equivalent queries before applying the limit.

Interactive search tries at most **two ID query variants** and **three
title query variants** per indexer per target: at most **five logical
searches**. Automatic searches (backlog, search-on-add, and auto-search
actions) use **one** highest-priority supported ID, the canonical title,
and at most **one `Searchable=true` alias**, in that order: at most **three
logical searches**. If the ID tier is unavailable, do not spend its slot
on another alias. Prefer manual searchable aliases, then original titles,
then other explicitly searchable aliases. No second ID query is made
automatically, even if the selected ID returns no results.

These budgets exclude at most one capability refresh and include every
attempted release query, including errors. The existing 20-target backlog
cap means at most 60 release-query requests per indexer per run (120 for two
runs), versus 100/200 under the previous five-query automatic proposal.
Actual use is lower when tiers succeed or aliases/IDs are absent. This is
still up to 3× the baseline one-query-per-target cost; keep query counts
visible in diagnostics. No new pagination expansion is included; adding
pages later requires its own explicit request budget.

For automatic search, advance when the current tier has no **eligible grab**:
identity, requested coverage, quality/profile, blocklist, size, and existing
in-flight checks must all permit it. A matching but blocklisted or
wrong-quality result must not hide a usable alternate-name release. Stop
at the first tier with an eligible candidate and select the best candidate
from all results collected so far. Interactive search runs all supported
tiers within the five-query budget so the user can inspect alternatives;
it retains rejected results and marks deadline-truncated search as partial.
The automatic executor also stops at its three-query limit. The evaluator's
identity verdict is identical in both modes; search breadth
and existing manual/automatic policy differ intentionally.

Create **one** `context.WithTimeout(ctx, s.searchTimeout)` for each
indexer/target execution, covering capability refresh and every tier. The
baseline callers create per-query timeouts; there is no existing aggregate
deadline to reuse. Propagate earlier caller cancellation. Indexers can run
concurrently, but tiers on one indexer are sequential. Unsupported-mode or
unsupported-parameter errors
invalidate the relevant capability and allow a supported text fallback
within the same budget. Authentication failures and 429 responses stop
that indexer for this operation; respect retry timing. Timeouts consume the
same deadline. A failure on one indexer does not discard another's results.

Deduplicate within an indexer by nonempty GUID, then exact download URL.
Never collapse distinct releases by title alone; if both keys are missing,
retain distinct occurrences (the baseline adapter already requires a
download URL). Preserve and reconcile ID evidence when the same release
appears in multiple tiers. Conflicting
duplicate IDs must remain visible and block automatic acceptance. Do not
merge evidence across unrelated indexers solely because titles agree.

## 8. API output, UI explanations, and observability

Extend [OpenAPI](../internal/api/openapi.yaml) first, then regenerate Go
types. Add optional `match` to `ReleaseCandidate` and identity fields to
metadata results/item detail. Update the explicit response mapping in
[acquisition handlers](../internal/api/acquisition_handlers.go); adding
internal fields alone does not expose them. Keep existing `accepted`,
`isUpgrade`, and `rejections` fields for client compatibility.

Keep the release-search response array shape. Add documented response
headers `X-Monarr-Search-Partial: true|false` and, when partial,
`X-Monarr-Search-Reason: deadline|indexer_failure|capabilities_unknown`.
Use that precedence when several causes apply and retain per-indexer detail
in logs. Return these headers even for an empty array. Extend web/mobile
search clients to read them and display a short incomplete-search notice;
do not require a candidate row to exist before reporting partial results.
Expose these headers through the existing CORS policy where needed. An
exhausted documented query budget is complete, not a timeout.

Example output shape, with synthetic target IDs where appropriate:

```json
{
  "matched": true,
  "method": "alias",
  "parsedTitle": "Have I Got News for You US",
  "targetTitle": "Have I Got News for You",
  "matchedTitle": "Have I Got News for You (US)",
  "reason": "Matched a known title for this series",
  "warnings": []
}
```

**How to read it:** `match.matched` establishes identity and coverage;
`accepted` still includes quality and acquisition policy. A correctly
identified release can therefore remain rejected for quality.

| Code/method | Example visible explanation |
|---|---|
| `alias` | “Matched alternate title: Have I Got News for You (US).” |
| `country` | “Matched US title variant; library series is identified as US.” |
| `id` | “Matched TVDB ID 453187; release title differs.” |
| `convention` | “Unqualified title assigned to the UK original; the US sibling has a known US-qualified alias.” |
| `title_mismatch` | “Release title ‘X’ does not match ‘Y’ or its known aliases.” |
| `country_conflict` | “Release specifies US; library series specifies GB.” |
| `id_conflict` | “Release TVDB ID 123 differs from library TVDB ID 456.” |
| `ambiguous_identity` | “This name identifies multiple library items; a matching ID is needed.” |
| `episode_mismatch` | “Release contains S05E02; requested S05E01.” |
| `coverage_unsupported` | “Date-named episodes are not supported by this matching path.” |
| `year_mismatch` | “Release year 2024 does not match library year 1995.” |
| `metadata_conflict` | “The stored title year and premiere year disagree; identity needs review.” |
| `numbering_scope_conflict` | “This alias names a programme within the provider's umbrella series; episode numbering cannot be mapped automatically.” |
| `identity_unresolved` | “Country variant could not be verified; no matching alias or ID.” |

Keep outer rejection code `not_matched` for compatibility; put the specific
code in `match.code`, with the improved text also in `rejections[].reason`.
Do not expose credentials, full query URLs, or raw provider responses.

On [Media Detail](../web/src/pages/MediaDetail.tsx), provide a compact
identity section showing IDs, known aliases, country evidence, source, and
last successful refresh. Add manual alias creation/removal through proposed
`POST /library/{id}/aliases` and `DELETE /library/{id}/aliases/{aliasId}`
routes. Payload: `title`, `searchable` (default false). These routes only
edit manual aliases, use existing authenticated-write authorization, and
validate 1–256 trimmed characters with at least one normalized token.
Reject duplicate manual normalized aliases; do not require global uniqueness
because ambiguity is handled by the matcher. Provider aliases are read-only.
Mobile must at least display this information and matching explanations;
manual alias editing can initially use the web UI, documented explicitly.

Log structured decisions with item ID, indexer ID, method, reason code,
external ID namespace, and query tier. Expose query-tier/capability failures
in existing search diagnostics. Aggregate counts for alias/country/ID
matches and conflicts using existing logging infrastructure; a new metrics
backend is outside scope. Refresh failures must say whether cached identity
was used.

## 9. Acceptance matrix and regression fixtures

Use fixture IDs for tests unless the fixture deliberately captures provider
mapping. Network tests use `httptest`; no CI calls to live metadata services.

| Area | Required cases |
|---|---|
| Screenshot | With both real UK/US identities from §1 present, EDITH and RAWR US S05E01 releases select US; unqualified S68E01 selects UK under the fresh-snapshot convention; wrong season/episode remains rejected. |
| Alias lifecycle | Add, batch load, refresh, successful empty refresh, partial endpoint failure preserving the entire snapshot, historical alias surviving two refreshes, manual alias survival, delete cascade, restart. |
| Countries | Suffix/prefix/bracketed middle; `US`, `U.S.`, `U. S.`, and parsed `U S`; optional qualifier on either side; UK→GB; US≠GB even with a matching returned ID; unknown evidence; coproduction ambiguity; market≠origin. |
| Literal words | `Us`, `This Is Us`, `The Last of Us`, `Made in America`, and an unbracketed middle `US` remain meaningful title words. |
| Alias scope | Same-tier ties remain ambiguous including unmonitored/complete works unless the defined convention applies; Cunk exception blocks title, country, query expansion, and ID rescue without blocking ordinary aliases. |
| Evidence ranking | Canonical beats another work's manual/provider alias; manual beats provider; provider beats qualifier-elided; exact compatible ID can select a lower tier; shadowed aliases appear in health before any search. |
| Regional convention | Unqualified UK/qualified US in both directions; both/neither qualified or three-way tie stays ambiguous; stale/failed/unfetched snapshots cannot break a tie; coverage cannot switch the selected work; returned ID overrides the convention. |
| Series year | `Doctor Who (2005)` versus `Doctor.Who.2005.S01E01`; both Show 2024 forms with/without trailing Summer of 1999; title qualifier/premiere conflicts; movie/numeric titles unchanged. |
| Empty normalization | Distinct CJK titles/aliases and punctuation-only names never match via an empty normalized key; ID fallback and display still work. |
| Release IDs | Matching ID rescues different title; differing known ID blocks matching title; matching IMDb plus conflicting TVDB blocks; zero/malformed/repeated IDs. |
| Query evidence | ID query returns an unrelated title with no result ID: no automatic match; matching returned ID succeeds without a title alias. |
| Coverage | Matching ID with wrong episode, season pack, multi-episode file, season zero, absolute numbering, unparseable coverage, and explicit unsupported daily/date coverage. |
| Movie isolation | Original/translated/alternate title with existing ±1 year rule; same-name remakes disambiguated by year before ambiguity; same numeric TMDB ID in different kinds. |
| ID lookup | All syntax in §5.1, case/whitespace/overflow/leading zeros, no text fallback for invalid explicit IDs, movie/TV/episode/person filtering. |
| Provider fallback | Missing TMDB key with working TVmaze; 404 vs 429 vs timeout; contradictory raw TVmaze externals; nonaddable TVmaze record falling through to addable TMDB result; offline local hit. |
| Source preservation | Persisted TVmaze→TMDB-ID enrichment survives restart without changing refresh source; native legacy/compat/import-list/adoption adds always stamp source; new Jellyseerr adds use selected TVmaze ordering; transient preferred-provider failure returns 503 and no Add. |
| Duplicate control | Add through TVDB then IMDb/TMDB yields existing item; concurrent adds; IDs pointing to two existing rows; same title/year but distinct IDs stays separate. |
| Capabilities | Generic-only, TVDB-only, IMDb-only, movie-ID-only, missing caps, explicit disabled generic including RSS, malformed XML, stale cache, config edit, concurrent refresh, caps auth/429 stopping all follow-up requests. |
| Query tiers | Automatic fallback past wrong-quality/blocklisted hits; automatic stop on eligible grab; automatic maximum three queries/one searchable alias; interactive maximum five; partial headers; one total deadline including caps; cancellation; typed HTTP-200 XML errors; TV tt-prefixed and movie numeric IMDb wire forms. |
| Error/queue contracts | HTTP 409 already_exists versus identity_conflict; durable provider failure returns nil from the real identity job handler; failure-state persistence failure still uses queue retry. |
| Deduplication | Same GUID across tiers preserves IDs; conflicting repeated evidence rejects; distinct releases with same title survive. |
| Path parity | Interactive, RSS, backlog, and explicit auto-search produce equivalent identity decisions for equivalent inputs. |
| Provenance | Restart preserves ID-match explanation; manual request cannot forge server evidence; existing `{}` rows import normally; copies share item identity. |
| UI/compat | Native web/mobile ID search; existing-library indication by every ID; Sonarr/Radarr lookup/add DTOs; clear errors and detailed rejection display. |
| Books | Existing author/title matching, ebook/audiobook queries, and edition copies unchanged. |

Add deterministic unit tests around the pure parser/evaluator; fixture-based
adapter tests for actual XML/JSON shapes; service tests for query budgets,
cache invalidation, and matching parity; API/compat tests for wire contracts;
and a small Playwright flow covering ID lookup, add, release alias match,
and a regional conflict. Include migration from the real prior schema, with
existing duplicate IMDb/TVDB values, downloads, and manual entries.

## 10. Migration, release, and recovery

This plan changes no runtime behavior and does not itself bump VERSION.
When implemented, bump [VERSION](../VERSION), update
[Usage](usage.md), [Settings](settings.md) where applicable, and the delivery
ledger in [STATUS](../STATUS.md). Record the accepted identity policy in a
new ADR and reconcile ADR 0011's stale implementation-status wording.

Roll out storage and enrichment before automatic matching consumes them.
These milestones are implementation order, not permission to deploy an
inconsistent subset. Deploy only after all runtime paths are connected and
the final gate passes; no interim feature flags are implied.
Until enrichment succeeds, exact legacy title matching continues and unsafe
country guesses remain unresolved. ID search must not depend on completion
of the whole-library backfill. Enable all video acquisition paths together
after the parity tests pass; a UI-only fix would leave RSS/backlog broken.

Back up SQLite before deployment. The migration is additive and performs no
file operations or library merges. On regression, stop automatic acquisition
and roll back to the previous binary only after verifying it can open the
migrated database in a downgrade test. Otherwise restore the backup with
the old binary and reconcile downloads created since it. Do not drop alias
tables as an improvised rollback or rewrite historical evidence.

Use a coordinated upgrade for nodes sharing the database: stop all old
workers before the new binary migrates or queues identity jobs, upgrade all
nodes, then resume work. Existing queue workers claim jobs without filtering
for registered handler kinds; an old worker would permanently fail the new
job type. Mixed-version operation is not supported by this milestone. Before
a binary rollback, disable the identity sweep and let identity jobs finish
or cancel them through the supported queue administration path. The
downgrade test must cover pending jobs as well as schema opening. Register
new handlers before `queue.Start`, including test and standalone wiring.

## 11. Implementation milestones and verification

### 11.1 Capture failures and introduce pure identity contracts

Add screenshot fixtures, ID-input parser, typed identity evidence, country
variants, and a pure evaluator. Keep existing production call sites using
the old matcher until storage and all callers are ready.

**Acceptance:** domain tests prove the positive screenshot case and the
negative country/ID/episode cases in §9. Repeat with randomized test order:

```bash
go test ./internal/domain/... -count=3 -shuffle=on
```

### 11.2 Persist identity and enrich existing items

Add migration, sqlc queries, source-scoped replacement, provider enrichment,
job registration, batching, and identity-index invalidation. Add manual alias
CRUD after storage exists. Preserve episode-provider selection.

**Acceptance:** migrate seeded old schema, restart, refresh with a failed
provider, and verify aliases/provenance and existing downloads survive.

```bash
make gen
go test ./internal/infra/sqlite/... ./internal/adapters/tmdb/... ./internal/adapters/tvmaze/... ./internal/app/library/... -count=1
```

### 11.3 Resolve external IDs across native and compatibility search

Implement the shared resolver, additive IMDb response fields, native error
mapping, verified-ID deduplication, all-ID in-library checks, transactional
add checks, and compatibility wiring. Add the UI input hints and exact-ID
existing-library lookup.

**Acceptance:** `tvdb:414217` and `imdb:tt16867040` fixture lookups resolve the
same series; adding through both creates one item. A missing provider key
still permits the keyless series route. Compatibility DTO tests pass.

```bash
go test ./internal/app/library/... ./internal/api/... ./internal/compat/... -count=1
```

### 11.4 Preserve indexer IDs and execute query tiers

Parse IDs/GUIDs from Newznab and Torznab feeds, implement shared capability
cache, structured query modes, season-zero support, tier execution, and
evidence-preserving deduplication. Wire both search implementations through
one executor. Apply §7.1's RSS capability guard while preserving the empty
generic RSS query on supported/unknown-capability endpoints.

**Acceptance:** request-recording tests assert exact modes/parameters,
fallback order, capability invalidation, and maximum three automatic/five
interactive logical searches per indexer. Authentication/429 errors must not fan out into more
requests on that indexer.

```bash
go test ./internal/adapters/torznab/... ./internal/app/acquisition/... -count=3 -shuffle=on
```

### 11.5 Switch matching paths and retain evidence through import

Build the all-library identity index; switch interactive search, RSS, and
automatic search together; add queue/history evidence; expose detailed
matching output on web/mobile. Preserve manual grabs and book behavior.

**Acceptance:** the same fixture produces the same identity verdict through
all acquisition paths. A restarted ID-matched download imports into the
original item/episodes. An unmonitored regional sibling still prevents an
ambiguous automatic grab unless the explicitly tested original/remake
convention resolves the tie. A lower-ranked alias never blocks a canonical
match solely because its item is present.

```bash
go test ./internal/app/acquisition/... ./internal/api/... -count=3 -shuffle=on
make test-web
make test-mobile
```

### 11.6 Complete documentation and run the delivery gate

Recheck the interfaces/source links in this plan, update shipped behavior
docs and VERSION, and record implemented milestones without deleting the
design rationale. Run the repository's required gate on the final files:

```bash
make lint                 # pinned linter and formatting
make test                 # Go packages, including architecture checks
make test-web             # browser-client unit tests
make test-mobile          # native-client types and tests
make test-e2e             # builds server/UI and runs Playwright
make gen                  # generated API and SQL contracts
git diff --exit-code -- internal/infra/sqlite/gen internal/api/gen
```

The generated-code check must run after intentional generated changes are
recorded, or compare generation against a saved pre-generation snapshot;
it must distinguish expected implementation changes from stale output.
Also run the race/coverage checks required by
[CI](../.github/workflows/tests.yml), preserving the coverage floor. Use
the Go version from [go.mod](../go.mod), Node version from CI, and pinned
Makefile tools. Report environmental blockers by exact command; do not call
an unrun or failed gate green.

**Definition of done:** every §9 behavior has a passing fixture or observable
UI check; all three ID workflows work; aliases survive refresh/restart;
automatic and interactive decisions agree; conflicting identities remain
blocked; upgrade/downgrade behavior is tested; required gates pass.

## 12. Upstream precedents and differences

Reviewed 2026-09-17. These links track upstream development branches and
may move; capture commit-pinned links with implementation fixtures when
building. The behavior proposed above is Monarr's contract, not a claim of
byte-for-byte Sonarr/Radarr compatibility.

- [Sonarr ParsingService](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/Parser/ParsingService.cs):
  `FindSeries` uses scene mappings, normalized titles, and external-ID
  fallbacks. Alias mappings identify a specific TVDB series. Monarr adopts
  the ID fallback concept while explicitly rejecting conflicting evidence.
- [Sonarr NewznabRequestGenerator](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs):
  checks advertised capabilities, generates ID queries, and places title
  searches in a later default-search tier. Monarr's exact stopping rule and
  three-query automatic/five-query interactive budgets are local choices.
- [Radarr ParsingService](https://github.com/Radarr/Radarr/blob/develop/src/NzbDrone.Core/Parser/ParsingService.cs):
  matches canonical/original/alternate/translated titles and external IDs,
  with year checks in its lookup paths. Monarr retains its own existing
  year tolerance.
- [TVmaze API](https://www.tvmaze.com/api): exact show lookup accepts TVDB
  or IMDb IDs; show AKA records have naming-market country information.
  That country must not be reused as origin evidence.
- [TMDB Find By ID](https://developer.themoviedb.org/reference/find-by-id):
  resolves external identifiers; consume only the requested media kind's
  result collection and retain verified cross-provider identity.
- [Torznab specification](https://torznab.github.io/spec-1.3-draft/torznab/Specification-v1.3.html):
  `t=caps` advertises search modes and supported parameters. Treat this
  draft as protocol guidance and pin Prowlarr/Newznab response fixtures;
  endpoints may implement only a subset.

## 13. Adversarial review — findings addressed on 2026-09-17

### 13.1 Initial agent review

An independent review agent examined the plan against the repository,
reviewed the corrections, and confirmed closure of its reported findings.
The author also checked the provider, acquisition, parser, and queue paths.
The subsequent supplied review found additional holes (§13.2); the initial
signoff was not a guarantee of completeness. These are design reviews, not
evidence that the feature is implemented or tested.

| Finding | Resolution |
|---|---|
| Non-Latin aliases can normalize to the same empty key. | Exclude empty keys from indexes/comparison; preserve display and ID fallback; add negative fixtures (§4.2, §9). |
| Same-name remakes were declared ambiguous before year/kind filtering. | Filter by kind and explicit year before identity selection; keep ID year checks (§6.2). |
| Adding a TMDB ID could silently change a TVmaze series' episode provider. | Persist and route by hydration source; test lookup/add/refresh continuity (§5.2). |
| TVmaze hydration could conceal a contradictory ID by echoing the request. | Check raw response IDs before assigning requested values (§5.2). |
| A nonaddable first-provider result could prevent a valid later lookup. | Continue exact lookup after unsupported hydration; specify final error precedence (§5.2). |
| Partial refresh could erase aliases; historical/original roles were lost. | Replace complete provider snapshots, persist roles, preserve historical and market records (§4). |
| Work-level endpoints cannot reliably identify all season-scoped aliases. | Use explicit scope and fixture-backed known grouping exceptions; block ID rescue for those exceptions and state the residual limit (§4.3). |
| Disabled modes and caps authentication/rate-limit errors could still trigger requests. | Separate unknown from disabled caps; stop on caps auth/rate limits; guard RSS (§7.1). |
| Diagnostic strings could drive control flow or force forbidden imports. | Structured domain evidence and port errors, mapped into application errors (§4.1, §6.2). |
| The claimed total search deadline did not exist in baseline code. | Create one context around caps and all tiers, and test the total bound (§7.2). |
| A wrong-quality/blocklisted hit could hide a usable alias release. | Automatic fallback stops at an eligible grab within its three-query cap; interactive search explores up to five queries and reports partial results (§7.2, §8). |
| Older workers would permanently fail new job types during rollout. | Require coordinated worker upgrade, handler registration, and pending-job downgrade tests (§10). |

**Document validation:** local file links, numbered section references,
balanced code fences, and whitespace checks passed. The SQL example passed
an in-memory smoke check of syntax, defaults, distinct-market alias storage,
and deletion cascades against minimal predecessor tables. This is not the
real-schema upgrade test required by §9. No runtime files changed. The prior
full-gate attempt remains incomplete: web tests passed, while lint required
unavailable dependency-network access and Go/end-to-end setup hit Go cache
permissions. These documentation revisions do not establish a green runtime
gate; implementation must run §11.6.

### 13.2 Supplied review — APPROVE WITH CHANGES

The supplied review checked baseline `0da1c75`, reported executing its parser
and matcher reproductions with Go 1.25.7, and queried TVmaze. It could not
refresh remote Git refs or inspect live TMDB responses. This revision
checked the cited local source/storage/error/queue paths and Radarr's
upstream request generator; live TVmaze refetch was unavailable, so §1 labels
the IDs/aliases as review-supplied observations. Do not describe those
executions as tests performed by this documentation update.

| Review item | Disposition in this revision |
|---|---|
| M1 — original/remake ambiguity breaks ordinary UK releases | Added the qualified/unqualified asymmetry convention for exactly two tied canonical series, with complete/fresh snapshot requirements, explicit provenance, both-direction fixtures, and an honest residual naming ambiguity (§6.2, §9). Missing alias data never establishes ownership. |
| M2 — provider aliases can block canonical names | Added canonical → manual alias → provider alias → qualifier-elided ranking, highest-tier ambiguity, compatible ID precedence, and shadowed-alias health/detail diagnostics (§6.2). |
| S1 — series-year variants and trailing episode years | Added metadata `(YYYY)`/`[YYYY]` variants, pre-marker release year extraction, bypass of global last-year scanning for series, and golden corpus fixtures (§6.1). |
| S2 — new blank sources and unwritten TMDB enrichment | Required source stamping on all insertion paths plus a dedicated transactional identity write, tested across restart and refresh (§5.2). |
| S3 — compat numbering change and outage-selected providers | Documented new Jellyseerr source selection; configured preferred-provider operational failure returns 503 for a new series instead of offering a lower-source Add (§5.2). |
| S4 — automatic query amplification | Automatic searches use one supported ID + canonical + one searchable alias, maximum three; interactive maximum remains five. Documented the remaining maximum 3× baseline cost (§7.2). |
| S5 — dotted US | Added `U.S.`, `U. S.`, and parsed `U S` variants with positional guards (§6.1). |
| S6 — overloaded 409 | Added machine-readable native error codes for already-added versus identity conflict, preserving the message and compat contracts (§5.1). |
| S7 — identity job retries | Explicitly return nil after durable provider-failure state; storage/cancellation failures still use queue error handling (§4.3). |
| S8 — daily episodes | Marked date-based coverage unsupported with a specific rejection and fixture (§6.2, §8). |
| Nits — fixture identities, IMDb wire form, migration reference | Added the UK/US IDs separately from Cunk examples; specified numeric-only movie IMDb serialization; cited migration 0017 (§1, §5.2, §7.1). |

All supplied findings have a documented disposition and an acceptance case.
The external reviewer has not re-reviewed this revision. Implementation and
the runtime delivery gate remain outstanding.
