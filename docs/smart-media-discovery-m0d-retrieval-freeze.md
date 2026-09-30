# Smart Media Discovery M0d: bounded seed source mixture

**Status:** tuning-only design freeze before any model ordering of these mixed
pools. M0c's held-out queries have been consumed and remain the historical
failure record; they are not used to choose this quota or score. This
experiment tests whether a general seed-derived topic source can improve
candidate coverage without exceeding the 41-call cold cap.

## Source rule

Read the verified seed detail first. Map only exact normalized provider
keyword names and IDs in the reviewed theme vocabulary to discovery sources.
The keyword is an optional source hint, never a required-theme admission
claim or evidence of a character's sexuality. The server must verify the
returned keyword ID/name pair in the seed detail; when that check fails it
uses the related-list route. Reusing an already verified seed keyword ID
avoids a redundant keyword-name lookup. The offline transformed details
retain names but not all IDs, so the captured discovery page's keyword ID is
recorded separately and the runtime ID/name check still needs a fixture.

Take one discovery page for each recognized topic, up to two distinct
topics. Select a topic's first eligible alias by the stable vocabulary
priority, independent of the benchmark seed title. An unrecognized topic
adds no discovery page and does not disable semantic seed comparison. A
multi-topic seed may contribute two independently verified pages; it is not
forced into one topic label. No trending fallback or unrelated catalog
source is allowed.

For the first route version, topic order is `queer`, `space_exploration`,
`political_drama`, `coming_of_age`, `found_family`. Alias priority within a
topic starts with `lgbt`, `boys' love (bl)`, `gay theme`, `gay romance` for
queer; `space travel`, `space exploration`, `spacecraft` for space; and
`politics`, `political drama`, `political corruption` for politics. Remaining
approved aliases follow the vocabulary's written order. This broad-first
source policy is frozen before mixed-pool scores; a later change needs a
versioned comparison and fresh evaluation.

Retain at most sixty list summaries. With one topic page, reserve forty
related-list positions and twenty topic-page positions. With two pages,
reserve twenty related positions and twenty from each topic page. Deduplicate
by exact TMDB ID, exclude the seed itself, and fill remaining slots from
the unused related-list order, never beyond sixty. The related order remains
the frozen recommendations/similar absolute-rank merge. Topic order is
provider page order. A duplicate keeps both source-rank contributions but
one candidate identity. Semantic shallow selection sees only the retained
list overviews and picks thirty. Detail enrichment and scoring reuse M0c's
ID-independent text, soft topic bonus, seed-only cutoff and hard filters;
no new ranking parameter is tuned from consumed H queries.

Call ceiling for a one-topic seed is one seed detail + three related pages
+ one discovery page + thirty selected details = **35**. A two-topic seed
uses **36**. A TVDB mapping or any other actual upstream call reduces the
thirty-detail allowance one-for-one to keep the 41-call cap. The source
mixture does not add a required-theme filter or change the exact 15s/10s/4s
deadlines. It is a retrieval candidate for M0, not production behavior.

## Captured tuning comparison

The first comparison uses four previously frozen tuning seeds, each with a
captured page for an exact name in its seed metadata:

| Seed | Exact seed keyword | Captured discovery keyword ID | Topic page |
| --- | --- | ---: | --- |
| Looking | `lgbt` | 158718 | LGBT page 1 |
| Bad Buddy | `boys' love (bl)` | 289844 | BL page 1 |
| Star Trek: The Next Generation | `space travel` | 3801 | Space travel page 1 |
| House of Cards | `politics` | 6078 | Politics page 1 |

These pages were captured for the earlier corpus, not fetched in response to
the consumed H rankings. Two additional tuning query wordings use the Bad
Buddy and House pools and are frozen before their mixed-pool model order.
All new topic candidates need query-specific provisional labels before
selection. Seven captured topic candidates lack detail rows; preserve them
as unavailable enrichment in the offline comparison rather than quietly
substituting a different title or inferring full detail from a list summary.

Report related-only and mixed pool suitable counts, semantic-selected thirty
coverage, post-detail eligibility and P@10/nDCG on the same graded identities.
Keep coverage and ranking attribution separate. This tuning result can choose
a revised route but cannot itself qualify release; freeze genuinely fresh
held-out queries and source-backed judgments afterward.
