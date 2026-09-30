# Smart media discovery M0a revision — bounded retrieval after the failed gate

**Status:** revised routes measured; none qualifies for M0b · **Baseline:**
`d7e938a` · **Initial M0a evidence:** [failed gate](smart-media-discovery-m0a.md)
at `0ced7c9` · **Held-out identities frozen:** `dcb6b6f` · **Route rules
frozen:** `c78e907` · **Sampled:** 2026-09-30 UTC.

Companion to [the discovery plan](plan-smart-media-discovery.md) §5 and §10.1.
This experiment tests revised retrieval routes before any encoder, domain
package, or feature implementation. The initial 30-title set has already
influenced route selection; it is tuning data. The following titles and query
strata are frozen separately before their TMDB identities, keywords, or
discovery positions are inspected. Titles with failed identity resolution
remain counted as unresolved; they will not be replaced.

## 1. Held-out titles and queries — no result-driven substitutions

These are agent-selected editorial examples of series with a gay male lead,
relationship, or prominent story. The labels are provisional, not human
adjudications. Selection uses prior general media knowledge and independent
coverage of [British LGBTQ+ series](https://www.bfi.org.uk/lists/10-great-lgbt-series),
[Asian LGBTQ+ series](https://www.timeout.com/hong-kong/film/asian-lgbtq-films-and-series-that-should-be-on-your-radar),
and [Thai Boys Love series](https://www.timeout.com/bangkok/lgbtq/thai-boys-love-culture).
No title is guaranteed to appear in TMDB or any route being tested.

| # | Series identity target | First air year | Intended stratum | Initial label | TMDB ID |
|---:|---|---:|---|---|---|
| 1 | Queer as Folk (US revival) | 2022 | English | central | 134967 |
| 2 | London Spy | 2015 | English | central | 64383 |
| 3 | Big Boys | 2022 | English | central | 202851 |
| 4 | The Other Two | 2019 | English | ensemble | 85519 |
| 5 | Love in the Big City | 2024 | Korean | central | 236956 |
| 6 | The Eighth Sense | 2023 | Korean | central | 223440 |
| 7 | Cherry Magic! Thirty Years of Virginity Can Make You a Wizard?! | 2020 | Japanese | central | 111204 |
| 8 | Ossan's Love | 2018 | Japanese | central | 80051 |
| 9 | We Best Love | 2021 | Mandarin | central | 116175 |
| 10 | HIStory3: Trapped | 2019 | Mandarin | central | 303418 |
| 11 | Until We Meet Again | 2019 | Thai | central | 95279 |
| 12 | Gaya Sa Pelikula (TMDB title: Like In The Movies) | 2020 | Filipino | central | 110370 |

**Query A:** `gay-themed TV series` — all 12 titles are the independent recall
reference. **Query B:** `gay-themed Asian series` — rows 5–12 form the
predeclared regional subset. It is a diagnostic stratum, not a promise that
the current v1 parser understands “Asian” as a hard filter. The result must
report English (four) and non-English (eight) counts separately. Provider
original language is verified after identity lookup; these labels are only
preselection expectations. Visibility strata will use vote counts with the
same deterministic higher/lower-half method as the initial M0a set.

## 2. Frozen route schedule — do not tune on the held-out 12

The six exact observed names are `gay romance` 240305 · `gay relationship`
265777 · `gay` 363345 · `lgbt` 158718 · `gay theme` 258533 ·
`boys' love (bl)` 289844. Runtime use would resolve each name exactly; IDs
here identify the observed fixture, not a permanently hardcoded catalog.
All discovery requests use `language=en-US` and no adult, language, year,
or genre filter unless a lane below specifies `with_original_language`.
This experiment compares these schedules, chosen from the original 30-title
tuning results before the held-out IDs or revised output are inspected:

| Route | Page schedule in dispatch/merge order | Sort | 60-slot retention |
|---|---|---|---|
| R1 expanded global | Six-ID OR group pages 1, 2, 3 | `vote_count.desc` | First 60 distinct rows in page/rank order |
| R2 language allocation | Six-ID OR global page 1, then the same group with original language `th` page 1, `ko` page 1, `ja` page 1 | `vote_count.desc` each | Initial quotas 20 global · 14 Thai · 13 Korean · 13 Japanese; dedup by ID and fill vacant slots from unretained rows in page order |
| R3 split evidence lanes | `gay theme` page 1, BL page 1, broad `lgbt` page 1 | `vote_count.desc` each | Interleave row 1 of each list, then row 2, etc.; first 60 distinct IDs |
| R4 user seed only | For each preselected seed, recommendations page 1, similar page 1, recommendations page 2 | Provider order | Interleave rows by absolute within-list rank, rec then similar on ties, dedup by ID, exclude seed |

R4 seeds are frozen as `Looking` TMDB 57774 (English/US) and `Bad Buddy`
TMDB 122009 (Thai), both from the original tuning set. They test a
user-supplied seed route, never a generic-theme anchor catalog. If a requested
page is beyond the provider's reported end, skip it without substituting
another list. Empty pages leave the pool short. Overlapping identities count
once in the retained pool; their rank contributions can be retained by a
later ranker but do not create extra enrichment slots. No auto-retry is used.

**Frozen shallow preselection:** For each retained candidate, score only the
English `overview` from the discovery or related-series summary. Add 4 once
if a case-insensitive whole phrase matches `gay`, `homosexual`, `same-sex`,
`boys' love`, `two men`, `two boys`, `male couple`, or `men in love`; add 2
once for `queer`, `lgbt`, or `lgbtq`; add 1 once for `romance`,
`relationship`, or `fall in love`. Use Unicode case folding and word/phrase
boundaries. Do not inspect benchmark membership, title, cast, popularity,
votes, or detailed keywords when selecting. Sort by score descending, then
route merge position, then TMDB ID; enrich the first 30. Compare this with
the first 30 in merge order. For R2 the initial quota merge order is global,
Thai, Korean, Japanese, each preserving provider rank; the fill phase uses
the same lane order. This preselection is a cheap experiment, not an
admission rule. Required-theme display still needs verified detail evidence.

**Cost at the existing cold ceiling:** R1 and R3 each need up to six keyword
lookups + three discovery pages + 30 enrichments = 39 calls. R2 needs six
lookups + four pages + 30 enrichments = 40 calls. R4 needs one TMDB seed
detail + three related pages + 30 enrichments = 34 calls; unique TVDB mapping
may add one detail = 35. A combined theme+seed R2 variant would need six
lookups + four theme pages + three related pages + one seed detail + only
27 enrichments = 41 calls; TVDB mapping would reduce enrichments to 26.
No retry allowance is hidden: automatic retries are zero, and a provider 429
uses the existing cooldown/partial failure path. The combined reduction is
an explicit proposed contract change, not an implementation instruction.
The 60-candidate cap applies after deduplication across theme and seed paths;
their 30/30 reservation and fill rule from the plan remains the comparison
baseline. The R2 language quotas are likewise a diagnostic candidate
allocation, not an approved v1 filter policy.

The experiment will measure both pool recall (in retained 60) and
post-selection recall (in 30 enrichments), separately for the original tuning
set and the frozen 12. It will report missing keyword membership and language
and vote-visibility splits. After viewing held-out output, these schedules and
preselection weights remain fixed. Candidate summaries may guide selection,
but cannot establish displayed theme claims. A seed-derived list will be
evaluated as seed retrieval; its success cannot stand in for arbitrary
theme-only search.

## 3. Observed retrieval — exact aliases do not make the pool

The credential-free [revision fixture](smart-media-discovery-m0a-revision-fixture.json)
retains exact identity matches, keyword IDs, request parameters, ordered page
rows, preselection features/scores, deduplicated pool IDs, selected IDs, and
detail-read outcomes. The provider requests were read-only on 2026-09-30 UTC
using Curator's configured TMDB credential in remote process memory. No key,
authenticated URL, or full synopsis is retained. All six exact keyword
lookups, 12 held-out details, and 15 distinct page calls succeeded: 33 calls,
zero errors, no retries. A separate offline check fetched the 127 unique
identities chosen by either 30-slot selection order across routes; all 127
detail requests returned the requested TMDB ID. These offline calls are
evaluation work, not a single interactive search or evidence that a 41-call
deadline was met.

The six aliases cover 28/30 original tuning titles and 10/12 held-out
titles. Only `Our Flag Means Death` 109939 and `Schitt's Creek` 61662 remain
outside the tuning-set alias union; `London Spy` 64383 and `The Other Two`
85519 remain outside the held-out union. Those four have valid detail
responses with other keywords. They remain in the recall denominators.

| Route and set | Raw rows / retained distinct | Pool recall | First 30 by provider/merge order | First 30 by frozen synopsis score | Detail success for selected IDs |
|---|---:|---:|---:|---:|---:|
| R1 expanded global · tuning 30 | 60 / 60 | 6/30 | 3/30 | 5/30 | 30/30 per order |
| R1 expanded global · held-out 12 | 60 / 60 | 0/12 | 0/12 | 0/12 | 30/30 per order |
| R2 language allocation · tuning 30 | 80 / 60 | 5/30 | 4/30 | 3/30 | 30/30 per order |
| R2 language allocation · held-out 12 | 80 / 60 | 3/12 | 0/12 | 1/12 | 30/30 per order |
| R3 split evidence lanes · tuning 30 | 60 / 54 | 7/30 | 6/30 | 6/30 | 30/30 per order |
| R3 split evidence lanes · held-out 12 | 60 / 54 | 1/12 | 0/12 | 0/12 | 30/30 per order |
| R4 Looking seed · tuning 30 | 60 / 59 | 7/30 | 3/30 | 6/30 | 30/30 per order |
| R4 Looking seed · held-out 12 | 60 / 59 | 0/12 | 0/12 | 0/12 | 30/30 per order |
| R4 Bad Buddy seed · tuning 30 | 60 / 60 | 3/30 | 1/30 | 1/30 | 30/30 per order |
| R4 Bad Buddy seed · held-out 12 | 60 / 60 | 0/12 | 0/12 | 0/12 | 30/30 per order |

“First 30” is **post-detail identity reachability** in this experiment:
those 30 identities were selected and their read-only `/tv/{id}` details
succeeded. It is not evidence-gated displayed recall; no theme/centrality
classifier or model was implemented. The table separates loss before the
pool, loss at 60-slot retention, loss at 30-slot selection, and detail-read
failure (zero here). The details were checked offline across routes; a
production request would fetch only its own selected IDs under §2's budget.

For R2, the four page responses held 80 distinct identities. Four held-out
titles were in those raw rows: `Love in the Big City` 236956, `The Eighth
Sense` 223440, `Cherry Magic!` 111204, and `Until We Meet Again` 95279.
The fixed Korean quota dropped `The Eighth Sense` (rank 16 of 20); 3/12
remained in the 60-slot pool. The frozen synopsis score chose only `Love in
the Big City` for detail. The other two retained titles had score zero from
their English summary text, so cheap lexical preselection did not rescue
them. This is a measured selection failure, not evidence that those works
lack the requested theme.

| Held-out title | TMDB ID | Original language | TMDB votes | Six-alias member | R1 pool/detail | R2 pool/detail | R3 pool/detail |
|---|---:|---|---:|---|---|---|---|
| Queer as Folk (2022) | 134967 | en | 47 | yes | —/— | —/— | —/— |
| London Spy (2015) | 64383 | en | 141 | no | —/— | —/— | —/— |
| Big Boys (2022) | 202851 | en | 48 | yes | —/— | —/— | —/— |
| The Other Two (2019) | 85519 | en | 73 | no | —/— | —/— | —/— |
| Love in the Big City (2024) | 236956 | ko | 48 | yes | —/— | yes/yes | —/— |
| The Eighth Sense (2023) | 223440 | ko | 34 | yes | —/— | —/— | —/— |
| Cherry Magic! (2020 live action) | 111204 | ja | 89 | yes | —/— | yes/— | yes/— |
| Ossan's Love (2018) | 80051 | ja | 27 | yes | —/— | —/— | —/— |
| We Best Love (2021) | 116175 | zh | 49 | yes | —/— | —/— | —/— |
| HIStory3: Trapped (2019) | 303418 | zh | 6 | yes | —/— | —/— | —/— |
| Until We Meet Again (2019) | 95279 | th | 67 | yes | —/— | yes/— | —/— |
| Like In The Movies (2020) | 110370 | tl | 18 | yes | —/— | —/— | —/— |

For the held-out English four, every route had 0/4 in the pool despite
two keyword-covered titles. For the non-English eight, R1 had 0/8 pool and
selected, R2 had 3/8 pool and 1/8 selected, and R3 had 1/8 pool and 0/8
selected. For held-out vote visibility, the stable higher/lower halves are
six each (48 votes at the split, ID breaks the tie). R2 reached 2/6 higher
and 1/6 lower in the pool, then 0/6 and 1/6 after selection. R1 reached
neither half. On the original tuning set, R1 reached 6/15 higher-vote and
0/15 lower-vote; R2 reached 2/15 and 3/15; R3 reached 4/15 and 3/15.
The regional quotas trade some familiar English results for Asian-language
ones, but omit Mandarin and Filipino lanes entirely and do not achieve
general discovery recall.

## 4. Decision requested — retrieval source or evaluation contract revision

The best frozen theme-only route on the original tuning set is R3 at 7/30
pool and 6/30 selected. On the untouched 12-title set it is 1/12 pool and
0/12 selected. R2 is 3/12 pool and 1/12 selected. These outcomes are far
below the plan's proposed 80% held-out thematic recall; no numeric target
relaxation is made here. The six-alias membership of 10/12 shows the principal
loss is page ordering and finite pool allocation, followed by 30-slot
selection for the regional route. The two held-out titles without aliases
also require a different retrieval source if the generic query should find
them. Similarity inference cannot correct either problem.

**Proposed next design gate:** review a genuinely additional theme candidate
source or a separately justified persistent editorial index, then test it
against freshly frozen titles before packaging the helper. An anchor index
would be a product change with catalog maintenance, provenance, and source
independence obligations; it must not import either benchmark set as
guaranteed candidates. Another option is to limit v1 to user-selected seed
discovery and change the stated generic-theme outcome, which would require
the owner's explicit scope decision. The measured seed examples differ
sharply and both miss all held-out titles, so they do not validate that
alternative yet. Simply spending 41 calls on deeper versions of these
routes without a new selection/source design is not supported by the
measurements. M0b and M1 remain stopped pending review.
