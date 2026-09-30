# Smart Media Discovery M0c: first real-model ranking probe

**Status:** tuning evidence only; held-out corpus and workload gate pending.
This follows the [R3 retrieval contract](plan-smart-media-discovery.md#5-retrieval--build-a-relevant-bounded-candidate-pool)
frozen at `f758242`. It tests whether the standalone MiniLM encoder changes
the order of previously inspected R3 theme and Looking seed candidates. It
does not implement the API, a full cold search, or a release-quality evidence
classifier.

## Method and provenance

Run `tools/curator-embed/probe-ranking.py` with the pinned helper and model,
plus a temporary JSON file of TMDB details:

```sh
python3 tools/curator-embed/probe-ranking.py \
  --details /path/to/temporary-tmdb-details.json \
  --model-dir /path/to/verified-model
```

The temporary input contains the 59 distinct identities in the fixed R3 and
Looking 30-row selections, plus the Looking seed (60 requests). All 60
`/tv/{id}?append_to_response=keywords` reads succeeded on 2026-09-29 through
Curator's configured TMDB key, kept only in nuc3 process memory. Full provider
overviews stay in the temporary file, outside the repository. The retained
[M0a route fixture](smart-media-discovery-m0a-revision-fixture.json) supplies
candidate IDs and list ranks; the
[annotation record](smart-media-discovery-m0a-annotations.md) supplies
provisional 0–3 relevance grades for reporting only. These cases influenced
route selection and are **tuning data**. The earlier 12-title set is only a
coverage diagnostic, not untouched ranking validation.

The probe's `m0c-probe-v1` text puts up to 16 alphabetical provider keywords
and genres before the synopsis, excluding title, cast, votes, ownership and
labels. The theme Q is “Stories about gay men and relationships between men.”
Looking S is encoded from its provider facts. R3's provisional admission
requires a direct narrow provider keyword or an explicit narrow synopsis
phrase; broad `lgbt` alone is insufficient. The probe makes **no centrality
claim** and treats all admitted R3 rows as `present`. Looking seed-only has no
gay-male hard gate. The metadata baseline sorts by reciprocal provider-list
rank, then TMDB ID. Semantic ordering sorts by cosine, then the same rank and
ID. Both orders use the identical admitted candidates. No relevance grade
enters admission, text construction, or sorting.

The fixed R3 selected 30 are the §5 theme-path shallow selection. The fixed
Looking 30 came from the historical gay-phrase R4 experiment, not the newly
specified seed-only provider-order preselection. This is a focused model
probe, not a measurement of that normative seed route. No seed cutoff is
applied yet. nDCG@10 uses `2^grade - 1` gain and logarithmic discount over the
eligible selected pool; grade at least 2 counts as suitable for P@10.

## Observed order and quality

| Route | Selected / evidence-eligible / suitable | Metadata P@10 · nDCG@10 | Semantic P@10 · nDCG@10 |
| --- | ---: | ---: | ---: |
| R3 gay male | 30 / 23 / 13 | 0.50 · 0.6031 | 0.70 · 0.8013 |
| Looking seed | 30 / 30 / 11 | 0.50 · 0.3224 | 0.60 · 0.6404 |

R3, rank order. Parentheses contain the provisional full-query grade:

| # | Metadata-only | Semantic |
| ---: | --- | --- |
| 1 | Heartstopper (3) | TharnType (3) |
| 2 | Elite (1) | Love By Chance (3) |
| 3 | Sex Education (1) | Heartstopper (3) |
| 4 | Heated Rivalry (3) | Sex Education (1) |
| 5 | Love, Victor (3) | 2gether: The Series (3) |
| 6 | Riverdale (1) | Smiley (3) |
| 7 | given (3) | Merlí (1) |
| 8 | Yuri!!! on Ice (1) | Semantic Error (3) |
| 9 | Ginny & Georgia (1) | Eyewitness (3) |
| 10 | Love By Chance (3) | Elite (1) |

Looking seed, rank order:

| # | Metadata-only | Semantic |
| ---: | --- | --- |
| 1 | Sex and the City (1) | The L Word (2) |
| 2 | Presidio Med (0) | The L Word: Generation Q (2) |
| 3 | Love, Victor (2) | Mid-Century Modern (3) |
| 4 | Malcolm & Eddie (0) | Rain Dogs (2) |
| 5 | The L Word (2) | EastSiders (3) |
| 6 | Tales of the City, 1993 (2) | Sex and the City (1) |
| 7 | El Chavo del Ocho (0) | The Newsreader (1) |
| 8 | The L Word: Generation Q (2) | Uncoupled (3) |
| 9 | That '70s Show (0) | Sexo Frágil (0) |
| 10 | Mid-Century Modern (3) | Malcolm & Eddie (0) |

The helper encoded 61 texts (theme Q, Looking S, and 59 distinct candidates)
in 1.783 seconds including process startup and model load on local macOS
arm64. This is a single run, not a p95 or a provider/search latency claim.
Both routes show useful nDCG lift on tuning data. Both still miss the 0.80
P@10 target; weak generic adult-friendship overlap remains in Looking's top
ten, and narrow TMDB tags without a strong main-story fit remain in R3.
No cutoff or boost was tuned from these grades in this probe.

## Next evaluation gate

Freeze at least 20 queries and 100 distinct series with tuning/held-out split
before inspecting held-out ranking. Include the required theme, seed, combined,
filter, region, missing-evidence, negation, thin-seed and non-LGBTQ strata.
Keep provider evidence separate from independent relevance grades and send
consequential disagreements for adjudication. Tune only on the tuning split,
then compare the frozen baseline and model on the held-out split and measure
the one-cold-search resource budget. These initial two routes do not validate
the final feature.
