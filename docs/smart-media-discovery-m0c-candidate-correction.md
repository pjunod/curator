# Smart Media Discovery M0c: corrected candidate construction

**Status:** v2 construction snapshot, before held-out model ordering. This
corrects the [historical v1 corpus](smart-media-discovery-m0c-corpus.md), but
does not freeze the final seed-preselection policy. The
[v2 candidate file](smart-media-discovery-m0c-candidates-v2.json) is an audit
of the plan's then-current provider-order route, not release evidence.

H02 now requests all three R3 discovery lanes with TMDB's
`with_original_language=ko` filter. The filtered pages produce 40 distinct
shallow candidates and 30 lexical-summary selections, rather than reusing
T01's unfiltered 30. The first selected titles include *Love in the Big City*,
*Semantic Error*, *To My Star* and *Where Your Eyes Linger*. All three
requests returned successfully. The unfiltered recapture reproduces the
historical R3 pool's exact 54 IDs and selected 30; all 54 list-summary
overviews matched the corresponding temporary detail overviews byte for byte.
The selector still consumes list summaries, not detail text.

Combined construction now applies the 30/30 disjoint theme/seed quota before
enrichment. Shared IDs belong to the theme quota while retaining both source
rank contributions. Theme path items sort by the frozen lexical summary score,
then merge position and ID. It takes fourteen from each path, then alternates
remaining theme and seed items until 28 slots are filled or exhausted. The
generator checks overlap, exhausted path and 27-slot behavior. T03, T06 and
H03 therefore change IDs from v1; their v1 judgments must not be reused
without query-specific reconciliation.

| Snapshot | Distinct candidates | Held-out distinct | Seen in tuning | New to held out |
| --- | ---: | ---: | ---: | ---: |
| Flawed v1 | 266 | 226 | 177 | 49 |
| Corrected v2 | 295 | 255 | 177 | 78 |

The original-language filter is an upstream-supported hard filter. Teen focus
remains a local exclusion because TMDB offers no equivalent trustworthy
field. H01's free-text “Asian series” is a separate interpretation question;
original language alone cannot establish origin or setting, and this v2
snapshot does not add an unrequested runtime language filter.

The [four-seed tuning comparison](smart-media-discovery-m0c-four-seed-tuning.md)
shows why the provider-order seed 30 is not yet a suitable final route for
Looking. Resolve the general seed preselection and fallback policy, capture
every full input pool it will read, then freeze grades and scoring before the
single held-out model run.
