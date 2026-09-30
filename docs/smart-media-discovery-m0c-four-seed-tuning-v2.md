# Smart Media Discovery M0c: generic four-seed tuning

**Status:** tuning only, before any held-out model order. This supersedes the
[per-seed-text experiment](smart-media-discovery-m0c-four-seed-tuning.md).
The two auxiliary non-LGBTQ query intents, full related-list pools and
provisional agent grades were frozen before their first model ordering.

## One shared rule

`seed-topic-v2` uses exact normalized provider keyword aliases and contextual
overview phrases. It does not treat “two women,” “space” as room size, or a
school-club president as topic evidence. Eight adversarial/positive assertions
run on import. Topic evidence is a **soft affinity**, never required-theme
admission. Multiple recognized topics or no recognized topic yield `none` and
zero topic bonus. The real Tales of the City 2019 seed has multiple topics;
Presidio Med has none. Both have sufficient description and keywords for
semantic comparison. Only fewer than eight lexical tokens in overview plus
the bounded approved keywords triggers `seed_similarity_unavailable` and the
plan's provider-only fallback. Topic ambiguity does not make a seed thin.

`provider_text-v2` is the same ID-independent function for every seed and
candidate: up to sixteen provider keywords, with exact vocabulary aliases
and a fixed context-keyword list before remaining alphabetical keywords,
then genres and synopsis. It excludes titles, cast, popularity, ownership and
grades. Keywords and genres are individually bounded, then the synopsis uses
the remaining bytes up to the helper's 8,192-byte per-text limit. A UTF-8
prefix operation preserves valid multibyte boundaries and the priority
fields. Captured shallow summaries are capped to the same limit; empty
summaries receive no vector and fall behind scored rows with provider-order
ties. This byte cap is separate from the encoder's 256-token truncation.
Long-keyword, long-overview and multibyte assertions pass; ordinary corpus
texts remain unchanged by the cap. The shallow selector reads only
pre-enrichment list overviews. For
each resolved seed topic, semantic shallow selection encodes the seed and up
to sixty summaries, then selects thirty with provider order and ID ties.
The detailed semantic score is cosine plus `0.06` for a provider-derived
candidate topic match; the metadata comparator is reciprocal provider rank
plus `0.02` for that **same** match. Both values were chosen on tuning data.
An unresolved topic gives both comparators a zero topic component while
usable seed semantics can still drive cosine. A genuinely thin seed uses the
provider-only route.

| Seed / topic | Full-pool suitable | Provider-order 30 suitable | Semantic-shallow 30 suitable |
| --- | ---: | ---: | ---: |
| Looking / queer | 19 / 59 | 9 | 15 |
| Bad Buddy / queer | 21 / 60 | 13 | 15 |
| Star Trek: TNG / space exploration | 25 / 55 | 17 | 20 |
| House of Cards / political drama | 25 / 60 | 12 | 18 |

Both rankers below operate on each seed's **identical semantic-selected 30**.
Selected nDCG uses the ideal over that 30; full-pool nDCG uses the ideal over
all 55–60 independently frozen pool grades. P@10 treats grade ≥2 as suitable.

| Seed | Metadata+topic P@10 / selected nDCG / full nDCG | Semantic+topic P@10 / selected nDCG / full nDCG |
| --- | ---: | ---: |
| Looking | .90 / .6663 / .6309 | 1.00 / .7144 / .6765 |
| Bad Buddy | .90 / .8465 / .8465 | 1.00 / .9173 / .9173 |
| Star Trek: TNG | .80 / .7410 / .7141 | 1.00 / .9108 / .8777 |
| House of Cards | .90 / .7103 / .6576 | 1.00 / .8931 / .8268 |

Mean selected-pool nDCG lift is .1179 and mean full-pool lift is .1123 on
these four **tuning** seeds. P@10 improves on all four. This does not validate
unseen seeds, provider latency, filters or release quality. The eight-token
thin-seed fallback, score cutoffs and combined Q/S weights have not been
qualified by this table. Model-off full-pipeline quality must be compared
separately from this same-selected-pool ranking ablation.

The shallow pass encoded 238 texts across four experiments in about 4.5
seconds on local macOS arm64. Four separate detailed union experiments took
about 1.4–1.6 seconds each. These are **not** one-request latency numbers:
each detailed union includes alternative selected sets. One actual search's
shallow and selected-detail batches, helper lifecycle, Go/provider costs and
two-core contention remain to be measured.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 tools/curator-embed/probe-seed-selection.py \
  --details /tmp/monarr-m0c-450-details.json \
  --looking-pages /tmp/monarr-m0c-additional-and-summaries.json \
  --extra-pages /tmp/monarr-m0c-extra-route-pages.json \
  --aux-pages /tmp/monarr-m0c-nonqueer-seed-pages.json \
  --model-dir /tmp/monarr-m0b-model
```

The next route freeze should apply this generic rule to full candidate pools,
resolve query-specific held-out labels before model order, then run held-out
once. Full provider summaries and details remain in temporary local inputs;
the repository retains identities, provisional labels, code and findings.
