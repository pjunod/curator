# Smart Media Discovery M0d: focused seed-source revision

**Status:** provisional tuning policy, chosen after inspecting the frozen M0d
v1 results. This comparison fits four tuning seeds and does not qualify M1 or
predict unseen-seed performance. The consumed M0c H rankings did not set this
rule. The v1 candidate, label and model-order artifacts remain unchanged.

## Source rule v2

For optional **seed-pool expansion only**, use one ID-independent rule:
consider a captured topic page when the seed detail contains an exact,
verified keyword ID/name pair in the focused-source allowlist. The current
allowlist is `boys' love (bl)` (TMDB keyword 289844) and `space travel`
(keyword 3801). It describes the source's specificity, not a required theme
or the identity of any benchmark seed. A narrower approved alias can qualify
even when the seed also has a broad keyword. When no allowlisted exact pair
is verified, use the related-list route. In particular, `lgbt` and
`politics` alone do not expand a seed pool in this version. They remain
available for explicit theme searches, and this result does not establish
that their discovery pages are generally poor.

If both focused aliases are present, take both pages in the fixed order
`boys' love (bl)`, then `space travel`; retain twenty related summaries and
twenty from each page. With one page, retain forty related and twenty topic
summaries. This is M0d v1's two-source quota and remains under its 36-call
ceiling. Deduplication, sixty-summary cap, thirty-detail cap, hard filters,
semantic shallow selector, scoring and 15s/10s/4s deadlines stay as frozen
in M0d v1. A runtime fixture must still
verify that seed keyword IDs and names agree before this policy is eligible
for production. The captured transformed seed details retain names but not
all IDs; the offline page records alone do not prove the runtime check.

## Tuning result

The v1 mixed pages and grades were frozen before their first model order.
The following policy selection uses those same orders; it is not an
independent second test. “Union nDCG” uses each seed's common related/mixed
candidate union as the ideal, so lost candidates count against a route.

| Seed | Related-only suitable pool / selected | Broad-first mixed suitable pool / selected | Related-only P@10 / union nDCG | Mixed P@10 / union nDCG | v2 route |
| --- | ---: | ---: | ---: | ---: | --- |
| Looking | 19 / 15 | 15 / 12 | 1.0 / .6765 | .8 / .6057 | Related |
| Bad Buddy | 21 / 15 | 28 / 19 | 1.0 / .9173 | 1.0 / .9258 | Focused BL page |
| Star Trek: The Next Generation | 25 / 20 | 32 / 23 | 1.0 / .8777 | 1.0 / .9258 | Focused space-travel page |
| House of Cards | 25 / 18 | 23 / 17 | 1.0 / .8268 | .9 / .7082 | Related |

Across these four tuning seeds, related-only has 90 suitable pool candidates,
68 suitable selected candidates, mean P@10 1.0 and mean union nDCG .8246.
Broad-first mixing has 98, 71, .925 and .7914. The fitted v2 route has 104,
75, 1.0 and .8387. These fitted means are diagnostic and cannot serve as a
release gate. The broad-first mixed route also selected three candidates
without captured details for TNG and three for House; the related-only route
selected none for either. Those missing details reduce post-enrichment
coverage and cannot be silently replaced with summary-only assertions.

The two extra preference intents rescore the **same seed-only model orders**
against narrower grades; their wording was not encoded in retrieval or ranking.
Their agent-derived grades were frozen before the v1 mixed-order run, but
only cover the mixed 60-candidate pools, so a fair related-only comparison
cannot be computed from them. On the mixed route, N01 (“romantic rivals at
university like Bad Buddy”) has 18 suitable in pool, 13 selected, 8 in the
top ten and selected-set nDCG .6708. N02 (“political corruption dramas like
House of Cards”) has 23, 17, 9 and .7783. These wording results show that
the seed-only ranker loses some more specific preferences; they do not
validate the focused-source rule or supply independent release judgments.

## Remaining qualification

The focused rule leaves the historical weak H seed and theme/combined cases
unqualified. Do not tune against those consumed H rankings. The smallest
useful next validation is a newly frozen, title-disjoint set with independently
source-backed judgments, provenance and uncertainty labels, candidate
identities and hard-filter evidence captured
before ranking. Include a focused-alias seed, a broad-only seed, a seed with
no recognized alias, and the theme/combined coverage intents that failed in
M0c. Report full pool, shallow-selected, detail-eligible and returned
coverage separately, and score against one fixed union ideal. In parallel,
verify runtime keyword ID/name pairs and actual provider/cache latency under
the already measured exact-mode budget. An implementation decision should
follow those checks rather than another open-ended alias sweep.
