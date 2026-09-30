# Smart Media Discovery M0c: four-seed tuning comparison

**Status:** historical `e891d17` tuning run with per-seed keyword lists and
overly broad topic patterns; superseded by the
[generic-text correction](smart-media-discovery-m0c-four-seed-tuning-v2.md).
No held-out model order has been inspected. The
[auxiliary seed identities](smart-media-discovery-m0c-aux-seeds.json) and
[agent-assigned provisional grades](smart-media-discovery-m0c-aux-seed-labels.json)
for Star Trek: The Next Generation and House of Cards were frozen before model
ordering. Looking and Bad Buddy use their earlier frozen full-pool grades.
Independent editorial checks remain necessary for consequential disputes.

## Candidate selection and scores

Each seed uses one detail response and three related-TV pages, merged by
absolute provider-list rank and deduplicated to at most 60 IDs. Provider-order
selects the first 30. `seed-topic-v1` infers a soft topic from the seed's
overview and keywords; an unresolved or multiple-topic seed returns `none`.
The overview-topic alternative scores *only captured list summaries* and
selects 30 with provider order and ID ties. The semantic-shallow alternative
encodes the same summaries and selects the top 30 by cosine to the seed.
Neither summary alternative sees candidate detail keywords before selection.
The seed text uses bounded provider keywords, genres and synopsis; no grade,
title, cast, popularity or ownership enters the vectors.

The detailed ranking comparison uses the same selected 30 for both modes.
The metadata comparator sorts by `sum(1/(60+absolute list rank))` plus a
soft `0.02` provider-topic match, then ID. The semantic sort uses seed cosine
plus `0.06` for that same match, then reciprocal rank and ID. A topic match
comes only from candidate detail keywords or synopsis, not relevance labels;
it is a soft score, never required-theme admission. Both weights were chosen
on these tuning examples, so their apparent lift may overfit.

| Seed / resolved topic | Suitable in full pool | Provider 30 | Overview-topic 30 | Semantic-shallow 30 |
| --- | ---: | ---: | ---: | ---: |
| Looking / queer | 19 / 59 | 9 | 14 | 14 |
| Bad Buddy / queer | 21 / 60 | 13 | 14 | 16 |
| Star Trek: TNG / space exploration | 25 / 55 | 17 | 19 | 20 |
| House of Cards / political drama | 25 / 60 | 12 | 18 | 18 |

On the **semantic-shallow selected 30**, the selected-pool nDCG ideal uses
only those 30 grades; the full-pool ideal uses all 55–60 frozen grades. This
keeps pure reranking and end-to-end selection effects distinct.

| Seed | Metadata+topic P@10 / selected nDCG / full nDCG | Semantic+topic P@10 / selected nDCG / full nDCG |
| --- | ---: | ---: |
| Looking | .90 / .6789 / .6428 | .90 / .7016 / .6644 |
| Bad Buddy | 1.00 / .8301 / .8301 | 1.00 / .8805 / .8805 |
| Star Trek: TNG | .70 / .6707 / .6463 | 1.00 / .9138 / .8805 |
| House of Cards | .80 / .6259 / .5795 | 1.00 / .8868 / .8210 |

Mean selected-pool nDCG lift on these four tuning seeds is .1443; mean
full-pool lift is .1369. Looking alone improves by only .0227/.0216, below
the proposed .03 mean target if treated as a single-query threshold. There is
no P@10 loss on Looking. The broad topic feature itself is material: the
metadata-plus-topic comparator is stronger than provider rank alone for the
queer seeds, and the model's incremental effect is measured against it.

The overview-topic selector is cheaper and sufficient on the two queer seeds.
On Star Trek, its semantic rerank returns .80 P@10 while the same-pool
metadata order returns .90; semantic shallow selection plus semantic rerank
returns 1.00. A general rule cannot be chosen from the two queer cases alone.
The current working direction is semantic shallow selection for resolved
topics and a documented provider-order fallback when the topic is unresolved;
that is **not yet the final frozen route**. The real Tales of the City 2019
seed resolves to multiple topics in the current classifier, and Presidio Med
has no recognized topic. Neither case is assigned a guessed queer bonus.

The four-seed shallow encoding experiment processed 238 texts in 4.503
seconds on local macOS arm64, then separate detailed union experiments took
1.394–1.551 seconds per seed. These are experiment timings over multiple
seeds and alternative candidate sets, not one user request or a p95. A
production resource run must measure one seed's actual shallow and selected
detail batches, helper lifecycle, provider calls and contention on a two-core
host before accepting latency.

## Reproduction and next step

```sh
PYTHONDONTWRITEBYTECODE=1 python3 tools/curator-embed/probe-seed-selection.py \
  --details /tmp/monarr-m0c-450-details.json \
  --looking-pages /tmp/monarr-m0c-additional-and-summaries.json \
  --extra-pages /tmp/monarr-m0c-extra-route-pages.json \
  --aux-pages /tmp/monarr-m0c-nonqueer-seed-pages.json \
  --model-dir /tmp/monarr-m0b-model
```

The temporary provider inputs contain full summaries/details and remain
outside the repository. After reviewer inspection, freeze the selected
retrieval/topic/score/fallback policy, recapture and grade every held-out
candidate in its actual input pool, then inspect held-out model order once.
The flawed v1 24-query candidate construction remains a historical diagnostic
and cannot support a release conclusion.
