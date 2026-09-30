# Smart Media Discovery M0c gate

**Verdict: M0c does not qualify M1.** This is a prototype evaluation of
retrieval, ranking and resource feasibility, not a released feature. The
quality corpus has 24 frozen queries, 295 distinct candidate identities and
630 initial query judgments, with later full related-pool judgments for four
seed routes. Judgments are agent-assigned or derived from provider facts;
four disputed items received reviewer-agent adjudication. They are not an
independent owner/human relevance set. The held-out split is by query: 177 of
255 held-out identities also appear in tuning.

## What was measured

The [policy](smart-media-discovery-m0c-policy-freeze.md),
[query-text inputs](smart-media-discovery-m0c-qtext-fixtures.json),
[candidate snapshot](smart-media-discovery-m0c-candidates-v2.json), and
[labels](smart-media-discovery-m0c-labels-v2.json) identify the offline run.
The first model run is retained [as invalid evidence](smart-media-discovery-m0c-invalid-first-run.json):
its combined path bypassed required-theme admission, omitted positive query
wording, fabricated theme provider ranks, and overstated returned counts.
The [second run](smart-media-discovery-m0c-repaired-v2.json) repaired those
contracts on the already-consumed queries. The
[final diagnostic](smart-media-discovery-m0c-repaired-v3.json) further fixes
combined pool accounting and fails closed on missing enriched detail language
for H02. It is a disclosed same-policy repair run, not fresh held-out
validation. It encoded 260 query/seed/shallow texts and 334 selected-detail
texts across all queries in 13.52 seconds on local macOS; this is not
one-request latency.

The table shows full shallow pool, enriched set, admitted set, displayed count
(maximum 20) and precision. “Suitable” means provisional grade 2–3. Pool
suitable is shown only where every item in that pool was graded. Model-off
uses provider-order seed selection and no cosine cutoff; model-on uses the
frozen semantic shallow selector and a seed-only cutoff. The same-selected
ablation is in the JSON and should not be confused with complete model-off.

| Query | Pool | Pool suitable | Selected / suitable | Eligible / suitable | Returned | Model-off P@10 | Model-on P@10 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| H01 · gay-themed Asian series | 54 | — | 30 / 7 | 23 / 7 | 20 | .2 | .6 |
| H02 · Korean original language | 40 | — | 30 / 25 | 26 / 25 | 20 | 1.0 | 1.0 |
| H03 · adult friendships + seed | 60 | — | 28 / 7 | 10 / 7 | 10 | .7 | .7 |
| H04 · Tales of the City 1993 | 60 | 4 | 30 / 3 | 19 / 3 | 19 | .3 | .3 |
| H05 · Tales of the City 2019 | 60 | 14 | 30 / 11 | 21 / 9 | 20 | .2 | .7 |
| H06 · LGBTQ+, no teen dramas | 20 | 5 | 20 / 5 | 12 / 5 | 12 | .3 | .4 |
| H07 · lesbian romance | 40 | — | 30 / 14 | 30 / 14 | 20 | .4 | .9 |
| H08 · adult coming of age | 20 | 2 | 20 / 2 | 7 / 1 | 7 | — | — |
| H09 · chosen family | 23 | 15 | 23 / 15 | 23 / 15 | 20 | .6 | .7 |
| H10 · political intrigue | 40 | — | 30 / 21 | 30 / 21 | 20 | .7 | 1.0 |
| H11 · exploring space | 39 | — | 30 / 26 | 30 / 26 | 20 | .7 | 1.0 |

Across the six held-out queries with at least ten suitable works in their
**graded selected set** (H02, H05, H07, H09, H10, H11), mean P@10 is .60
model-off and .8833 model-on. This meets the plan's proposed .8 numeric
threshold on provisional labels, but does not replace independent
adjudication or the failed coverage cases. Across all ten held-out queries
that returned ten items, the corresponding values are .51 and .73; H08 is
excluded from P@10 because it returned seven. Model-on ranker nDCG improves
over the same-selected metadata comparator from .5779 to .8088 across nine
theme/combined queries, where the ideal is limited to the graded selected
set. The two fully graded seed related pools improve from .6864 to .7743
under their full-pool ideals. These two denominator scopes are deliberately
reported separately. H04's nDCG individually falls from .8053 to .7677.

H04 is a retrieval coverage failure: its entire sixty-item related-list
pool has only four suitable works. No ranker can make ten good suggestions
from it. H01, H03 and H06 also have only seven, seven and five suitable
selected items, respectively; H08 has two in the pool and one eligible.
H05 differs: fourteen suitable works exist in its full pool, eleven reach
the selected thirty, and nine remain eligible after the frozen seed-only
cosine cutoff. Selection and cutoff therefore lose a possible tenth suitable
suggestion even though its pool is sufficiently rich.
No sparse route was silently dropped or filled with unrelated trending
titles. H12 is an expected `needs_refinement` fixture emitted by the offline
evaluator for unsupported negation; runtime parser behavior is unmeasured.

The [capture audit](smart-media-discovery-m0c-capture-omissions.json) lists
21 historical transformed detail rows missing language and year. The raw
provider response bodies were not retained, so this is an export omission,
not evidence that TMDB omitted its fields. H02 is the only evaluated query
whose required hard filter uses one of these fields. Candidate 96571 lacks
detail-level `original_language` and is excluded despite a Korean list
summary. That removes one weak item and yields H02's final 26 eligible,
P@10 1.0 result. An approval check rejected locating an existing credential
through nuc3 container mounts as credential probing beyond the temporary
test authorization; no alternate credential path was attempted.

## Resource evidence on nuc3

The final Linux x64 distroless image was temporarily rebuilt with cached
base/build stages. The pinned model was mounted read-only. All benchmark
containers ran with no network, a read-only root, two CPU cores and a
512 MiB memory limit. The image tag, transferred model/input/binary files
and test containers were removed afterward; a final `docker ps` check found
no benchmark container running. Normal Docker builder cache was not pruned.

| Experiment | Measured result | Scope |
| --- | --- | --- |
| Captured Looking helper workload | 60 shallow plus 31 detail texts; 12 protocol requests; 20 warm repeats: p50 2.976s, p95 4.213s, max 4.914s; cold process 3.459s; cgroup peak 197.5 MB; helper VmHWM 189,396 kB | Helper only; warm p95 exceeds the provisional 4s ranking allocation. No Go or provider work. |
| Go + persistent helper hybrid stress | Ten complete runs of 41 scheduled loopback calls with injected 35 ms upstream delay and the same 91-text Looking encoding load: p50 9.211s, p95/max 12.701s; combined cgroup peak 192.9 MB; Go VmHWM 12,624 kB, helper 189,840 kB | **Hybrid stress envelope, not an exact frozen request mode:** the 41-call budget is combined mode, whereas Looking's semantic shallow selection belongs to seed-only mode (34-call cap). The result cannot prove actual-mode 12s compliance or failure. |
| Scheduler probes in hybrid harness | Ordinary title call at one arrival point 36 ms; different-key second cold rejected under 1 ms; injected 429 blocked the next call without retry; synthetic 5 ms ownership sleep gave cache p95 5.79 ms | Illustrative prototype checks, not production endpoint, saturated priority, real ownership/cache, or partial-response p95. |
| Exact theme prototype | Ten complete 39-call runs; one Q + thirty detail texts; p50 5.494s, p95 5.690s; combined cgroup peak 193.4 MB | Synthetic 35 ms loopback provider, persistent helper; 10s provider, 4s cumulative ranking and 15s total budgets enforced by the harness. |
| Exact seed prototype | Ten complete 34-call runs; seed plus 59 summary texts, then seed plus thirty detail texts; p50 5.985s, p95 6.059s; combined cgroup peak 193.6 MB | Same synthetic host/budgets; two interleaved inference stages. |
| Exact combined prototype | Ten complete 41-call runs; Q, seed and 28 detail texts, with no shallow semantic stage; p50 5.723s, p95 5.805s; combined cgroup peak 193.3 MB | Same synthetic host/budgets. |
| Focused failure/admission checks | A stopped helper was killed/reaped within 109–111 ms; a different second cold request was rejected under 1 ms; four-total attached and two cached-reader bounds, distinct-key cache miss, and joined-caller release passed deterministic gate checks | The helper cancellation is prototype code. One ordinary title arriving 150 ms into a search took 35.5–36.1 ms; saturated priority remains unmeasured. Synthetic cache p95 was 5.6 ms with an injected 5 ms ownership sleep. |

These three exact-mode runs are **synthetic feasibility evidence**, not
production request p95. Live TMDB round trips, production shared-limiter
contention, actual cached-response p95, library-task contention, and
saturated ordinary-work priority remain unmeasured. The revised prototype
now bounds the helper pipe and kills a deliberately stopped child, but the
actual Go recommendation service does not exist yet. Deterministic gate
checks exercise four-total attached callers, two cached readers *during* an
active cold request, a distinct cache key and release after an attached caller
cancels. A queued ordinary request wins the next shared token over a queued
recommendation; canceled grants are skipped without consuming that token.
These last admission/dispatch corrections were verified with a focused Go
test after the timings and did not change measured request stages. The injected 429
blocked the next call without retry; it produced 0/3/0 successful pages
in theme/seed/combined runs and a partial *stage* duration of
.577/.037/.724s, not a complete partial-response latency or sufficiency
measurement. These remaining checks are **unmeasured**, not passes.

## Gate decision

| Gate | Verdict | Reason |
| --- | --- | --- |
| Candidate sufficiency | **Fail** | H01/H03/H04/H06/H08 cannot yield ten suitable displayed works from the selected pool; H04 has a 4/60 full-pool ceiling. |
| Mean P@10 ≥ .8 on sufficiently labeled queries | **Provisional numeric pass** | .8833 on six selected-set-sufficient queries, using agent/derived labels and a consumed query split. |
| No wrong-theme or hard-filter violation | **Partial** | Mandatory-theme admission is applied to all combined candidates and missing H02 language fails closed; independent editorial truth and a production parser/filter implementation are absent. |
| Semantic nDCG lift ≥ .03 without motivating-query loss | **Provisional numeric diagnostic** | Mean same-selected lift exceeds .03 in both denominator scopes on agent/derived labels and a consumed split; H02 remains 1.0, but H04 declines. The aggregate does not establish motivating-query nonregression for release. |
| Helper memory ≤ 512 MiB | **Pass for measured helper workload** | Helper high-water mark about 185 MiB; combined hybrid cgroup peak 192.9 MB is separately scoped. |
| Warm-model cold-search p95 ≤ 12s; cache p95 < 1s | **Synthetic exact-mode pass; production unmeasured** | Exact-mode prototype p95 was 5.690/6.059/5.805s for theme/seed/combined under injected 35 ms provider latency. The earlier hybrid stress p95 12.701s is not a request-mode failure. Real provider and ownership/cache p95 remain unmeasured. |
| Deadline, 429, joining and ordinary-work priority | **Prototype partial / production unmeasured** | Exact-mode harness enforces 15s/10s/4s budgets and reaps a stopped helper; prompt rejection, joined/cached-reader caps and one cooldown path passed prototype checks. Saturated priority, full partial responses and production integration remain unverified. |

M1 remains gated. The next design pass must address candidate sufficiency,
obtain independent relevance adjudication and a fresh validation split, and
measure exact request modes with deadline-safe Go/helper integration. Do not
retune the consumed set or raise the rate/deadline limits to make this report
appear to pass.
