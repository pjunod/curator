# Smart Media Discovery M0c: frozen corpus and judgment limits

**Status:** historical v1 capture, superseded for candidate construction by
the corrected [v2 candidate freeze](smart-media-discovery-m0c-candidates-v2.json).
The v1 Korean query reused unfiltered discovery pages, and its combined
selection did not implement the documented pool/quota/fill rule. Its v1 labels
are not final evaluation truth. No held-out model order was inspected before
these defects were found. Editorial adjudication, a final route/scoring freeze,
held-out ranking and resource measurement remain pending.

## Capture

The [24-query manifest](smart-media-discovery-m0c-corpus-plan.json) was frozen
before candidate capture. The [candidate freeze](smart-media-discovery-m0c-candidates.json)
contains 266 distinct TMDB TV identities across 23 ranked queries and one
unsupported-negation refinement case. Bounded keyword lookups and result
pages were read on nuc3 using Curator's configured TMDB key without
printing it. All 266 participating series had successful detail responses.
Full provider overviews stay in local temporary files; retained files hold
candidate IDs, grades and provenance. The separate 110-series
[identity snapshot](smart-media-discovery-m0c-identity-snapshot.json) is an
earlier capture aid, not an additional 110 evaluation participants.

The existing R3 gay-male route follows the normative fixed candidate
selection. Looking uses the normative provider-order seed selection. Bad Buddy
and both Tales of the City identities use the same related-list shape as
evaluation experiments. Broad LGBTQ, lesbian and the four non-LGBTQ themes use
exact TMDB keyword lanes as exploratory retrieval fixtures; their routes are
not a shipping contract. Combined queries take fourteen IDs per path, then
stable fill to twenty-eight. These definitions were frozen without reading
embedding ranks. The captured Looking summary values matched the 59 detail
overview values byte for byte in this snapshot; the shallow selector still
reads only list summaries.

The held-out queries have 226 distinct candidate identities: 177 occur in a
tuning query and 49 are new. The split is by **query**, not by series title.
Any measured generalization must say so; repeated identities alone cannot
show title-disjoint transfer.

## Relevance judgments

The historical [label freeze](smart-media-discovery-m0c-labels.json) has 630
query-specific agent-assigned or programmatically derived judgments covering
all 266 v1 participating identities. Grade 2 or 3 means suitable. These
judgments were assigned before the model order for these queries. Four
disputed tuning cases have first-party-source
[reviewer adjudications](smart-media-discovery-m0c-adjudications.json).
The remaining labels are provisional agent judgments from provider descriptions
and prior M0a annotations, with some held-out grades programmatically carried
forward and modified by query constraints. They are distinct from runtime
admission fields but not yet independently verified against editorial sources.
Thin summaries can make a
grade uncertain. The seed-specific and non-LGBTQ tuning notes are retained in
the [combined labels](smart-media-discovery-m0c-seed-combined-tuning-labels.json)
and [other-theme labels](smart-media-discovery-m0c-other-theme-tuning-labels.json).
Consequence-bearing disputes must be resolved without showing the adjudicator
which ranker placed the work where.

| Route | Suitable / judged | Evaluation treatment |
| --- | ---: | --- |
| T01 gay-male theme | 16 / 30 | Tuning |
| T02 Looking seed | 9 / 30 | Tuning; candidate insufficiency |
| T03 Looking combined | 6 / 28 | Tuning; sparse |
| T04 gay-male, no teen | 7 / 30 | Tuning; sparse |
| T05 Bad Buddy seed | 13 / 30 | Tuning |
| T06 Bad Buddy combined | 12 / 28 | Tuning |
| T07–T12 other themes | 109 / 153 | Tuning; exploratory routes |
| H01 Asian gay-male | 7 / 30 | Held out; sparse |
| H02 Korean-language gay-male | 1 / 30 | Held out; sparse hard filter |
| H03 Looking combined, no teen | 6 / 28 | Held out; sparse |
| H04 Tales of the City 1993 seed | 3 / 30 | Held out; sparse |
| H05 Tales of the City 2019 seed | 9 / 30 | Held out; sparse |
| H06 queer, no teen | 5 / 20 | Held out; sparse |
| H07 lesbian romance | 14 / 30 | Held out |
| H08 adult coming of age | 2 / 20 | Held out; sparse |
| H09–H11 other themes | 62 / 83 | Held out; exploratory routes |
| H12 “gay stories without romance” | 0 ranked | Return `needs_refinement`; exclude from ranking averages |

Sparse queries are not counted as P@10 passes because a ranker cannot return
ten suitable titles from the frozen pool. Zero-ideal and fewer-than-ten-return
metrics require separate reporting. Synthetic parser/failure cases from the
manifest are also outside ranked-query averages.

## Reproduce the freezes

```sh
python3 tools/curator-embed/freeze-corpus-candidates.py \
  --extra-pages /tmp/monarr-m0c-extra-route-pages.json \
  --looking-pages /tmp/monarr-m0c-additional-and-summaries.json
python3 tools/curator-embed/freeze-corpus-labels.py \
  --details /tmp/monarr-m0c-302-details.json
```

The temporary files are not committed; they contain provider text and no
exposed API key. The frozen JSON files are the stable candidate and judgment
records. Every nuc3 collection was a one-shot process that exited; no test
container or service was started for this corpus.

## Next gate

Resolve consequential label uncertainty, freeze the full retrieval,
admission, text, score, cutoff and metadata comparator versions, then inspect
held-out ranking once. Report sparse coverage separately from ranked quality.
The resource harness must measure per-request encoder batches, provider
latency, rate limiting, title-search contention, second-cold rejection,
deadline/partial behavior and combined memory before M1 review.
