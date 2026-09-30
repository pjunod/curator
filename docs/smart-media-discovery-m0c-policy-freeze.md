# Smart Media Discovery M0c: scoring policy freeze

**Status:** frozen before the first held-out model ordering. This is a
prototype evaluation policy, not a production release decision. The query
identities are in [the corpus plan](smart-media-discovery-m0c-corpus-plan.json),
the corrected provider snapshot in
[the v2 candidate file](smart-media-discovery-m0c-candidates-v2.json), and
provisional agent judgments in
[the v2 labels](smart-media-discovery-m0c-labels-v2.json). The split is by
query, not by title: 177 of 255 held-out titles also occur in tuning.

## Routes and admission

The gay-men theme route uses R3's six exact keyword lookups and three first
discovery pages. It merges at most sixty list summaries, ranks them by the
frozen lexical summary score, and enriches thirty. A candidate must have a
narrow provider keyword or an explicit gay-men statement in its synopsis;
these establish *present* evidence. Only an anchored main-story synopsis
statement establishes *central* evidence. Admission is evidence tier 1 or 2.
Within each tier, metadata order uses reciprocal provider rank from the
captured retrieval lanes; the model order uses cosine to the canonical theme
sentence plus retained positive query preferences, then that same metadata
rank. H01 retains `Asian series` as a soft preference without inventing a
hard production-language filter. H03 retains `adult gay friendships`; H07–H11
retain their positive paraphrases where distinct. Parsed negative filter spans
do not enter embedding text.
The model cannot promote an item across evidence tiers. The original-language
filter is passed to all three R3 discovery lanes for H02 and checked again
against the enriched detail row. Missing detail language fails that hard
filter even if the list summary says `ko`. Known teen-focused
stories are excluded locally when that filter is requested. An ambiguous teen
case is retained and identified as such in the error analysis.
For this prototype, a known teen focus means a provider keyword exactly
`teen drama`, `teenager`, `teenagers`, `high school`, `high school student`,
`high school students`, `school romance`, `school life`, `lgbt teen`, or
`teen coming of age`, or an overview phrase explicitly describing a teen or
high-school student as a protagonist. Mere use of a school as a setting does
not establish this exclusion.

Other theme discovery lanes are exploratory exact TMDB keyword snapshots.
They are scored for failure analysis only. They cannot establish that the
release API has a general theme retrieval route. Their local admission checks
require a provider keyword or synopsis phrase for the requested topic;
`lgbtq` maps to the broad queer topic, while `lesbian` requires explicit
lesbian, girls-love or women-in-love phrasing. They do not use the gay-men
centrality tier.

The seed route merges recommendations page 1 with similar page 1 by absolute
list position, then adds recommendations page 2, to at most sixty distinct
titles. It excludes the exact seed ID. For a seed with at least eight lexical
tokens in its overview and bounded approved keywords, it encodes the seed
and each nonempty *list summary* overview. The thirty highest shallow cosine
scores receive detail enrichment, with provider order and ID ties. An empty
summary sorts after scored rows. A genuinely thin seed uses provider order
for its thirty. Multiple or unknown provider topics do not make a seed thin.

Every enriched seed candidate and seed uses `provider_text-v2`, the same
ID-independent bounded text function. Seed detail score is cosine plus 0.06
when the candidate's provider metadata matches the seed's single recognized
soft topic. Metadata comparator is reciprocal provider rank plus 0.02 for
that same topic match. An unresolved topic contributes zero to both. The
minimum seed cosine for display is 0.48, selected from the four tuning seeds;
it applies to the raw cosine before the topic bonus. It is a confidence
heuristic, not a probability. Report eligible and returned counts separately
from precision so short lists cannot appear to pass P@10.

Combined mode retains the corrected 30/30 disjoint theme/seed path quota and
28 enrichment slots of the v2 provider snapshot. Shared IDs consume a theme
slot and keep both provider rank contributions. The theme path's first
fourteen are lexical-summary ordered; the seed path's first fourteen use its
frozen provider order. Remaining slots alternate paths. This route has **no**
semantic seed shallow selector in M0c. Every candidate, including one from
the seed retrieval path, must pass the required gay-men evidence gate and all
hard filters. A high seed cosine cannot substitute for that evidence. After
admission, its model score is 0.6 times theme cosine plus 0.4 times seed
cosine, plus a 0.06 seed-topic match; evidence tier remains the first sort
key for every admitted item. The comparator combines actual per-list
reciprocal provider ranks with the same 0.6/0.4 weights and a 0.02 seed-topic
match. No seed cosine cutoff is used in combined theme search. This
combined rule was specified before held-out scoring but not independently
tuned, so its quality is exploratory.

The unsupported negative phrase in H12 returns `needs_refinement`; it is not
silently discarded or converted to a different search.

## Evaluation and limits

The frozen 0–3 grades are provisional agent-assigned or mechanically derived
from provider facts. Four disputed items have reviewer-agent adjudications
with first-party sources. They are not owner or independent human judgments.
P@10 requires ten returned items and counts grades 2–3 as suitable. Lists
return at most twenty. Selected nDCG uses the best order of the *same enriched
candidates*; full-pool nDCG uses all graded related-list candidates for seed
routes. Theme and combined routes have grades only for their enriched set, so
their reported full ideal is explicitly limited to that set even though the
pool count records the larger shallow pool. These denominators
must remain distinct. Compare model-on with model-off on the same selected
pool for rank attribution, and compare whole pipelines separately for
selection plus ranking attribution. Report pool, selected, eligible, and
returned counts for every query, including the known H04 4/60 coverage
ceiling. Retain zero-ideal cases as explicit coverage failures.

The first held-out evaluator run was reviewed and found invalid: it bypassed
required theme admission in combined mode, dropped positive Qtext,
fabricated theme provider ranks from selected positions, and overstated
returned counts. Its raw output is retained as historical evidence. The
corrections above restore the pre-existing plan's hard-filter and text
contracts. They are evaluator repairs, not tuning from its scores. The
repaired run must be identified as a reused, consumed query set rather than
an untouched first held-out result.
The expected normalized Qtext for each scored theme/combined query is now in
[input fixtures](smart-media-discovery-m0c-qtext-fixtures.json), byte-identical
to the repaired run's inputs. These are offline ranking inputs tied to each
exact prose/filter pair; they do not demonstrate a runtime parser.
