# Smart media discovery review — challenge the build contract before Fable

**Reviewed:** 2026-09-29. **Initial verdict:** request changes to the proposed build
contract; proceed with the bounded M0 feasibility work after those changes.
This is a documentation review, not approval of an implemented feature.

**Current disposition:** Fable subsequently approved **M0 only, with changes**;
M1–M5 are not approved to build. The author addressed that review in the plan;
see §6. The earlier independent review and its verification remain historical
evidence in §§1–5, not approval of the later build. No M0 experiment has run.

Companion to [the build plan](plan-smart-media-discovery.md). This independent
adversarial pass preserves the original findings so the author and Fable can
distinguish corrections from decisions still requiring evidence. The product
direction is coherent: retrieve candidates externally, enforce evidence-based
filters, rank locally, and preserve the existing Add flow. The principal
problems are cache layers that escape the promised bounds, lifecycle changes
that can leave cached ranking behavior stale, and missing edge contracts.

## 1. Evidence — the proposed text and the fetched integration baseline

The reviewed plan was the 756-line draft with SHA-256
`3c08ee9ca1108c8504a0cdf5eaf9021ea66411badaea39fb6e667434d1dfff1c`.
Later author amendments do not change what this initial verdict evaluated.

Code was read with `git show` at locally fetched `origin/main`,
`10d5a9672ea1bf19476bfb62c5df8034f65324ad`, rather than inferred from the older
dirty working checkout. Inspected source included:

- `internal/adapters/tmdb/client.go` and `discover.go`: transport, cache,
  limiter, shallow records, external identity resolution, and readiness.
- `internal/ports/metadata.go`: SearchResult and existing provider ports.
- `internal/app/library/preview.go`: ownership and identity-conflict policy.
- `web/src/api.ts` and relevant AddMedia identity/mutation declarations.
- `deploy/Dockerfile` and `.github/workflows/docker.yml`: current container
  assumptions and the CI build target.

Line references to those files below refer to that exact commit. No remote
fetch, live TMDB request, model inference, package build, feature tests, or
Cinema implementation verification was performed in this review. The plan's
Cinema observations remain author-supplied evidence. The baseline is not
claimed to be the latest remote main. No feature code was changed.

## 2. Required corrections — close the contracts before implementation

### F1 · P1 — the underlying TMDB cache defeats the feature's resource and credential boundaries

**Plan sections:** §2.2, §5.1, §9.

**Evidence:** `tmdb.Client` owns `cache map[string]cacheEntry` at
`internal/adapters/tmdb/client.go:40`. `get` sets `cacheKey := full` at line
93 and inserts successful responses at line 139. There is no entry cap or
expired-entry eviction. With bearer credentials, the token is placed in a
header, so it is absent from that URL-derived key. `Configured` in
`discover.go:122` checks that a key exists, not that it is valid.

**Failure scenario:** discovery searches touch thousands of distinct series.
The new 2,000-entry facts cache evicts correctly, but every upstream response
remains in the old map, defeating the aggregate 64 MiB claim. Separately, an
operator replaces bearer token A with token B. The feature invalidates its
own cache, but `get` can still return A's cached response for the same URL.
The promised credential-change fence is therefore ineffective even when all
new feature caches are implemented exactly as specified.

**Required fix:** specify the complete cache path, including the existing
adapter. A dedicated recommendation adapter using extracted transport with
its response cache disabled is sufficient if it uses the shared HTTP client,
limiter, cooldown, and credential generation. Injecting a bounded,
generation-aware cache policy is another valid solution. Instantiating the
current `tmdb.New` unchanged is insufficient: it creates another unbounded
cache and an independent limiter. This need not become a broad refactor of
ordinary title search's caching policy.

**Acceptance:** cold recommendation traffic cannot populate an unaccounted
raw-response cache; expiry/eviction releases retained entries. After changing
between two bearer credentials, a same-URL recommendation cannot return a
prior-generation cached response. An in-flight old-generation response must
not repopulate the current-generation cache.

### F2 · P1 — ranked-cache identity omits provider generation and effective model state

**Plan sections:** §7.3, §8.1, §9.

The ranked-list key contains query/filter/seed and rules/ranking/model versions,
but omits provider configuration generation, effective ranking mode, and
metadata freshness dependencies. Model version alone does not encode whether
the helper is enabled, loading, failed, or recovered.

**Failure scenario:** a semantic result is cached, then the user disables the
model. A repeat request can reuse semantic ordering while reporting disabled
status. Conversely, metadata-only results cached during a model failure can
remain in place after recovery. A key change invalidating only the lower
provider caches can leave the ranked list intact. A list assembled just before
source metadata expires can also outlive its source TTL while §9 promises no
expired provider data.

**Required fix:** carry provider generation and an encoder-state generation
through request snapshots and ranked-cache keys, or explicitly invalidate
and fence the same transitions. Store the actual ranking mode with each
entry. Define cache expiry as no later than the minimum applicable source
expiry, or explicitly change the freshness promise. Fence admission and
publication so disable or provider changes during a request cannot publish
results under the new generation. Ownership still needs its existing fresh
lookup on every response.

**Acceptance:** cache → disable → repeat produces metadata-only behavior;
failure → cache → recovery can produce semantic behavior; key rotation
cannot reuse prior-generation ranked results; provider expiry cannot be
extended merely by wrapping facts in a newer ranked-cache entry.

### F3 · P2 — valid requests can have no semantic score, and degraded seed results have no cutoff policy

**Plan sections:** §6.1, §8.1, §10 M0/M2.

The API accepts a theme with neither query nor seed. Q is defined only for
nonempty ranking text, and S only for a seed, leaving that valid request
without either score. Separately, seed-only results require a calibrated
similarity cutoff, yet model-disabled and failed-model requests must also
return metadata-only ordering. A cosine cutoff cannot run in that mode.

**Failure scenario:** the user selects the Gay male stories chip and submits
without prose. One implementation embeds the theme label, another creates
zero scores, and a third reports an encoder failure. After a model crash,
seed-only search either admits all provider suggestions or returns nothing,
depending on an undocumented builder choice.

**Required fix:** publish a small decision table for theme-only, prose+theme,
seed-only, and prose+seed inputs under semantic and metadata-only operation.
Choose canonical theme text as Q, or deliberately select metadata-only ranking
when no ranking text exists. For seed-only degradation, explicitly choose
provider-ranked recommendations with a displayed limitation, a separately
validated metadata gate, or an unavailable result. Do not silently apply a
semantic cutoff where no semantic score exists. Define behavior when seed
metadata is too thin to embed meaningfully.

**Acceptance:** table-driven tests cover every input/mode combination,
including model recovery, with defined eligibility, ordering, ranking label,
and warning behavior. M0 records the chosen seed cutoff together with its
degraded-mode policy.

### F4 · P2 — filter composition and interpretation authority need an exact contract

**Plan sections:** §3.2, §4.1, §8.1.

The contract allows up to three genre IDs but does not specify AND versus OR.
It allows `centralThemeOnly=true` when the required theme has been removed,
without specifying rejection or interpretation. Defaults for omitted limit
and filters are not enumerated. “Parse known phrases locally” can also mean a
browser rule engine, while the plan places the rules in a Go domain package
and expects the API's applied interpretation to be authoritative.

**Failure scenario:** the browser displays Drama and Comedy while upstream
TMDB discovery uses AND and local checks use OR. A theme is removed but a
centrality requirement remains, so one client shows results and another shows
none. A future native client interprets the same prose differently from web.

**Required fix:** define genre semantics, omitted/null/empty/false defaults,
and invalid filter combinations in a request-normalization table. Make the
server the interpretation authority; a browser may display provisional chips,
but must reconcile to the returned applied snapshot. If client parsing is
required, specify the shared versioned artifact and parity checks. Define
whether explicit filter overrides also remove contradictory inferred terms
from the embedding query, so clearing a theme has predictable ranking effects.

**Acceptance:** identical requests normalize identically across clients;
multi-genre filtering agrees upstream and locally; removing a theme with
centrality selected has one documented outcome; omitted and explicitly false
filters do not collapse into the same inference instruction accidentally.

### F5 · P2 — keyword groups and provider-rank aggregation lack deterministic rules

**Plan sections:** §4.1, §5.1–§5.2, §6.1, §8.2.

The plan names a reviewed vocabulary and approved keyword groups but supplies
neither their initial contents nor a precise requirement for producing them
at M0. Retrieval combines multiple keyword groups, recommendation pages, and
similar pages; reciprocal rank is stated as rank “within each retrieval
list,” without defining whether pages and keyword groups are separate lists.

**Failure scenario:** a builder uses OR for broad retrieval keywords and
another uses AND, producing very different recall. An alias has no exact
keyword match, leaving a declared theme with no retrieval route. Page two's
first result receives rank 1 instead of rank 21, or duplicate appearances in
the same source are counted twice. Rankings and page allocation change with
implementation order even though each follows the prose.

**Required fix:** make the initial vocabulary artifact an explicit M0/M1
deliverable, containing each theme's retrieval keyword groups, boolean
composition, local evidence rules, and zero/partial-resolution policy. Define
a retrieval-list identity, absolute rank across pages, and one contribution
per candidate per list before summing reciprocal ranks. Specify deterministic
page allocation when several groups compete for three theme pages. Document
which retained path slot owns a candidate found in both theme and seed paths.

**Acceptance:** fixtures prove the exact outbound keyword operators and page
order; duplicate pages cannot improve a candidate's score; page-two ranks
continue page one; partial/no keyword resolution has a distinct, documented
coverage or error outcome. Every exposed theme has a verified retrieval path
before its chip is enabled.

## 3. Empirical gates — retain these as evidence requirements, not claimed defects

These concerns are deliberately unresolved by the proposal's M0 gate. They
are not reasons to demand a larger model or redesign the whole feature now.

| Gate | Adversarial question and required evidence |
|---|---|
| Theme retrieval and centrality | Can the finite rules achieve the stated recall and zero wrong-theme top-ten target together? Report per-theme and centrality-on results. Include real keyword sparsity and adversarial negation. An empty strict list must not pass quality merely because it has no false positives. |
| Evaluation validity | Define relevance grades and nDCG gains before tuning. If all evidence-eligible results receive the same binary relevant label, a 0.03 ranking lift may be impossible to measure despite useful within-theme ordering. Include graded suitability for stated query preferences and an independently assembled known-relevant set to expose retrieval misses. |
| Runtime targets | The inspected workflow builds a Linux image on x64; it does not demonstrate a multi-architecture release matrix. Record what distributions actually exist, then test the helper in the final distroless image and the intended local-development target. Neither all-platform support nor unsupported architectures should be invented. |
| Concurrency and latency | Existing TMDB limiting is 10 requests/second with burst 10. Two cold searches approaching 71 requests each compete with ordinary metadata traffic, so one-request latency does not establish the two-active-request budget. Measure partial results, title-search contention, and queue deadlines on named hardware. |
| Inference versus retrieval cost | The 512 MiB helper target excludes the Go process and its caches. Measure combined process/container peak memory as well, and report the helper-only number accurately. Full model availability does not repair missing candidates. |
| Real-model parity | Pin the actual committed manifest, tokenizer/pooling fixtures, source attribution, and executable build dependencies. A Cinema model-name match is not proof of numerical parity or license-complete packaging. |

## 4. Remaining decisions — make Fable's next review concrete

Before treating the plan as ready to build beyond M0:

1. **Choose the adapter cache boundary** from F1 and document which shared
   limiter/cooldown serves ordinary metadata and recommendations. Prefer the
   smallest seam that makes new traffic bounded without broad unrelated work.
2. **Choose the fallback ranking matrix** from F3. The response must describe
   actual behavior, particularly when local ranking was enabled but cannot run.
3. **Commit the interpretation/retrieval contract** from F4/F5, including which
   decisions are fixed now and which require M0 evidence before M1/M3.
4. **Have Fable challenge empirical feasibility:** vocabulary recall versus
   evidence precision, the evaluation label/ranking design, actual packaging
   targets, and two-request performance under the shared provider limiter.

The plan already gets several important boundaries right: preview/Add identity
is preserved, ownership is refreshed after cache hits, missing ownership is
not treated as absence, required themes cannot be overridden by embeddings,
provider facts are escaped, and queries are excluded from URLs and logs. Keep
those invariants while addressing the findings; they are not optional
optimizations to trade away when a resource or quality target is missed.

## 5. Author disposition — preserve original findings above

The initial review does not claim its findings have been corrected. The plan
author should append a dated disposition here or link a separate addendum
after amendment, mapping F1–F5 to the changed sections. Record empirical gates
as pending until their artifacts and measurements exist. No implementation
tests were appropriate or executed for this documentation-only review.

### 5.1 Author amendments — 2026-09-29

The author amended the plan after the initial review. These are specification
changes, not implemented fixes or measured performance results. The amended
snapshot submitted for reviewer verification has SHA-256
`d963dd001d0c823079a0c2a66c2845adcdf9ca2d99ac2da7cc90e872c07b3581`.

| Finding | Amendment | Verification required during build |
|---|---|---|
| F1 | §2.2/§5.1/§9 select an extracted shared transport with raw caching disabled for recommendations; shared limiter/cooldown and credential snapshots remain | Prove recommendation traffic bypasses the old map, and rotated credentials fence in-flight publication |
| F2 | §9 adds provider/encoder generations, effective ranking mode, source fingerprints/expiry, lifecycle invalidation, and response-publication checks | Exercise disable, failure/recovery, rotation, source replacement, and expiry during work |
| F3 | §6.1 defines canonical theme ranking text, input/fallback matrix, thin-seed behavior, and distinct warnings; semantic cutoffs never apply to metadata-only results | Table tests for every combination and real-model cutoff calibration at M0 |
| F4 | §3.2/§8.1 make server interpretation authoritative; define every default/null case, OR genres, AND dimensions, invalid centrality combinations, and removed-filter ranking text | Normalization/API tests and UI reconciliation against the applied snapshot |
| F5 | §4.1 supplies initial ordered aliases and a required validated vocabulary artifact; §5 defines resolution failure states, one OR group, page schedule, quotas, and absolute per-list ranks | Fixture-backed keyword validation, duplicate/rank tests, and motivating-theme recall before release |

The author also amended §6.2 with independent graded relevance and explicit
nDCG gain/discount rules, §7.2 with two-request and combined-memory measurement,
and §10.2 with the inspected Linux x64 CI scope and actual generation/race/
coverage gates. All empirical gates in §3 remain pending. Fable should review
the amended design and these pending gates, not mistake this disposition for
proof that the feature already works.

### 5.2 Independent amendment verification — 2026-09-29

**Snapshot verified:** SHA-256
`d963dd001d0c823079a0c2a66c2845adcdf9ca2d99ac2da7cc90e872c07b3581`.
The reviewer re-read the changed interpretation, vocabulary, retrieval,
ranking, evaluation, normalization, runtime, and cache contracts. This was
another source/document inspection; no implementation or runtime validation
was performed.

**Updated verdict:** F1, F2, F4, and F5 are resolved at the specification level.
F3 is substantially resolved, with one narrow cutoff case still requiring an
explicit policy before semantic ranking is implemented. The design is ready
for Fable's review and bounded M0 feasibility work; these findings do not
establish that a working feature has passed its release gates.

| Finding | Independent verification |
|---|---|
| F1 | Resolved: recommendation raw caching is disabled below the bounded caches, while the extracted transport shares credentials, limiter, and cooldown. The old ordinary-search cache is explicitly outside the feature-memory claim. |
| F2 | Resolved: provider and encoder generations, actual rank mode, source fingerprints, minimum source expiry, and publication-time checks address the reported lifecycle and stale-response scenarios. |
| F3 | Mostly resolved: canonical theme Q, the fallback matrix, warnings, and thin-seed policy define the previously missing cases. The residual Q-only cutoff case is described below. |
| F4 | Resolved: server interpretation is authoritative; defaults, nulls, OR genres, AND dimensions, and theme/centrality removal are specified. |
| F5 | Resolved: vocabulary validation is a required artifact, retrieval and admission aliases differ explicitly, zero/partial resolution has defined outcomes, and page/rank/quota ordering is deterministic. |

**Residual F3:** §6.1 permits Q-only semantic ranking when seed facts contain
fewer than eight lexical tokens and residual prose supplies Q. With no required
theme, the following paragraph requires a weak-match cutoff but identifies
only S-only and combined-Q/S calibrations. A prose+thin-seed request therefore
has no assigned threshold family. Add a separately calibrated Q-only threshold
to M0's deliverables, or explicitly use metadata-only provider suggestions for
that combination. Do not silently reuse the S or Q/S threshold. This is a
small contract correction, not a reason to replace the runtime design.

No other consequential contract blocker was found in this amendment pass.
The new relevance grades and independently assembled recall set address the
evaluation concern; the actual corpus, labels, thresholds, and measurements
still do not exist as verified evidence. Theme recall versus strict admission,
centrality precision, real-model parity and ranking lift, final-image helper
packaging on actual targets, two-request latency under the shared provider
limiter, and combined Go/helper memory remain pending M0/release gates.

**Final verification of the residual correction:** the reviewer inspected
§6.1 again in snapshot SHA-256
`de25c29227e9ec9421c1cf1c8bf635ddd41ee47b8c4c10d03307e5eb2c120edf`.
Thin-seed requests without a required theme now explicitly use metadata-only
provider suggestions, including when prose exists, with the corresponding
seed/prose/provider warnings. Q-only ranking is retained only behind a required
theme's evidence gate. This resolves residual F3 without inventing another
uncalibrated cutoff.

**Final contract verdict:** F1–F5 are resolved in the reviewed specification.
No remaining required contract blocker was identified. Ready for Fable's
design review and M0; empirical gates listed above remain pending, and no
implementation or release approval is implied.

After final verification, the author updated only the plan's status header
to reflect this verdict; the verified build contract did not change.

## 6. Fable review — approve M0 only, with changes

**Received and incorporated:** 2026-09-29. **Fable's verdict:** “APPROVE M0
ONLY, WITH CHANGES. Do not approve M1–M5 on this text.” The owner supplied
the review in this chat. This section summarizes it and records the author's
disposition; it is not a claim that Fable has approved these amendments.

Fable read GitHub main at `da73227` (v0.33.0, PR #49), 17 commits beyond the
original plan's source baseline, and read Cinema at `b5fa758`. Fable reported
source/API-documentation inspection only: no live TMDB query, model execution,
container build, or verification of a Cinema HTTP embedding endpoint.
The original review described the plan as untracked in an older checkout;
that observation is historical. Before this revision the documents were
committed, local HEAD was `22d37e6`, and `origin/main` was `da73227`.

The author's revision rechecked local source for the current limiter/cache,
preview paths, ownership query, error writer, container workflow, VERSION,
and repository delivery/version rules. No implementation or experiment was
performed as part of these documentation amendments.

### 6.1 Must-fix findings and author disposition

| Fable finding | Concrete concern | Revised contract / state |
|---|---|---|
| M1 — request arithmetic | 71 upstream calls consume at least 6.1 s at 10 rps/burst 10 before network cost; two calls need at least 13.2 s, incompatible with optimistic latency | §5.3 now admits one cold search, no waiting queue, 30 enrichments/41 total calls, cached-only readers separately, prompt 429 for a second cold caller. Shared 10 rps ceiling/cooldown plus an 8 rps/burst 4 recommendation sub-limit and ordinary-metadata dispatch priority; arithmetic and partial outcomes are explicit. M0 measures it; not yet proven. |
| M2 — helper packaging | The static distroless image cannot host an unspecified dynamically linked Candle/tokenizer helper, and no archive workflow exists | §7.1 chooses a standalone glibc helper built in a Debian 12 Rust stage and a proposed distroless cc final image. `tools/curator-embed/` owns its crate/lockfile/toolchain. M0b must prove actual non-root final-image execution, dependency closure, parity, build time, and cache needs on Linux x64. No image change or build has happened. |
| M3 — independent runtime versus Cinema | Standalone complexity was justified by an asserted non-goal; Cinema might supply the same vectors but its API was unverified | Owner explicitly chose standalone: the apps must function on their own even while integrated. §7.1 records that reason, rejects a mandatory Cinema adapter, and retains the shared port. This is an owner decision; availability of a Cinema embedding endpoint is irrelevant to the chosen v1. |
| M4 — order M0 around coverage | The most consequential unknown is cheap keyword coverage and top-pool membership, not inference | §10.1 makes approximately 30 independently chosen gay-themed series the first experiment; record keyword coverage, hit@60/100/200, selected-30 hit, incidental-theme rate, and exact sort/filters. Below 60% keyword coverage or over half incidental/insufficient top-60 rows stops the current strategy before helper/domain work. Passing these screens is not the 80% release recall gate. No coverage measurement yet. |

### 6.2 Should-fix findings and smaller corrections

| Finding | Author amendment |
|---|---|
| S1 — teen filter empties the list | §4.2 excludes affirmative teen-focus evidence and retains unknown focus, with truthful label/help. Language/year remain strict. Coming-of-age alone is not treated as proof of teen focus. |
| S2 — filter upstream | §5.1 names original-language, first-air-date and OR-genre parameters; local validation remains. Seed endpoints are unfiltered and have a specific sparse-results explanation. |
| S3 — ownership cost/rule duplication | §8.3 names the KnownTMDBIDs narrow-query precedent, at most one batched read per namespace (three total), and extraction of the existing conflict predicate into one shared pure domain helper. |
| S4 — sparse centrality | §4.2/§6.2 set zero false-central claims as the subset-specific gate, report empty/coverage counts, and add help text. There is no minimum central-result count or broad recall target for this optional strict subset. |
| S5 — paths/build ownership | §2.2 names `web/src/PreviewDrawer.tsx`, its CSS, and `metadataPreview.test.ts`; §7.1 selects a separate crate under `tools/curator-embed/` with a local lockfile and explicit Rust CI checks. |
| S6 — cache precision | §9 distinguishes five-minute freshness/8 MiB response reads from unbounded retained entries, and explains v3 URL-key rotation versus v4 header-only bearer rotation. |
| S7 — release version/docs | M5 proposes 0.34.0 if 0.33.0 remains the base, otherwise the next unallocated minor; VERSION, settings, usage, and deployment changes ship with behavior. This docs-only revision does not bump VERSION. |
| S8 — prose aliases | §4.1 adds a distinct interpretation table: `gay TV series` → Gay male stories; `queer`/LGBT/LGBTQ+ → LGBTQ+ stories; all initial themes have testable prose aliases. |
| Error helper/genre list | §8.1 uses existing `writeCodedError`, documents free-string codes and a fixed versioned TV genre allowlist, with no extra per-query genre request. |
| Paging/concurrency fixture | §5.2 permits bounded concurrent reads while retaining deterministic schedule order; §10.2 adds the same-show similar-page-1 rank-1/recommendations-page-2 rank-23 regression. |

### 6.3 What remains unapproved and unmeasured

M0 proceeds in order: coverage, standalone final-image packaging, then ranking
and workload qualification. A failed retrieval screen stops runtime work.
M1–M5 remain conditional until the M0 evidence and any resulting contract
changes receive review. In particular, no document inspection can establish
keyword recall, centrality precision, model ranking lift, final-container
compatibility, CPU/RAM cost, or latency under provider contention.

The owner decision settles independence from Cinema. The chosen cc image is a
concrete packaging proposal requiring a real build/run, not an observed fix.
The reduced enrichment budget must be evaluated against recall; smaller
request counts alone do not establish useful recommendations. The original
independent review's findings remain addressed in their subject areas, but
its verdict must not be cited as approval of the newly changed runtime and
traffic contracts.

## 7. M0a follow-up — list recall becomes a diagnostic, candidate quality is the gate

**Reviewer decision:** 2026-09-29 local time, after inspecting the committed
[first M0a report](smart-media-discovery-m0a.md) at `0ced7c9` and the
[revised route comparison](smart-media-discovery-m0a-revision.md) at
`3f80a0b`. The reviewer independently reconstructed the ordered page merges,
quota/fill behavior, shallow-score selection, and selected detail identities
from the credential-free fixtures. This accepts the reported catalog-coverage
losses as measurements, not a claim that all returned works are irrelevant.
The initial 38/41 keyword/reference proxy was corrected after review; no
row-level incidental-theme rate was established by that proxy.

The original §6.2 proposed at least 80% recall of a frozen known-relevant
catalog list as a release gate for broad theme queries. M0a showed how a
30-enrichment budget misses many members of such a list, including titles
whose keywords are present. The reviewer withdrew that as a *necessary*
broad-query gate: a search can miss titles in a large catalog and still
offer ten useful suggestions. Keep independent-list recall and its language/
visibility splits as coverage/bias diagnostics, and keep finite expected-set
coverage for predeclared narrow requests. The updated §6.2 instead gates
broad discovery on independently adjudicated candidate relevance, at least
ten evidence-supported eligible results for broad queries with abundant
matches, existing precision@10 ≥ 0.8, no known wrong-theme top-ten result,
and no hard-filter violation. It retains the semantic nDCG lift of ≥ 0.03,
resource budgets, standalone runtime, and multi-query held-out evaluation.
The change neither narrows the owner's generic-theme feature nor declares
any measured route ready to build.

The next evidence step is the preregistered
[selected-candidate annotation](smart-media-discovery-m0a-annotations.md)
for R3 theme search and Looking seed suggestions. Its agent labels are
provisional and require reviewer adjudication before release decisions.
At that stage, M0b and M1–M5 remained on hold until candidate-quality
evidence and any resulting retrieval design could be reviewed. The subsequent
M0b disposition is recorded below.

### 7.1 Selected-candidate adjudication and M0b disposition

**Reviewer decision after `9661967`: PROCEED TO M0b feasibility; M1–M5
remain gated on M0c.** The reviewer independently checked all 60 annotated
route positions, arithmetic, and 59 distinct detail identities. R3's fixed
selected pool has 12 strongly supported central candidates under TMDB
metadata, with a thirteenth plausible suitable candidate whose centrality
remains uncertain. That is enough to test whether a standalone semantic
ranker improves the actual ordering. The current unmodeled R3 top ten is
7/10 suitable, below the proposed precision target; this is a reason to
test the model, not a reason to refuse the feasibility experiment.

The initial agent annotation had Looking seed fit ≥2 on 9/30 selected
titles. The reviewer adjudicated *The L Word* and *Generation Q* as grade-2
seed matches because adult queer friendship in Los Angeles is a meaningful
connection to Looking even without a gay-male lead. The revised seed pool
therefore has 11/30 plausible supported matches; its fixed top ten remains
7/10. No mandatory gay-male filter is implied for a seed-only request.
All four initial `contradicted` theme labels in this set became
`insufficient`: another central focus does not prove absence of gay-male
material. The candidate table keeps separate theme-evidence and seed-fit
columns, flagged uncertainty, and the original versus adjudicated counts.

External editorial evidence may change **real-world relevance** without
changing what TMDB metadata can support at runtime. Netflix's account of
Eric's major gay-identity and faith arc in *Sex Education* supports a
provisional real-world grade 2; its frozen TMDB-only row remains grade 1.
Likewise, ABC describes the male lead's sexuality storyline in *The
Newsreader*; bisexuality is not contrary gay-male or queer evidence. These
sources are cited in [the annotation](smart-media-discovery-m0a-annotations.md).
Remaining flagged cases require resolution for M0c quality evaluation. M0b
may measure packaging and vector parity before that adjudication. The R3
six-alias/three-lane retrieval and its 41-call combined-request arithmetic
must be reconciled with §5 before M0c or M1; neither is a shipping contract
approval yet.
