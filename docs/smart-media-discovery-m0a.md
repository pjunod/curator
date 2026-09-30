# Smart media discovery M0a — retrieval coverage evidence

**Status:** M0a early stop; retrieval contract needs revision · **Baseline:**
`d7e938ad0874be3921ec549858b028cacb84650b` · **Reference set frozen:**
`6a59dc9` on 2026-09-29 · **Sampled:** 2026-09-30 00:19 UTC.

Companion to [the discovery plan](plan-smart-media-discovery.md) §10.1 and
[its review](plan-smart-media-discovery-review.md) §6. This file preserves the
reference set before any TMDB keyword or discovery result is inspected.
Membership below was selected by this agent before any TMDB keyword or
discovery result was inspected. It is an editorial starting point, not a
human-adjudicated set or provider evidence label. TMDB identities were matched
by title, first-air year, and origin country; thematic labels await
human/reviewer adjudication. The credential-free ordered page IDs,
reference keywords, and exact request parameters are retained in
[the fixture](smart-media-discovery-m0a-fixture.json). No model or feature code
was built.

## 1. Reference set — 30 preselected series

The set deliberately mixes older/newer work and productions from North America,
Europe, Australia, and Asia. `Central` means the male same-sex relationship or
gay male experience is understood to drive a main story; `ensemble` means a
prominent thread shares the main story with other characters. These are
provisional agent editorial labels for this early screen. Ambiguous cases remain in the
denominator until reviewed. The title/year pair is the identity lookup target;
do not replace misses with easier TMDB matches.

| # | Series | Year | Origin | Initial label | TMDB ID |
|---:|---|---:|---|---|---|
| 1 | Queer as Folk (UK) | 1999 | UK | central | 2388 |
| 2 | Queer as Folk (US) | 2000 | US/Canada | central | 2902 |
| 3 | Noah's Arc | 2005 | US | central | 2234 |
| 4 | Looking | 2014 | US | central | 57774 |
| 5 | Cucumber | 2015 | UK | central | 61932 |
| 6 | Banana | 2015 | UK | ensemble | 61931 |
| 7 | Please Like Me | 2013 | Australia | central | 61350 |
| 8 | EastSiders | 2012 | US | central | 67202 |
| 9 | Vicious | 2013 | UK | central | 47039 |
| 10 | The Real O'Neals | 2016 | US | central | 62856 |
| 11 | Special | 2019 | US | central | 87194 |
| 12 | It's a Sin | 2021 | UK | central | 116174 |
| 13 | Heartstopper | 2022 | UK | central | 124834 |
| 14 | Young Royals | 2021 | Sweden | central | 125910 |
| 15 | Love, Victor | 2020 | US | central | 97186 |
| 16 | Our Flag Means Death | 2022 | US | central | 109939 |
| 17 | Fellow Travelers | 2023 | US | central | 216089 |
| 18 | Smiley | 2022 | Spain | central | 214609 |
| 19 | Uncoupled | 2022 | US | central | 201380 |
| 20 | Interview with the Vampire | 2022 | US | central | 128098 |
| 21 | Schitt's Creek | 2015 | Canada | ensemble | 61662 |
| 22 | Will & Grace | 1998 | US | ensemble | 4454 |
| 23 | The New Normal | 2012 | US | central | 44005 |
| 24 | Tales of the City | 2019 | US | ensemble | 87731 |
| 25 | 2gether: The Series | 2020 | Thailand | central | 99631 |
| 26 | I Told Sunset About You | 2020 | Thailand | central | 106840 |
| 27 | Bad Buddy | 2021 | Thailand | central | 122009 |
| 28 | Gameboys | 2020 | Philippines | central | 103831 |
| 29 | Semantic Error | 2022 | South Korea | central | 157208 |
| 30 | Old Fashion Cupcake | 2022 | Japan | central | 203990 |

The selection drew on editorial coverage of
[Queer as Folk and related shows](https://time.com/6188510/queer-as-folk-gay-television/),
the [Television Academy's LGBTQ+ series guide](https://www.televisionacademy.com/features/news/online-originals/best-lgbtq-tv-shows),
[BFI's LGBTQ+ series list](https://www.bfi.org.uk/lists/10-great-lgbt-series),
[GLAAD's 2024–25 TV report](https://assets.glaad.org/m/b93f9f3873eff34/original/GLAAD-2024-25-Where-We-Are-on-TV.pdf),
and [Time Out's Asian series coverage](https://www.timeout.com/hong-kong/film/asian-lgbtq-films-and-series-that-should-be-on-your-radar).
They supplied candidate titles and cross-checks, not TMDB keyword results.
The individual thematic labels above are provisional agent judgments and
must be adjudicated before the later held-out quality evaluation.

## 2. Measurement contract — no substitutions after lookup

Resolved the four proposed `gay_male` aliases (`gay romance`, `gay relationship`,
`gay`, `lgbt`) through `/search/keyword`, accepting only exact normalized names.
Fetched `/tv/{id}?append_to_response=keywords` for each verified reference ID.
Record lookup errors separately from a valid detail response with no admitted
keyword. Keyword coverage uses all 30 frozen titles as the denominator and
reports unresolved identities explicitly.

For the exact resolved ID union, fetched `/discover/tv` pages 1–10 with
`sort_by=popularity.desc`, then with `sort_by=vote_count.desc`. Specify
`with_keywords` as IDs joined by `|`, with no language, year, or genre filter
for the broad baseline. Record raw page order and IDs so hit@60/100/200 can be
recomputed. The selected-30 diagnostic uses the first 30 distinct candidates
from the production first-three-page schedule; it does not imply 200-row
interactive retrieval. Inspect top-60 details or keyword fields to count
incidental/insufficient evidence separately from explicit gay male themes.

Ran a separate filtered example using `with_original_language`,
`first_air_date.gte`, `first_air_date.lte`, and OR-joined `with_genres`, plus a
seed-only recommendations/similar example. Record exact parameters and counts;
do not merge these samples into the broad baseline.

**How to read the result:** keyword coverage below 18/30, or more than 30 of
the top 60 carrying only incidental or insufficient evidence, triggers the
plan's early stop. Passing those screens does not establish its later 80%
held-out recall target. A rise from hit@60 to hit@200 points to candidate
ordering or cap loss; it cannot repair missing keyword membership.

## 3. Baseline — metadata coverage survives, bounded retrieval fails

All four `/search/keyword` requests succeeded. Exact normalized matches were
`gay romance` 240305 · `gay relationship` 265777 · `gay` 363345 · `lgbt`
158718. The broad request used `with_keywords=240305|265777|363345|158718`,
`language=en-US`, `page=1` through `10`, and no original-language, date, or
genre filter. TMDB reported 1,270 results and returned 20 rows on each page.
All 30 reference detail requests succeeded. There were zero identity lookup,
detail, keyword, or page errors in this run.

| Measure, denominator 30 frozen series | `popularity.desc` | `vote_count.desc` |
|---|---:|---:|
| Any of the four exact keywords on reference details | 26/30 | 26/30 |
| Present in first 30 rows selected for enrichment | 2/30 | 3/30 |
| Hit in first 60 rows (three production pages) | 5/30 | 6/30 |
| Hit in first 100 rows (offline diagnostic) | 8/30 | 12/30 |
| Hit in first 200 rows (offline diagnostic) | 14/30 | 22/30 |

The four references with no **approved retrieval** keyword were `Special`
87194, `Our Flag Means Death` 109939, `Schitt's Creek` 61662, and
`Old Fashion Cupcake` 203990. These are valid detail responses with other
keywords, not lookup errors or empty keyword records. The first 30 distinct
identities are the selected-30 diagnostic for this theme-only request; there
were no duplicates in either sort's first 60 rows. The large 60→200 increase,
especially for vote count, shows order/cap loss. The four absent keyword
members cannot be repaired by more pages of the same union.

**Keyword/reference proxy, not an evidence label:** Of the popularity top
60, 38 rows were outside the frozen set and lacked an exact narrow keyword
ID (`gay romance`, `gay relationship`, `gay`, `gay theme`, or BL). The
vote-count top 60 had 41 such rows. These are reproducible *proxy/unknown*
counts from the retained fixture. They do not prove that those shows lack a
gay-male storyline or explicit synopsis evidence; the fixture does not retain
overview annotations. Reference-set membership is an evaluation observation,
never a production admission rule. For example, `Only Murders in the
Building` 107113, `The L Word` 3475, and `RuPaul's Drag Race` 8514 fall in
the proxy group; `Queer as Folk` 2902 carries only broad `lgbt` despite its
agent editorial relevance label. The plan's formal over-half
incidental/insufficient-evidence cutoff remains **unverified** pending
independent row-level thematic review. The measured 5/30 pool and 2/30
selection loss alone justify stopping this retrieval contract.

**Language breakdown:** TMDB reports 22 reference titles with original
language `en` and eight with another original language. Keyword membership
was 19/22 and 7/8 respectively. Popularity first-60 hits were 5/22 and
0/8; vote-count first-60 hits were 5/22 and 1/8. At 200 rows, popularity
reached 13/22 and 1/8; vote count reached 16/22 and 6/8. This diagnostic
shows the top pool especially loses non-English titles. Original language
is provider metadata, not a judgment about the availability of an English
synopsis.

**Visibility breakdown:** To avoid a subjective “obscure” label, divide the
frozen set at its TMDB vote-count median, with stable ID tie-breaking:
15 higher-vote titles (124–1,777 votes) and 15 lower-vote titles (19–119
votes). Both groups have 13/15 approved-keyword membership. Popularity's
first 60 contains 5/15 higher-vote and 0/15 lower-vote titles; vote count's
first 60 contains 6/15 and 0/15. At 200, popularity reaches 9/15 and 5/15;
vote count reaches 13/15 and 9/15. This split is descriptive, not a
predeclared statistical threshold; the exact vote counts are in the fixture.

## 4. Alternative routes — none rescues the three-page cap

Two additional exact `/search/keyword` matches were observed:
`gay theme` 258533 and `boys' love (bl)` 289844. These are diagnostic
aliases, not a silent change to the approved vocabulary. Each row below
uses `sort_by=popularity.desc`, `language=en-US`, no other filters, and up
to three 20-row pages. `Selected 30` means first 30 distinct rows after
the stated page order.

| Keyword ID group (`|` means OR) | TMDB total | First 60 distinct | Reference hit@60 | Selected-30 hit |
|---|---:|---:|---:|---:|
| 240305\|265777\|363345 (approved narrow only) | 466 | 60 | 2/30 | 2/30 |
| 240305\|265777\|363345\|258533 (add gay theme) | 702 | 60 | 6/30 | 4/30 |
| 258533 (gay theme only) | 318 | 60 | 7/30 | 5/30 |
| 289844 (BL only) | 1,525 | 60 | 1/30 | 1/30 |
| 158718 (broad LGBT only) | 978 | 60 | 5/30 | 2/30 |

The expanded exact keywords increase reference *membership*: adding
`gay theme` to the three narrow IDs raises coverage from 7/30 to 18/30;
adding BL raises it to 19/30. Membership still does not place the works in
the first 60. A deterministic three-call combination, one page each of
`gay theme`, approved narrow, and broad LGBT, interleaving rows by position
and deduplicating on TMDB ID, yielded 53 unique candidates, 5/30 reference
hits in that pool, and 3/30 in the first 30. Replacing broad LGBT with BL
yielded 52 unique, 4/30 in the pool, and 2/30 selected. These combination
figures are exploratory tuning on the frozen set, not held-out validation.
Every combination stays at three discovery calls; none makes the current
60/30 pool adequate. Keeping `vote_count.desc` for the broad three-page
route alone yielded 6/30 at 60 and 3/30 selected, so the alternate sort
does not fix the cap either.

## 5. Filters and seed path — separate diagnostics

The filtered page used the same four-ID union and `sort_by=popularity.desc`
with `with_original_language=en`, `first_air_date.gte=2010-01-01`,
`first_air_date.lte=2025-12-31`, `with_genres=18|35` (Drama OR Comedy),
`language=en-US`, and `page=1`. TMDB reported 225 total rows. All 20 returned
rows matched the requested original language, date range, and at least one
genre locally. None was in the frozen reference set. That is a one-page
example, not a filtered recall estimate.

Seed-only calls used `Looking` TMDB 57774 with `language=en-US`, `page=1`,
and no upstream language/year/genre filters. `/tv/57774/recommendations`
returned 20 rows and five reference identities (Love, Victor 97186 ·
EastSiders 67202 · Tales of the City 87731 · Uncoupled 201380 · Banana
61931). `/tv/57774/similar` returned 20 rows and zero reference identities.
TMDB reported 719 and 20,001 total results respectively. The two paths
illustrate their distinct quality and why seed-only retrieval cannot be
assumed to replace a generic-theme route.

## 6. Disposition — stop before helper or feature implementation

The current retrieval bet loses most independently selected references:
only 5/30 enter the capped pool despite 26/30 keyword membership.
The selected-30 budget retains just 2/30. The vote-count alternative and
narrow/expanded keyword groups improve some numbers but do not repair the
bounded pool. No model can rank a missing title into it.

Revise the retrieval source or page-allocation contract before M0b. A new
candidate source must be measured against the same frozen identities plus a
held-out set, with explicit network-call and 30-enrichment costs. Ten pages
of the current broad vote-count union reached 22/30 but require seven more
discovery calls than v1 and still leave eight references absent; four keyword
lookups + ten pages + 30 enrichments would be 44 calls without seed work,
already above the 41-call cold cap. Reducing enrichments to fit would further
shrink the selected set. Merely increasing the page limit is therefore not
an accepted fix. This document requests review of a revised retrieval design;
it does not authorize the runtime spike or M1–M5.
