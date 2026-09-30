# Smart Media Discovery M0c: corrected tuning probe

**Status:** historical tuning run at `dcd3b0f`, before the evidence-proxy-v2
correction. The current probe script requires `--summaries` and produces
different R3 metrics. The [first probe](smart-media-discovery-m0c-probe.md)
remains a historical result. This probe uses a stronger provider-only centrality
proxy, the intended Looking provider-order selection, and a separately versioned
semantic shallow-selection experiment. The [reviewer adjudications](smart-media-discovery-m0c-adjudications.json)
change relevance labels for four cases in *both* ranking methods; they do not
change runtime provider evidence, admission, centrality, or model input.

## Reproduce

```sh
python3 tools/curator-embed/probe-ranking-v2.py \
  --details /path/to/temporary-tmdb-details.json \
  --model-dir /path/to/verified-model
```

The temporary input has all 59 Looking seed-pool identities, the R3 selected
identities, and the seed (89 distinct detail rows). All provider details were
read successfully on 2026-09-29. Full overviews remain outside this repository.
The 29 additional Looking grades were [frozen](smart-media-discovery-m0c-looking-extra-labels.json)
before semantic shallow selection. The older 30 grades come from the M0a
annotations. Grades enter metrics only. The same candidates and evidence tiers
are used by the baseline and semantic order for each comparison.

`retrieval-r3-v1` retains the 60-pool and 30-detail limits.
`evidence-proxy-v1` admits narrow provider tags or synopsis evidence, assigns
central only for explicit overview patterns, and assigns present otherwise.
This conservative proxy is not a validated production classifier. R3 has
7 unknown, 21 present and 2 central selected rows. `text-priority-v2` places
approved theme/seed keyword signals before generic keywords in the 16-keyword
text limit. On these 89 rows it loses no prioritized theme signals to that
limit; the older alphabetical policy lost three. The model text excludes
titles, cast, ratings, ownership and relevance labels. `seed-preselect-v1`
encodes up to 60 *overview summaries* before selecting 30 detail enrichments;
detail keywords are unavailable at selection time. Ties use provider order
and ID. Both modes use the same fixed reciprocal provider-rank metadata tie
break after their leading score. P@10 requires ten returned results; short
lists report P@k and a null P@10. Zero-ideal nDCG is flagged separately.

## Results after adjudication

| Route and 30-slot selection | Eligible / suitable | Metadata P@10 / nDCG@10 | Semantic P@10 / nDCG@10 |
| --- | ---: | ---: | ---: |
| R3 theme, fixed lexical selection | 23 / 16 | 0.80 / 0.7831 | 1.00 / 0.8771 |
| Looking seed, normative provider order | 30 / 9 | 0.50 / 0.3933 | 0.70 / 0.7593 |
| Looking seed, semantic shallow experiment | 30 / 14 | 0.70 / 0.4728 | 0.70 / 0.5867 |

Before adjudication, the corresponding R3 baseline/semantic P@10 values were
0.60/0.70 and nDCG values 0.7317/0.8157. The semantic shallow Looking
comparison was 0.70/0.60 P@10 and 0.4728/0.5640 nDCG. The normative Looking
provider-order list does not contain ten suitable suggestions even with ideal
ordering. The shallow experiment corrects candidate sufficiency, but its
unmodified semantic top ten still misses 0.80. This is a tuning failure to
resolve before freezing scoring; it is not a held-out pass.

An exploratory Looking-only soft queer-affinity score was scanned at cosine
bonuses 0.02, 0.04, 0.06 and 0.08. It uses provider keywords/overview only,
not labels. At 0.06, the shallow-selected top ten reaches 0.90 P@10 and
0.7016 nDCG, but this threshold was inspected against tuning grades and may
overfit. No bonus is frozen or generalized to other themes. A seed without
an LGBTQ provider signal must follow a different resolved topic policy.

The local macOS arm64 helper encoded 60 shallow texts in 1.277 seconds and
77 detail/query/seed texts in 2.091 seconds, 3.369 seconds total over two
process starts. This is total *experiment* time: it includes two alternative
Looking selections and the R3 experiment. It is not the encoder time for one
user search. It also excludes provider latency, Go orchestration, contention
and result handling. Per-request timing needs measurement in the one-cold
workload. The temporary Linux test containers
from M0b were stopped and removed; this local probe started no service.

## Next gate

Freeze at least 100 distinct real-provider identities, independent relevance
grades and complete route/scoring/cutoff versions for the 24-query split
before viewing held-out model order. Evaluate tuning queries beyond R3 and
Looking before selecting any bonus. Then run held-out quality and the real
limiter/scheduler resource harness, including title-search contention, 429,
deadline/partial behavior and combined memory. M1 remains gated on that
evidence and reviewer approval.
