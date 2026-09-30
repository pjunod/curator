# Smart Media Discovery M0c: evidence and shallow-input correction

**Status:** tuning evidence. This corrects the [v2 historical run](smart-media-discovery-m0c-tuning-v2.md)
after reviewer counterexamples. Neither run is a held-out quality pass.

## Input boundary and evidence

The Looking shallow selector now reads captured `/tv/57774/recommendations`
pages 1 and 2 and `/tv/57774/similar` page 1, taken before enrichment. Their
59 distinct identities exactly match the frozen Looking pool. At capture on
2026-09-29, all 59 summary overview strings matched subsequent detail
overview strings byte for byte. This establishes equivalence for this
diagnostic snapshot; the selector uses the summary values regardless. Detail
keywords are seen only after the 30 slots are selected. Provider text is in
temporary local files and the identity-only [110-series snapshot](smart-media-discovery-m0c-identity-snapshot.json)
is retained in the repository. All 110 detail requests succeeded. This is
an identity snapshot, not 110 independently judged evaluation participants:
the 24-query labeled corpus and its distinct-series floor remain incomplete.

`evidence-proxy-v2` removes broad centrality patterns. A gay person mentioned
in an unrelated story remains `present`; two men falling for the same woman
without a narrow keyword is `unknown`. The probe asserts five adversarial
examples, including one quotation and one denial. These recorded examples
do not receive centrality; the anchored templates have not been proven safe
for every quoted, denied or incidental description. Provider keywords alone
never promote `central`. The revised
R3 pool contains 7 unknown, 22 present and 1 central selected row. This is
still a narrow tuning proxy, not a production evidence classifier.

Reproduce with the verified MiniLM model, the private temporary detail input,
and the captured pre-enrichment list response:

```sh
python3 tools/curator-embed/probe-ranking-v2.py \
  --details /tmp/monarr-m0c-probe-extended-details.json \
  --summaries /tmp/monarr-m0c-additional-and-summaries.json \
  --model-dir /tmp/monarr-m0b-model
```

## Corrected comparison

| Pool and ranking | Suitable in pool | Metadata P@10 / nDCG@10 | Semantic P@10 / nDCG@10 |
| --- | ---: | ---: | ---: |
| R3 fixed theme selection, 23 eligible | 16 | 0.70 / 0.7087 | 1.00 / 0.8771 |
| Looking provider-order 30 | 9 | 0.50 / 0.3933 | 0.70 / 0.7593 |
| Looking semantic-shallow 30 | 14 | 0.70 / 0.4728 | 0.70 / 0.5867 |

The metadata ranker in the last row is a *same-pool ranking ablation*:
it already benefits from semantic preselection. It is not the complete
model-off system. The complete model-off pipeline is provider-order selection
plus metadata rank, the middle-row metadata result. A fair deployed comparison
must separately measure complete model-off and model-on pipelines, then use
same-pool ablations to attribute ranking effects.

Looking provider-order selection cannot produce ten suitable options from
its current pool. Semantic shallow selection offers fourteen, but the plain
cosine ranking still returns seven suitable in its top ten. The exploratory
Looking queer-affinity bonus from v2 is not selected. A general provider topic
feature requires multiple seeds and a metadata-plus-topic comparator before
any model benefit is attributed to it.

The local helper took 1.317 seconds for 60 shallow texts and 2.133 seconds
for 77 detailed texts in this multi-route experiment. The latter includes R3
and the union of both Looking selection alternatives. These numbers cannot be
used as one-request latency. The resource harness must measure each actual
request's query, optional seed, selected detail texts, helper state and
provider delay separately.

## Remaining gate

Capture and grade candidates for the other tuning/held-out strata, freeze
retrieval, admission, text, score and cutoff versions, then inspect held-out
orders once. The 110-series identity snapshot alone is insufficient for the
planned 24-query evaluation. M1 remains gated on held-out quality and a
realistic one-cold workload result.
