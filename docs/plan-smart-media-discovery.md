# Smart media discovery — build theme search and related-series suggestions

**Status:** ready for Fable design review; adversarial findings resolved at
specification level and independently rechecked. M0 empirical gates remain
pending. No feature code implemented. **Written:** 2026-09-29.
**Scope:** TV series on web, including
mobile web; additive server API for later native adoption.

Companion to [the preview plan](plan-search-result-preview.md),
[discovery ADR 0015](adr/0015-discovery.md), and
[the architecture](architecture.md). This document specifies what to build,
in what order, and what must be demonstrated before release. The companion
[adversarial review](plan-smart-media-discovery-review.md) records challenges
and their disposition. Work milestone by milestone. Re-verify the code seams
in §2 against the actual build branch before editing; the author's working
checkout is older than the inspected integration baseline.

## 1. Outcome — find a theme, then explore without losing it

The motivating request is: “Find a gay-themed TV series, then intelligently
suggest others like it.” Curator should find works whose titles do not contain
the query, explain the connection, and let the user add them with the existing
library controls. A popular but unrelated series is not a successful result.

The first release provides:

1. **Describe what you want** beside the existing title-search mode in Add
   media → Series. Example: `gay-themed TV series`.
2. **More like this** on series results and their preview, with the selected
   work identified explicitly. A result need not already be in the library.
3. **Visible refinements:** theme, genre, language, year range, central-theme
   requirement, and Hide in library. The initial theme remains active when
   following More like this; changing the seed does not clear it.
4. **Reasons grounded in metadata:** a keyword or synopsis excerpt for a
   thematic match; a separate indication when only descriptions are similar.
5. **Existing preview and Add behavior**, including identity checks, quality,
   folder, monitoring, and search-on-add choices.

**Non-goals:** movies and books in this release; native-app feature parity;
watch-history personalization; automatic additions; new import-list behavior;
indexer release ranking; download availability predictions; whole-TMDB
mirroring; arbitrary conversational instructions; generated plot facts;
mandatory cloud inference; any runtime dependency on a running Cinema server.
These boundaries keep the first evaluation focused on discovery quality.

Gay themes are one acceptance case in a reusable theme system. Do not equate
gay male stories with all LGBTQ+ stories, and do not equate either with adult
content. Broadening to LGBTQ+ is an explicit user action. Classification
describes the work's content; cast demographics do not establish its theme.

## 2. Evidence — build from the newer code, preserve local work

### 2.1 Inspected revisions and limits

| Source | Revision inspected | Meaning |
|---|---|---|
| Curator working HEAD | `0da1c755da44fdb0cd7bf615997016c8c096dc5c` | Older checkout with substantial pre-existing edits and untracked plans; not the implementation base |
| Locally fetched `origin/main` | `10d5a9672ea1bf19476bfb62c5df8034f65324ad` | Source inspected with `git show`; no claim that this is the latest remote head |
| Cinema / plurx local HEAD | `b5fa758644d413ef73000ded00cedc15766eba02` | Reference implementation inspected locally; deployment state not verified |

The build must start from then-current main in an isolated checkout, carrying
this plan deliberately. Do not reset, stash, or overwrite the user's existing
working changes to achieve that. Fetch and record the build SHA at M0.

### 2.2 Existing code to reuse

The paths below refer to the inspected `origin/main`; some files are absent
from the older working tree. Re-verify signatures at build time.

| Seam | Existing behavior | Required extension |
|---|---|---|
| `internal/app/library/library.go`, `Service.Search(ctx, kind, query)` | Title search, exact external-ID resolution, provider-chain identity handling | Keep unchanged; do not reinterpret ordinary title input as a theme |
| `internal/ports/metadata.go`, `SearchResult` | Carries TMDB/TVDB/IMDb IDs, `Source`, `HydrationSource`, title, year, overview, poster | Wrap it with recommendation-specific evidence and ownership; preserve Add identity |
| `ports.PreviewProvider.PreviewExternal(ctx, kind, ref)` | Returns a verified work without seasons or episodes | Reuse shallow provider readers for facts; add keyword capability |
| `library.Service.PreviewMetadata(ctx, kind, ref)` | Preview with fresh ownership and addability | Reuse ownership rules through a batched read-only seam; avoid calling the full preview pipeline 60 times |
| `internal/api/preview.go`, `GET /metadata/preview` | Existing preview endpoint and typed states | Use as-is on selection; this project does not invent another preview API |
| `internal/adapters/tmdb/client.go` and `discover.go` | HTTP client, context, rate limiting, shallow summaries; existing raw cache is unbounded and URL-keyed | Extract shared transport; recommendations bypass that raw cache and use §9's bounded caches |
| `internal/app/discover/discover.go` | Curated rows, cached metadata, no auto-add | Leave curated rows intact; use a separate recommendation service |
| `web/src/pages/AddMedia.tsx`, `PreviewDrawer.tsx`, `metadataPreview.ts` | Title results, drawer, route state, existing mutation | Add discovery controls and result wrappers; retain original selection object for Add |
| `internal/api/openapi.yaml`, `internal/api/gen/` | Spec-first native API | Add contracts to the schema, regenerate Go; do not hand-edit generated code |
| `cmd/monarr/main.go` | Wires metadata, library, discovery, API | Wire recommendation service, resource limits, and optional encoder |
| `deploy/Dockerfile` | `CGO_ENABLED=0` Go binary in distroless static image | Prove optional model helper packaging before adopting it |

Current `SearchResult` does not itself express unknown or ambiguous ownership.
The existing preview does. A recommendation must not turn a failed ownership
lookup into “not in library” merely to fit the older boolean.

### 2.3 What Cinema actually supplies

Cinema's inspected `crates/plurxd/src/library_search/semantic.rs` runs
`all-MiniLM-L6-v2` on CPU using Candle, produces 384-dimensional normalized
vectors, limits input to 256 tokens, and bounds inference to two worker
threads. Its metadata classifier is a separate finite rule system in
`crates/plurx-core/src/metadata/classification.rs`. It is not a general
instruction-following model or a source of catalog facts.

Reuse the model choice, bounded execution, evidence separation, and failure
isolation. Curator needs its own external candidate retrieval: a similarity
model cannot rank a show that never enters the candidate pool. Do not copy
Cinema's library/cluster queue, storage schema, or finite theme list wholesale.

## 3. Interaction — explicit search modes and persistent theme context

### 3.1 Page shape

```text
Add media                                      Movie  [Series]  Books

 [Title search]  [Describe what you want]
 [gay-themed TV series                              ] [Find series]

 Looking for: [Gay male stories ×]    [Genre: Any ▾]
 [Central theme only □] [Language: Any ▾] [Year: Any ▾]
 [Hide in library ☑]

 Similar to: <selected series, year> [Remove]       (when a seed exists)

 <poster> Title · year                         [View details] [Add]
          Theme found in provider keywords
          Why this matches ▾                  [More like this]

 12 suggestions · 48 candidates checked · Some metadata unavailable
```

This is a layout contract, not a rendered prototype. Existing root/profile/
monitoring controls remain available and shared with the preview drawer.

### 3.2 Submit, refine, and follow a result

- Ordinary title search retains its current debounce and exact-ID syntax.
  Describe mode submits on Enter or Find series, not every keystroke.
- Parse known phrases in the server's local Go rules and show the returned
  interpreted chips with results. Support the declared vocabulary in §4;
  show unhandled text with
  “Used for similarity only.” Unrecognized negation requires correction,
  rather than silently using a meaning the user did not intend.
- A selected chip is authoritative. Removing an inferred chip records an
  override for the current draft so submitting the same prose does not
  silently restore it. Explicit filter edits replace inferred values for
  that dimension; the UI shows the applied interpretation. The server is the
  sole interpretation authority; web does not maintain a second prose parser.
- More like this uses a verified provider ID, never title text as identity.
  It carries the active theme and filters into the next search. From ordinary
  title results, no theme is implicitly imposed; show optional theme chips
  based on the seed's evidence for the user to choose.
- “Keep this theme” remains visible. Offer “Broaden to LGBTQ+ stories” only
  as a new explicit filter choice; never broaden after an empty response.
- Removing the seed returns to the retained description and filters. If no
  description or theme remains, show the input rather than a random feed.
- Keep the seed itself out of results by verified identity, including aliases
  across providers. A different remake with the same title is a distinct work.
- Cancel obsolete requests and ignore late responses by request generation.
  A new theme, seed, or filter must never display an older response as current.
- Result selection uses the existing preview drawer. Add and More like this
  are sibling buttons, never nested inside the preview trigger.

### 3.3 Navigation, privacy, and accessibility

Keep discovery query/filter state in tab memory, not persistent browser
storage. The URL may contain `mode=describe` and the existing preview identity,
but not the description, theme, or seed trail. Back from a preview restores
the in-memory results and scroll; reload retains supported route state and
asks the user to re-enter the discovery query. Ordinary title-search routing
is unchanged. Do not reuse the title `q` route parameter for describe text.

Return `Cache-Control: no-store` for recommendation and status responses.
Do not log query text, matched excerpts, or theme values. Provider requests
still send resolved keyword IDs and seed IDs to TMDB; “local AI” does not
mean offline catalog discovery. State that in Settings.

Use labeled mode controls, keyboard submission, focus restoration, live
loading/result announcements, and touch targets at least 44 CSS px on mobile
web. Explain errors in text. Reasons cannot depend on hover or color alone.
Native apps retain existing behavior; the API is additive so a later native
milestone can use the same contract without duplicating ranking logic.

## 4. Query interpretation — modest promises with enforceable filters

### 4.1 Versioned vocabulary

Create a pure `internal/domain/recommendation` package for query rules,
evidence evaluation, and ranking. Proposed files and types in this plan are
new unless §2 marks them as existing.

The initial vocabulary covers gay male stories, lesbian stories, broader
LGBTQ+ stories, coming of age, found family, political drama, and space
exploration. Each entry records a stable key, display label, exact phrase
aliases, allowed provider-keyword aliases, and positive/contrary evidence
rules. Treat this as `theme-rules-v1`, committed as code or embedded data.
It is a reviewed product vocabulary, not an automatically generated tag set.

M0 must produce the versioned vocabulary artifact and its provider-resolution
fixtures before M1 exposes any theme. Initial retrieval aliases to verify are
listed below; these are proposed search strings, not claims that those exact
keywords currently exist in TMDB. Each theme has exactly one OR group in v1,
with at most four ordered aliases. A keyword is admitted only when its returned
name exactly matches an allowlisted normalized name in that theme's artifact.

| Theme key | Ordered keyword aliases to verify | Evidence distinction |
|---|---|---|
| `gay_male` | gay romance · gay relationship · gay · lgbt | Broad LGBT retrieval alone cannot prove the narrower theme |
| `lesbian` | lesbian romance · lesbian relationship · lesbian · lgbt | Broad LGBT retrieval alone cannot prove the narrower theme |
| `lgbtq` | lgbt · gay · lesbian · transgender | Umbrella theme; may admit any explicitly evidenced constituent |
| `coming_of_age` | coming of age · adolescence · growing up | Adolescent setting alone does not prove a coming-of-age story |
| `found_family` | found family · chosen family | Biological family keywords are not aliases |
| `political_drama` | politics · political intrigue · government | Also requires provider Drama genre; satire alone is insufficient |
| `space_exploration` | space exploration · space travel · astronaut · outer space | Outer-space setting alone does not prove exploration |

The artifact must contain validated IDs/names observed by fixtures, runtime
resolution aliases, canonical embedding text, admission rules, centrality
rules, contrary-evidence rules, and positive/negative examples for every
exposed theme. IDs are observations, not permanently hardcoded lookups.
Adjust aliases only with recorded retrieval evidence and a rules-version bump.
If a theme has no verified retrieval route, keep its chip unavailable with an
explanation; the motivating `gay_male` route must pass before release. A broad
retrieval alias and a narrow admission alias are different fields in the
artifact. Do not accidentally promote all four retrieval aliases into evidence.

For `gay-themed TV series`, map exact aliases to `gay_male`; do not resolve
`gay` to a title, cast member, adult flag, or the broad `lgbtq` theme. Distinct
themes may share retrieval keywords but have different admission rules.
Initially accept one required theme per request. More complex conjunctions
receive a clarification response rather than lossy interpretation.

Map supported prose such as `no teen dramas` to a visible `excludeTeenFocus`
filter; do not claim an embedding can enforce negation. Unsupported phrases
containing exclusions (`not`, `without`, `except`, `no`) produce
`needs_refinement`, with the unhandled span. Quoted seed names are not parsed
out of prose in v1: the user selects a real title using title search.

### 4.2 Evidence and the limits of metadata

| Evidence state | Meaning | Allowed display / eligibility |
|---|---|---|
| `central` | Reviewed rule finds an explicit synopsis statement connecting the main story to the requested theme | Eligible for Central theme only; show supporting text |
| `present` | A mapped keyword or explicit synopsis statement supports the theme, but centrality is unproven | Eligible for normal theme search; do not claim a central storyline |
| `unknown` | Evidence missing, incomplete, or only semantically similar | Excluded from required-theme results; may appear in seed-only similarity mode |
| `contradicted` | Explicit contrary evidence defeats the rule | Excluded from that theme; a model score cannot restore it |

Implement centrality conservatively with reviewed patterns, not a generic
substring check. `not a gay romance`, a named supporting character, and a
review saying the work lacks representation must not establish centrality.
When syntax falls outside the validated rules, return unknown centrality.
The system may under-retrieve; it must not claim certainty from absent data.

`excludeTeenFocus` requires affirmative evidence of adult focus from the
reviewed rules; unknown focus is excluded while this strict filter is on.
Its label/help text says “Adult-focused stories only; unclassified shows
excluded.” A production language or year filter also excludes unknown values.
Language means original language, not whether an English synopsis is available.

Reasons are templates over stored evidence: `Theme found in TMDB keywords`,
`Synopsis describes …`, or `Similar description to <seed>`. Include source,
field, exact supporting keyword/excerpt, and fetched time in expandable
details. Escape all provider text as plain text. Do not generate reasons with
a language model. Similarity is not a confidence percentage or a diagnosis
of which part of a description matched.

## 5. Retrieval — build a relevant, bounded candidate pool first

### 5.1 TMDB capabilities

Extend the adapter with these upstream operations. Extract a shared transport
object owning the HTTP client, credential snapshot/generation, limiter, and
provider cooldown. Recommendation calls use that transport with raw-response
caching disabled; only the bounded caches in §9 retain their data. Existing
title/discovery methods may retain their cache policy for this scope, but must
share the transport limiter/cooldown. Calling `tmdb.New` a second time unchanged
would create another unbounded cache and limiter and does not meet this plan.

| Need | Upstream operation | Bound |
|---|---|---|
| Resolve theme vocabulary | `/search/keyword?query=…` | At most four alias lookups on a cold query; accept only exact normalized allowlisted keyword names |
| Theme candidates | `/discover/tv?with_keywords=…` | Up to three pages total across approved keyword groups |
| Seed candidates | `/tv/{id}/recommendations`, then `/tv/{id}/similar` | Up to three pages total across both paths |
| Facts and evidence | `/tv/{id}` with supported appended `keywords,external_ids` | At most 60 unique candidate enrichments plus one seed lookup; no seasons or episodes |

Resolve keyword IDs from approved names rather than hardcoding unverified
numbers. Cache resolutions with their provider names. A failed keyword
lookup does not mean “no titles match.” Broad keyword retrieval may feed a
narrow theme, but the local evidence gate still applies.

Resolve aliases in artifact order. Successful lookups with no exact allowed
name are legitimate misses. If all lookups succeed but no keyword resolves,
return `needs_refinement` with `theme_unavailable`, not an empty successful
search. If some lookups fail but others resolve, use the resolved union and
mark coverage partial. If failures leave no keyword resolved, return 503
`provider_unavailable`. Never drop the required theme and use only the seed.
Join resolved IDs with `|` for OR; local admission implements the narrower
theme rule. A combined query uses the same theme group as theme-only search.

The documented TMDB TV discovery API supports keyword filters; related-TV
endpoints supply seed candidates. See the [TV discovery reference](https://developer.themoviedb.org/reference/discover-tv),
[keyword search](https://developer.themoviedb.org/reference/search-keyword),
[recommendations](https://developer.themoviedb.org/reference/tv-series-recommendations),
[similar series](https://developer.themoviedb.org/reference/tv-series-similar),
and [details](https://developer.themoviedb.org/reference/tv-series-details).
Verify response envelopes and append support with recorded fixtures at M1;
do not assume movie and TV keyword envelopes are identical.

### 5.2 Retrieval sequence and coverage

1. Validate/normalize the request; determine applied filters and unsupported
   text. Do this before provider or inference work.
2. Resolve an optional seed through verified external IDs. V1 accepts TMDB
   and TVDB series refs. TVDB-to-TMDB mapping must be uniquely verified;
   ambiguous, unavailable, or unmappable seeds receive distinct responses.
   Do not silently title-search for a replacement seed.
3. Retrieve candidate pages using the schedule below, then deduplicate by
   TMDB identity. Theme plus seed reserves 30 candidates for each path before
   filling unused capacity from the other path. Theme-only or seed-only may
   use all 60 places. Never fill with unrelated trending TV.
4. Enrich at most 60 candidates with a pool of four concurrent reads. Existing
   summary records may be shown only after the required filters can be
   evaluated. Failed enrichment cannot establish a required theme.
5. Apply hard filters, remove the seed, rank, and mark ownership from current
   library identities. Return up to 20 suggestions. Do not hydrate seasons
   to generate a recommendation card.

**Page schedule:** theme pages are 1, 2, 3 of its single OR-group discovery
query. Seed pages are recommendations 1, similar 1, recommendations 2; skip
pages beyond a provider-reported last page, without inventing another source.
For a combined request interleave theme 1, recommendations 1, theme 2, similar
1, theme 3, recommendations 2. Response arrival order cannot change the pool.
Assign identities found in both paths to the theme quota first, retaining
their rank contributions from both paths; fill the seed quota with remaining
seed identities, then fill unused slots from remaining theme and seed rows in
scheduled order. If either path fails, fill from the usable path while still
enforcing every selected filter and reporting partial coverage.

**Rank identity:** a retrieval list is the provider endpoint plus canonical
keyword group or seed ID and filters, excluding page. Recommendations and
similar are separate lists; different pages of either are one list. Compute
absolute rank using the v3 page-size contract, to verify as 20 in M1:
`(page - 1) * 20 + rowIndex + 1`, before deduplication or filtering.
Store each candidate's best absolute rank once per list. Repeated pages or
aliases cannot add another contribution. If provider paging changes, update
the adapter contract and fixtures instead of guessing rank from arrival order.

An arbitrary description with no recognized theme and no seed returns
`needs_refinement`: choose a theme or a title first. The model can rank prose
within an established pool, but v1 cannot search the entire world's catalog
semantically. Explain this boundary directly in the empty/input state.

No pagination cursor in v1. Return counts and a `coverage` indicator of
`bounded` or `partial`; “60 candidates checked” is not “all matching shows.”
Filters or a different seed start a new bounded search. Revisit paging only
if evaluation finds relevant results consistently beyond the candidate cap.

### 5.3 Deadlines, upstream limits, and partial answers

Set a 15-second end-to-end server deadline, including queue time. Allow at
most two active recommendation requests and four queued requests per process;
the encoder is serialized separately. Reject excess work with 429 and
`Retry-After: 2`. Candidate retrieval/enrichment gets at most 10 seconds of
the total budget; reserve time for ranking and the response. These are design
bounds to measure at M0/M5, not observed performance claims.

The cold maximum is 71 upstream requests: four keyword lookups, six candidate
pages, one seed lookup, and 60 enrichments. A TVDB mapping may consume the
seed lookup budget through a separate lookup path; if another detail call is
needed, reduce candidate enrichment allowance so the total remains 71.
Every request, including retries, counts. Use no automatic retry inside the
interactive deadline. Respect provider `Retry-After` and stop that source on
429; do not spin or make the other provider path bypass the same cooldown.

When some sources fail, return only candidates that pass all selected filters
and mark coverage partial. When all necessary retrieval fails and there is
no usable cache, return `provider_unavailable`, not an empty successful list.
When the model fails, metadata-only ordering may still return eligible
results with an explicit degraded state. Never relax filters on failure.

## 6. Ranking — evidence gates first, similarity second

```text
validated request → theme and/or seed retrieval → shallow facts
                                                   │
                        hard filters + theme evidence + seed exclusion
                                                   │
                        bounded local sentence embeddings (optional)
                                                   │
                        rank → fresh ownership → result wrappers
                                                   │
                                  existing preview / explicit Add
```

### 6.1 Ranking contract

Hard filters run before scoring. Required theme, centrality, language, year,
and adult-focus filters cannot be outweighed by popularity or similarity.
Build one embedding input from bounded theme keywords, genres, and synopsis;
put evidence-bearing fields before long plot text. Exclude cast demographics,
user identity, viewing history, folder paths, and release names.

Encode the description and seed separately. Proposed ranking v1:

- `Q`: cosine similarity to description, when it has nonempty ranking text.
- `S`: cosine similarity to the seed metadata, when present.
- Semantic score is `Q` or `S` when only one exists, otherwise
  `0.6 × Q + 0.4 × S`. This is a starting policy, subject to the evaluation
  gate, not an assertion that those weights are calibrated.
- Sort by evidence tier (`central`, then `present`) for theme searches,
  semantic score descending, then reciprocal provider rank, then TMDB ID.
  For seed-only searches, omit theme tiers. Reciprocal provider rank is
  `sum(1 / (60 + rank))`, with rank one-based within each retrieval list.
- In metadata-only mode, use the same evidence tiers followed by reciprocal
  provider rank and stable TMDB ID. Do not assign fabricated vectors or scores.
- If any eligible candidate fails embedding, use metadata-only ranking for
  the entire response. Mixing missing scores with real scores can quietly
  favor whichever item happened to fit in the time budget.

The normalized ranking text `Qtext` consists of the canonical active theme
description followed by residual positive prose, if any. Remove all parsed
filter spans from prose, including overridden/cleared themes and exclusions;
explicit filter values determine the applied semantics. Unhandled positive
prose remains similarity text. Unsupported negative spans require refinement.
A plain theme selection therefore has a meaningful Q without freeform input.
Apply the declared evidence/genre filters even when `Qtext` mentions them.

| Inputs after normalization | Semantic mode | Metadata-only mode |
|---|---|---|
| Theme only | Q from canonical theme text; evidence tiers then Q | Evidence tiers then provider rank |
| Prose + theme | Q from canonical theme plus residual prose | Same filters and evidence tiers; prose preferences cannot affect ordering, so show `prose_ranking_unavailable` |
| Seed only | S from seed facts; calibrated weak-match cutoff | Provider recommendations/similar ranks, no cosine cutoff; show `provider_suggestions_only` |
| Prose + seed, no theme | Q/S combination; calibrated weak-match cutoff | Provider ranks with `prose_ranking_unavailable` and `provider_suggestions_only` |
| Theme + seed, with optional prose | Q/S combination after theme admission | Evidence tiers then provider ranks; warn if prose preferences cannot affect ordering |

Seed facts are too thin for S when the normalized overview plus approved
keywords has fewer than eight lexical tokens, excluding title and genres.
If an active required theme supplies Q, rank by Q alone after theme admission,
with `seed_similarity_unavailable`. With no required theme, return metadata-only
provider suggestions even if residual prose exists; include
`seed_similarity_unavailable`, `provider_suggestions_only`, and, when prose
exists, `prose_ranking_unavailable`. This avoids an uncalibrated Q-only cutoff
for a thin seed. The eight-token threshold is an explicit initial policy to
evaluate at M0. Missing required seed identity is still an error, not a
thin-description case.

Return ranking mode and evidence labels, not raw cosine percentages. Use a
fixture-calibrated similarity cutoff for seed searches without a required
theme; calibrate S-only and combined-Q/S cases separately. The thresholds
must be recorded with the ranking version after M0 evaluation. Before that,
the plan makes no numeric quality guarantee from raw cosine. Theme-filtered
results remain supported by evidence even if their prose similarity is low.

### 6.2 Relevance evaluation before UI polish

Commit an evaluation corpus with query, seed identity where applicable,
candidate IDs, licensed/source-attributed evidence excerpts, and human labels
`relevant`, `related_but_wrong_theme`, `irrelevant`, or `insufficient_evidence`.
Use at least 20 queries and 100 distinct series, including unfamiliar titles,
gay male / broad LGBTQ+ distinctions, lesbian stories, adult versus teen
focus, regional remakes, missing keywords, negation, long synopses, and at
least three non-LGBTQ+ themes. Synthetic adversarial metadata complements
real-provider samples; it does not replace them.

In addition to those admission labels, independently grade each work's fit to
the full query as 0 (irrelevant/wrong theme), 1 (weakly related), 2 (relevant),
or 3 (particularly strong fit to the expressed preferences). nDCG uses gain
`2^grade - 1`, logarithmic discount `log2(rank + 1)`, the same held-out pool for
both orders, and deterministic ID tie-breaking. Report zero-ideal queries
separately rather than scoring them as perfect. Assemble known-relevant sets
independently of the retrieval output so a missing title remains a recall
failure. Report centrality-on subsets separately; empty results on a query
with known relevant works cannot pass a precision-only check.

Split tuning and held-out queries before adjusting ranking. Do not label with
the same rules or model being evaluated. Record disagreements and have the
owner or reviewer adjudicate thematic/centrality labels before release.

Measure retrieval recall separately from ranking: of known relevant titles
for each query, how many reached the candidate pool? A reranker cannot repair
low recall. Compare keyword/provider ordering against semantic ordering on
the identical pool. Release targets, subject to explicit review if unmet:

- At least 80% recall against the annotated relevant set across held-out theme
  queries, with per-query counts reported; this is not global catalog recall.
- Mean precision@10 at least 0.8 on held-out queries with at least ten labeled
  relevant works. Smaller sets report precision at available results and
  omissions separately; returning one good item does not score as ten.
- No known wrong-theme result in the first ten for the motivating gay-themed
  queries, and no hard-filter violation in any fixture.
- Semantic ranking improves mean nDCG@10 by at least 0.03 over the baseline,
  without reducing motivating-query precision. If it does not, revisit the
  model/ranking; do not call an ineffective AI toggle complete.
- Every displayed thematic claim has traceable supporting evidence; central
  claims have adjudicated evidence or remain unknown.

M0 records exact grading, tie handling, cutoff selection, dataset version,
hardware, and results. These criteria are proposed acceptance targets, not
measurements already performed.

## 7. Model runtime — local, optional, and separately proven

### 7.1 Proposed implementation decision

Use the same pinned MiniLM model as the initial candidate. The
[model card](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2)
describes a 384-dimensional sentence encoder with an English focus and
256-wordpiece truncation. It does not establish accuracy for this task.

**Preferred packaging:** a small companion executable, `curator-embed`,
supervised by the Go process over stdin/stdout. Implement only sentence
encoding in Rust with [Candle](https://github.com/huggingface/candle), drawing
on Cinema's proven encoder shape. Keep the Curator Go binary free of CGO.
Ship the helper in the release archive/container so users do not install a
second service or run Cinema. No listening port, browser model, Python runtime,
Ollama dependency, or arbitrary remote-model URL in v1.

This introduces a second build toolchain and artifact. It is a deliberate
trade-off, contingent on M0 proving packaging and runtime budgets on the
supported targets. Do not silently replace it with a hosted API, change the
base image, or drop a supported architecture if the spike fails; revise this
decision with the measured evidence first. Confirm the release target matrix
from the actual workflow at M0 rather than assuming only this Mac matters.

### 7.2 Process and encoding contract

Proposed adapter port:

```go
type SentenceEncoder interface {
    ModelID() string
    Encode(ctx context.Context, texts []string) ([][]float32, error)
    Close() error
}
```

`Encode` returns exactly one finite, L2-normalized 384-vector per input, in
order; model ID includes weights, tokenizer, pooling and text-format version.
The helper handles one JSON-line request at a time, with request ID, protocol
version, model ID, and at most eight texts per batch, each at most 8 KiB.
Stdout is protocol only; stderr is bounded diagnostics without input text.
Limit each output line to 256 KiB. Reject mismatched request IDs, model IDs,
vector counts, dimensions, NaN/Inf, and invalid norms. No partial batch reuse.

Encode texts individually without padding, or prove attention-mask-correct
pooling if batching tensors. Pin tokenizer truncation, special tokens,
mean-pooling, and normalization with reference-vector fixtures. An embedding
implementation with the same model name but different pooling is not an
equivalent cache producer. Default inference is one active batch and two CPU
threads. Cancellation or deadline stops work; kill/reap an unresponsive
helper, fail its batch, and restart only on the next explicit request, with a
30-second cooldown after a crash. Cap pending batches through the service's
global request admission, not an unbounded helper queue.

M0 targets: helper RSS at most 512 MiB; cached-result response p95 under one
second; warm-model, cold-candidate search p95 under eight seconds on an agreed
two-core CPU host. Model download is excluded and displayed separately.
Measure CPU contention with ordinary title search and an active library task,
both for a single cold search and two concurrent cold searches. Report combined
Go/helper/container peak memory separately from helper RSS, cache occupancy,
queue time, and partial-response rate. A single fast search is not proof of the
two-active-request budget under the shared 10 requests/second limiter.
Failure to meet targets is a design finding, not permission to raise them
silently.

### 7.3 Installation, enablement, and lifecycle

Add Settings → Discovery → “Use local semantic ranking”, default off. Theme
retrieval and evidence-based ordering work with it off. Display model state
`disabled`, `not_installed`, `downloading`, `loading`, `ready`, or `failed`.
Saving the toggle records intent even while the model is unavailable; show
why it is not active instead of rejecting the setting.

The explicit Enable and download action fetches model data only, never
executable code. Pin revision
`1110a243fdf4706b3f48f1d95db1a4f5529b4d41` as the initial candidate, validate
config/tokenizer/weights sizes and SHA-256 from a committed manifest, and
verify attribution/license when packaging. Cinema's inspected weights are
90,868,376 bytes; UI size includes all files. Install under
`MONARR_DATA_DIR/models/<manifest-digest>/` through temporary files and atomic
rename after verification. Reject oversized, truncated, or corrupt artifacts.
Use a fixed manifest URL set with HTTPS and a bounded redirect allowlist;
no user-controlled fetch URL. Permit pre-seeding those exact files offline.

One install runs at a time with a 10-minute deadline and a 200 MiB temporary
disk budget. Persist enablement, not an in-progress download. On restart,
clean incomplete temp files; report not installed and expose Retry. Do not
repeatedly download on page open or boot. Disabling cancels downloads and
inference, reaps the helper, releases RAM, and retains verified files for
reuse. Uninstall/clear-cache is a separate explicit operation with the model
disabled; it touches only the owned model/cache directory.

## 8. Server contract — recommendation wrappers preserve Add identity

### 8.1 API and types

Add `POST /api/v1/metadata/recommendations` with normal native-API auth and a
16 KiB body limit. Discovery reads external data and disposable caches but
never creates media items. POST avoids putting descriptive searches in URLs.

```json
{
  "kind": "series",
  "query": "gay-themed TV series",
  "seed": { "provider": "tmdb", "id": "12345" },
  "filters": {
    "theme": "gay_male",
    "centralThemeOnly": false,
    "genres": [],
    "originalLanguage": null,
    "yearFrom": null,
    "yearTo": null,
    "excludeTeenFocus": false,
    "hideInLibrary": true
  },
  "limit": 20
}
```

The seed ID above is illustrative, not a title recommendation. `seed` and
`query` are independently optional but at least one nonempty query, theme,
or seed is required. Accept only `kind=series`; query is at most 500 Unicode
characters, seed IDs positive decimal integers, limit 1–20, genres at most
three validated TV genre IDs, original language a supported provider code,
and year range 1900 through current year + 5 with from ≤ to. Reject unknown
filter keys/enums rather than silently ignore a constraint. Explicit filters
override inferred dimensions; represent a deliberately removed theme as
`theme: null`, distinct from omitted/infer-from-query.

Request normalization is server-owned and versioned. Defaults and composition:

| Field | Omitted | Explicit value / null |
|---|---|---|
| `kind` | Invalid; required | Only `series` |
| `query` | Empty | Trim Unicode whitespace; null invalid |
| `seed` | None | Positive TMDB/TVDB ref; null clears it |
| `filters` | Infer supported spans, then use these defaults | Object only; null invalid |
| `theme` | Infer one supported theme, else none | Valid theme replaces inference; null means no theme |
| `genres` | Infer supported named TV genres, else no filter | Array uses OR; empty clears; null invalid |
| `originalLanguage` | No filter | Verified original-language code; null clears |
| `yearFrom`, `yearTo` | No bound | Inclusive integer bound; null clears that bound |
| `centralThemeOnly` | false | Boolean; true without an active theme is 400 |
| `excludeTeenFocus` | Infer supported exclusion, else false | Boolean overrides inference; null invalid |
| `hideInLibrary` | true | Boolean; false shows owned works; null invalid |
| `limit` | 20 | Integer 1–20; null invalid |

Different dimensions are ANDed. Multiple genres are ORed both in the upstream
query and locally. Year is first-air year. Only theme, genre, and the documented
teen-focus phrase are inferred in v1; language/year/centrality use explicit
controls. The web clears Central theme only when removing its theme and sends
both changes; malformed direct API combinations receive 400. Server returns
the authoritative applied values and `rankingText`, so a future client needs
no duplicate interpretation rules. English interpretation is the v1 contract;
other-language prose may require refinement and is not promised equal recall.

Response envelope:

```text
state: ready | needs_refinement
applied: normalized query, rankingText, seed, filters, interpretationVersion
unhandled: text spans and actionable reasons, if any
ranking: semantic | metadata_only
modelState: disabled | not_installed | downloading | loading | ready | failed
coverage: bounded | partial
retrievedCount, checkedCount, eligibleCount, hiddenOwnedCount, returnedCount
warnings: [{code, message}]
results: [{
  key: "series:tmdb:<id>",
  item: existing SearchResult,
  ownership: absent | present | ambiguous | unknown,
  libraryItemId?: integer,
  addability: supported | unsupported | conflict,
  themeEvidence: central | present | unknown | contradicted,
  reasons: [{code, source, field, value, fetchedAt}],
  fetchedAt: RFC3339 timestamp
}]
```

`needs_refinement` returns no results and performs no inference; the client
shows suggested corrections. No silent title-search or trending fallback.
Counts refer to unique identities, not upstream row counts. `checkedCount`
counts candidates whose required evidence was evaluated; failed enrichment
is separately represented by a warning and partial coverage.

Add `GET /api/v1/metadata/recommendations/status` for feature/model/provider
readiness; it does no provider or model work. Extend the existing Settings
GET/PUT with `semanticRankingEnabled` and persist as
`discovery_semantic_enabled` in `app_meta`. Add explicit model operations
`POST /api/v1/metadata/recommendations/model/install` (202, idempotent while
running) and `DELETE /api/v1/metadata/recommendations/model` (409 while enabled).
They use the same authorization policy as current settings writes; Curator
does not acquire a fictional administrator role in this feature.

| Outcome | HTTP / state | UI meaning |
|---|---|---|
| Unsupported interpretation | 200 `needs_refinement` | Correct the visible filters/text |
| No eligible candidates | 200 `ready`, empty results | No matches in checked candidates; suggest explicit refinements |
| Partial provider failure | 200 `ready`, `coverage=partial` | Show eligible results plus limitation |
| Encoder unavailable | 200, `ranking=metadata_only`, warning | Theme filters still apply; ordering is degraded |
| Malformed request | 400 `invalid_recommendation_request` | Show field error |
| Seed not found | 404 `seed_not_found` | Select a different verified title |
| Seed ID conflict | 409 `identity_conflict` | Resolve identity; do not substitute another title |
| Seed cannot map to TMDB | 422 `unsupported_seed` | Existing title preview/Add still work |
| Missing key or necessary source unavailable | 503 `provider_unavailable` | Explain setup/unavailability; do not report no matches |
| Provider settings changed during work | 503 `recommendation_state_changed` | Discard old-generation results; user can retry |
| Admission full | 429 `recommendations_busy` | Retry-After 2 seconds, user retry |

Use the existing structured API Error shape with code/message, not a new
error wrapper. A provider cooldown surfaces a warning when another usable
source exists, otherwise 503 with a safe Retry-After value. A deadline with
usable eligible candidates returns partial results; without them return 503.

### 8.2 Application and port boundaries

Create `internal/app/recommendation.Service` to orchestrate interpretation,
candidate retrieval, evidence, optional encoding, ranking, and ownership.
Keep API DTOs out of domain and ports. Add narrow ports for the following:

```go
type RecommendationSource interface {
    ResolveKeywords(ctx context.Context, aliases []string) ([]Keyword, error)
    Candidates(ctx context.Context, request CandidateRequest) (CandidatePage, error)
    Facts(ctx context.Context, tmdbID int64) (SeriesFacts, error)
    ResolveSeed(ctx context.Context, ref domain.ExternalRef) (SeriesFacts, error)
}

type RecommendationOwnership interface {
    LookupSeries(ctx context.Context, ids []domain.ExternalIDs) ([]Ownership, error)
}
```

`Keyword` carries provider ID/name; `CandidateRequest` carries source path,
resolved keyword IDs or seed ID, filters and page; `CandidatePage` carries
provider ranks and immutable `SearchResult` values. `SeriesFacts` carries
verified IDs, source, original language/year, genres, overview, keywords,
fetched time and explicit missing-field state. `Ownership` is input-aligned
and carries the four preview ownership states plus an optional local ID.
Define these types in ports using domain and stdlib only. The TMDB adapter
implements retrieval; SQLite implements batched ownership. Respect
[the architecture test](../internal/arch_test.go).

### 8.3 Identity and Add invariants

Results use `series:tmdb:<id>` as wrapper key, while their `item` remains a
stable existing SearchResult. Facts enrichment must verify the requested
TMDB ID before producing a card and preserve `HydrationSource=tmdb` for
TMDB candidates. Metadata and preview enrichment cannot change the original
Add selection object or its pending/added key.

Use all verified TMDB/TVDB/IMDb IDs for ownership. Match one library item with
no conflicting namespace → present; no match after a successful lookup →
absent; multiple/conflicting identities → ambiguous; failed DB lookup →
unknown. Reuse/extract the preview conflict rules, not title/year matching.
Do not call `Library.List` and grade every upgrade for each recommendation.

Hide in library removes only known-present results. Unknown and ambiguous
rows remain visible with their state explained. Block Add on ambiguous or
conflicting identity. Unknown ownership permits the existing Add validation
to decide; do not claim the row is absent. Open in library requires a real,
unambiguous local ID. For compatibility, `item.inLibrary` is true only for
present, but discovery UI must consult the richer wrapper state.

Add receives the original item and existing options. Server Add remains the
final authority if ownership changed after search. On `already_exists`,
refresh ownership/preview rather than showing a second copy as added. On
success, update every matching visible wrapper, the preview, and current
added-items strip; invalidate existing library/wanted queries. If Hide in
library is on, remove the newly owned card after announcing success without
losing focus; focus the next result or the results heading.

## 9. Caches and operations — bounded, disposable, no catalog database

Use bounded in-memory LRU caches for v1; no new SQLite schema for suggestions,
vectors, preferences, or search history. `app_meta` holds the opt-in setting.
Models live on disk separately. Rebuilding small hot candidate pools after a
restart is accepted; revisit persistence only with measured repeated cost.

| Cache | Key | Fresh TTL / cap |
|---|---|---|
| Keyword resolution | Provider config generation, normalized alias set, rules version | 7 days; 256 entries |
| Candidate pages | Provider config generation, normalized retrieval arguments, language, page | 30 minutes; 256 entries |
| Series facts | Provider config generation, TMDB ID, metadata language | 24 hours; 2,000 entries |
| Vectors | Model+tokenizer+text-format digest and exact normalized text digest | LRU; 2,000 entries; invalidate with source/model changes |
| Ranked candidate lists | Provider generation, encoder generation, effective rank mode, applied query/filter/seed snapshot, rules/ranking/model versions | At most 5 minutes, bounded by source expiry; 128 entries; no ownership attached |

Cap aggregate recommendation cache memory at 64 MiB and bound each stored
overview/keyword list. Cache eviction is safe at every stage. Do not retain
raw queries longer than the five-minute in-memory response cache. Hash keys
do not anonymize the retained metadata; keep them out of logs as well.
Provider-key changes increment a configuration generation so old data cannot
mask missing/changed credentials. Check that the key is configured before
cache use; that does not validate the credential remotely. A changed key
requires a cold fetch before new-generation provider data can be cached.

The 64 MiB cap includes all retained recommendation cache layers, including
decoded facts and any raw bodies. The extracted recommendation transport
does not insert bodies into the old TMDB map. Response bodies are temporary
and size-bounded; cap the aggregate transient bytes separately through the
four-read concurrency bound. A 64 MiB cache limit is not a process-RSS limit.
Ordinary metadata's existing cache is outside this feature cap; measure it
as part of total process memory without claiming this project fixes it.

Capture provider configuration and encoder-state generations on admission.
Provider key changes invalidate every recommendation layer, not just facts;
discard any in-flight old-generation response before caching or publishing
and return `recommendation_state_changed`. Shared transport uses the request's
credential snapshot, not a second late read that could mix generations.

Increment encoder generation on enable/disable, ready/nonready transitions,
model changes, and failure/recovery. Store actual ranking mode with the cache
entry. Disabled/nonready state cannot use a cached semantic order; newly ready
state cannot use a metadata-only ranked entry. If the encoder changes during
work, keep same-provider eligible facts but recompute metadata-only ordering,
emit `ranking_state_changed`, and do not cache that response. The next request
may use the new ready generation. Generation checks apply again immediately
before publishing a cache entry or response.

Each ranked entry records the source fingerprints and earliest source expiry
for its keyword resolution, pages, seed facts, and candidate facts. Its expiry
is `min(created + 5 minutes, every source expiry)`. Source replacement
invalidates dependents immediately; TTL is not extended by copying a record
into another cache. Model changes invalidate vectors by their digest. These
rules are required even if the initial implementation uses simple whole-cache
invalidation rather than a dependency index.

Before publishing, recheck expiry as well as generation. Discard expired
candidate facts and recompute ranking/counts with partial coverage; expired
required keyword/seed/list dependencies require revalidation within the same
deadline or a provider-unavailable response. No response silently extends
freshness because its request began before a source expired.

Do not serve expired provider data on error in v1. A cache hit within TTL is
valid; report its fetched time. This intentionally differs from Discover's
stale-row policy because precise theme evidence must be inspectable. Refresh
ownership on every response, including ranked-cache hits, and apply Hide in
library after that refresh. Never cache the ownership-filtered result list.

Deduplicate concurrent identical retrieval/encoding work. Bound shared work
by its own deadline; one disconnected caller must not cancel another caller's
result, and zero waiters should cancel the work. Release admission slots on
all exits. Do not install a scheduler job or crawl in the background.

Expose aggregate durations by stage, candidate counts, cache hit/miss counts,
model readiness/failures, queue rejections, and provider outcomes with fixed
labels. Never use query text, theme, seed ID, title, or provider URLs as metric
labels. Settings status should distinguish unsupported helper platform,
missing model, corrupt model, and unavailable provider.

## 10. Milestones — prove retrieval and runtime before broad UI work

| Milestone | Work and primary files | Acceptance |
|---|---|---|
| M0 — feasibility and baseline | Record current main SHA; inspect seams; build tiny `tools/curator-embed/` spike with locked Rust dependencies; assemble held-out corpus and baseline evaluator | Real model loads on release targets; vector parity, packaging, runtime budgets, retrieval/quality baseline recorded; resolve the seed-only cutoff and ranking policy before M3 |
| M1 — retrieval and evidence | `internal/ports/recommendation.go`, `internal/domain/recommendation/`, TMDB adapter methods/fixtures, shared bounded budgets | Theme query retrieves titles without title-term overlap; strict evidence excludes adversarial wrong-theme cases; no season requests; bounded calls and 429 handling proven |
| M2 — service, identity and API | `internal/app/recommendation/`, SQLite batched ownership, API handler/OpenAPI, wiring in `cmd/monarr/main.go` | Metadata-only API passes contract, identity, cancellation, cache and partial-failure tests; no library writes from discovery |
| M3 — local ranking and settings | `internal/adapters/embedding/`, helper protocol/build, model manifest/install, Settings API, runtime diagnostics | Real encoder improves held-out ranking; disabled/corrupt/crashed/timeout states preserve metadata-only results; process cleanup and bounded memory verified |
| M4 — web experience | AddMedia, existing preview drawer, API types, route state, Settings UI, styles | Describe → refine → More like this → preview → Add works on desktop and 390 px; filters survive seed changes; original Add identity and defaults preserved |
| M5 — qualification and handoff | E2E fixtures, release/container packaging, usage/settings/deployment docs | Quality and resource targets met; current-main checks green; observed limitations and actual target support recorded |

M0 is a decision gate, not a feature release. If the helper or model does not
earn its cost, retain the evaluation and return a revised design for review.
M1/M2 may remain useful implementation groundwork, but metadata-only discovery
does not satisfy the accepted full feature on its own.

### 10.1 Regression matrix

| Area | Cases that must fail safely or retain behavior |
|---|---|
| Meaning | Gay male vs broader LGBTQ+ vs lesbian; central vs supporting; negated synopsis; historical use of “gay”; missing keywords; unsupported exclusions; cleared inferred chips |
| Retrieval | Known relevant title never enters pool; theme+seed path starvation; keywords resolve to unrelated fuzzy matches; empty vs failed provider; partial keyword lookup; page/request cap |
| Identity | Same title/year different IDs; seed alias; conflicting external IDs; TVDB-only seed mapping; already-owned across namespaces; DB read failure; add from stale cache |
| Ranking | Fake vectors cannot admit a wrong-theme title; deterministic ties; invalid vectors; one failed batch makes whole response metadata-only; metadata changes invalidate vectors |
| Resource behavior | Cancel during provider call and inference; shared work with two callers; queue full; helper crash/hang; corrupt download; disable during install; restart with partial files |
| UI | New request beats old response; seed retains theme; language filter semantics; preview Back/focus; More like this does not Add; Add uses original result; duplicate conflict; unknown ownership remains visible |
| Packaging | Non-root read/write permissions; no helper/model supported state; actual architecture binary; no missing runtime libraries in final image; normal Go-only build still succeeds |
| Compatibility | Exact-ID/title search, Discover lists, preview, acquisition, and existing mobile API consumers retain their contracts |

Use synthetic provider fixtures for deterministic CI and a separately run,
attributed real-model evaluation. Routine unit tests must not need TMDB keys,
network access, or model downloads. Reserve actual provider/model smoke checks
for explicit qualification; store results, not credentials or private queries.

### 10.2 Commands and evidence

At implementation time, run focused package checks after each milestone.
These proposed package paths become runnable when created:

```bash
go test ./internal/domain/recommendation/... ./internal/app/recommendation/...
go test ./internal/adapters/tmdb/... ./internal/adapters/embedding/...
go test ./internal/api/... ./internal/infra/sqlite/... ./internal -run 'Recommend|Preview|Identity|Dependency'
make gen-api                         # Regenerate after schema edits.
npm --prefix web run typecheck        # Validate handwritten web API/types.
make test-web                        # UI logic and contracts.
make test-mobile                     # Existing client compatibility.
```

Before release, use the repository's actual current-main gates, including
`make lint`, `make test`, `make test-web`, `make test-mobile`, and
`make test-e2e`. The inspected tests workflow additionally runs `make gen` with
a generated-code diff check, `go vet`, race-enabled Go tests with the coverage
ratchet, and Go/web builds; Docker CI builds the final image on Linux x64.
No multi-architecture release matrix was established by that workflow. M0
must inventory actual distributed archives/containers and intended development
targets, then record the helper matrix without inventing platform support.
Inspect the current workflows for changes to those requirements. M0 must add
and document locked helper build/test and real-model evaluation targets;
commands that do not yet exist are not claimed as executed evidence here.
Run the focused recommendation E2E during development and the full required
checks once on the final candidate. Do not broaden repeated tests without a
change, failure, or unresolved risk.

## 11. Decisions for review and release handoff

1. **TV first, web first:** proves the motivating case while preserving the
   existing native contract. Native UI and other media kinds get explicit
   follow-up milestones after quality is established.
2. **Retrieve externally, rank locally:** covers unowned titles and keeps
   inference independent of cloud subscriptions. Coverage remains limited by
   provider metadata and the bounded pool; surface that limitation.
3. **Evidence gates before embeddings:** similarity cannot override the
   requested theme. The accepted cost is fewer results when metadata is thin.
4. **Companion encoder rather than CGO or a Cinema dependency:** preserves the
   Go build and independent operation, at the cost of a second build artifact.
   M0 must establish whether that cost is practical.
5. **No persistent recommendation store in v1:** simple invalidation and no
   search-history database, at the cost of recomputation after restart.
6. **Finite interpretation:** explicit filters handle supported constraints;
   arbitrary prose needs a theme or seed. If users need broad conversational
   discovery, revisit a query-planning model separately with its own tests.

Fable should challenge the retrieval recall, evidence/centrality rules,
helper packaging, and workload bounds before approving the implementation.
Do not spend review time selecting a larger model without proving why the
small model and candidate retrieval miss the acceptance cases.

Release evidence must include: exact code/model/data versions; provider
fixture provenance; held-out relevance comparison; cold/warm latency and peak
RSS on named hardware; supported packaging targets; failure-state captures;
and the regression commands/results. Update [usage](usage.md),
[settings](settings.md), and [deployment](deployment.md) in the same behavior
change. Rollback is disabling semantic ranking and reverting the additive
feature; no media-library schema rollback is required.
