# Smart Media Discovery M0d: proposed bounded capture

**Status:** proposed scope for Paul's explicit approval. Do not access a
credential or make these calls before that approval. Automatic approval
review rejected inspecting nuc3 container mounts to locate an existing TMDB
credential as credential probing beyond authorization for temporary tests.
No alternate credential path was attempted.

## Frozen diagnostic identities and routes

All three routes below use **page 2** to seek new source candidates. That is
an explicit diagnostic retrieval-policy revision, not a test of the unchanged
page-1 runtime route. Page 2 is not presumed title-disjoint: measure and
publish exact candidate-ID overlap with every M0c/M0d tuning and consumed
held-out set. Any independent-subset result must name its actual size. Do
not substitute a different seed, alias, page or title if a case is sparse.

| ID | Query and seed | Exact source pages | Runtime-shaped selection and detail ceiling |
| --- | --- | --- | --- |
| C01 | “Queer coming-of-age series, no teen dramas”; theme only | `coming of age` keyword 10683 and `lgbt` keyword 158718, each `/discover/tv` page 2 | Deduplicate up to 40 summaries, lexical summary selection of 30; detail at most 30. The supported no-teen hard filter is applied after detail and fails closed on missing evidence. |
| C02 | “Gay historical political romance like Fellow Travelers”; combined with TMDB TV seed **216089** | Exact gay-theme, BL and LGBT keyword lanes from M0c, each `/discover/tv` page 2; seed recommendations page 1, similar page 1, recommendations page 2 | Keep the frozen 30/30 disjoint theme/seed pool and first-fourteen/alternating selection of 28. Every selected candidate, from either origin, must pass the required gay-men theme-evidence gate and any hard filter. Positive query text enters the frozen 0.6 theme/0.4 seed score. |
| C03 | “Shows like Cherry Magic! Thirty Years of Virginity Can Make You a Wizard?!”; seed TMDB TV **111204** | Seed recommendations page 1, similar page 1, recommendations page 2; `boys' love (bl)` keyword 289844 `/discover/tv` page 2 only after exact seed keyword ID/name verification | Focused-source v2 quota: 40 related +20 topic, deduplicated to at most 60 summaries, semantic shallow selection of 30, then detail and frozen seed score/cutoff. If the exact keyword pair is absent, use related-only and record that branch. |

These are new **query-seed assignments**. Seed 111204 already appears as a
candidate in the M0d tuning corpus; that overlap is disclosed rather than
presented as unseen-title evidence. A candidate's repeated identity across
routes is counted once in each compared union. Freeze query wording,
provider page order, candidate IDs and source ranks, labels, missing-detail
status, parser output and acceptance thresholds before inspecting any new
model order. No scoring adjustment is allowed from these three outcomes.

## Request ceiling and evaluation boundary

The only endpoint families are TMDB v3 `/search/keyword` for exact alias
verification, `/discover/tv` with the stated keyword IDs and page number,
`/tv/{id}` with keywords in the detail response, and
`/tv/{id}/recommendations` or `/tv/{id}/similar` for the stated seed pages.
Use no page 3, broad search, retry or additional endpoint. Hard total:

| Case | Alias lookups | Discovery pages | Seed detail + related pages | Full-union candidate details | Maximum GETs | Runtime-shaped maximum |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| C01 | 2 | 2 | 0 | 40 | **44** | 34, including 30 details |
| C02 | 6 | 3 | 4 | 60 | **73** | 41, including 28 details |
| C03 | 0 | 1 | 4 | 60 | **65** | 35, including 30 details |
| **Total** | | | | | **182** | |

Full-union detail calls beyond the runtime-shaped selected-detail ceiling
exist **only** to support independent source-backed relevance judgments.
They may not influence preselection, admission or ranking for a simulated
request. Exact ID/name verification uses the seed detail already budgeted
for C03, with no redundant keyword lookup. Keep the 60-summary, 41-upstream
call, 15s total, 10s provider and 4s cumulative ranking limits. Stop each
route on 429, deadline or budget exhaustion and report partial counts.

The compared union for each case must have complete 0–3 judgments backed by
first-party detail or an identified independent source, with provenance and
uncertainty per item. Missing detail is recorded separately; it does not
become inferred hard-filter evidence. Report suitable counts in full pool,
selected set, detail-eligible set and returned list, plus selector loss,
semantic-cutoff loss and hard-filter loss as separate numbers. Use one
compared-union nDCG ideal per case and same-selected ranking attribution.
Diagnostic acceptance requires no hard-filter/theme-evidence violation and
at least ten suitable candidates surviving each stage for a case to count
toward P@10; where ten survive, target P@10 at least .8. Cases with fewer
than ten suitable are explicit coverage failures. These three cases alone
cannot qualify M1 or establish unseen-title performance.

## Credential handling and cleanup

The intended source, if Paul authorizes it, is the TMDB credential already
configured for Curator/Monarr on nuc3. Its type and accessible interface
have **not** been verified; the rejected mount inspection must not be
retried without Paul's direct approval. Prefer [TMDB's documented API Read
Access Token](https://developer.themoviedb.org/docs/authentication-application)
in an `Authorization: Bearer` header for v3 GETs. If the
configured credential is a v3 `api_key`, use it only through a supported
client path with verified URL/log redaction; otherwise stop and request a
user-provided read access token. Never put a secret in a command argument,
printed output, logged URL, committed file or saved capture. Hold it only in
process memory, avoid shell tracing, and omit authentication fields from
sanitized provenance. Save useful sanitized list/detail responses locally
for reproducibility, then remove transient remote files and containers and
verify no benchmark process remains on nuc3 or the local host.
