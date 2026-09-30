# Smart Media Discovery M0d: coverage stage audit

**Status:** retrospective diagnosis of consumed M0c held-out queries. This
audit does not tune a new retrieval rule or turn agent-derived grades into
independent validation. Counts come from the repaired v3 diagnostic and its
fixed candidate/label snapshot.

| Case | Full pool | Selected thirty | Detail-eligible | Established failing stage |
| --- | --- | --- | --- | --- |
| H01 · gay-themed Asian series | 54, full-pool suitability **ungraded** | 7 suitable / 30 | 7 suitable / 23 | Source shortage versus selector loss cannot be separated yet. The eligible stage did not lose any selected suitable work. |
| H03 · adult friendships + seed | 60, full-pool suitability **ungraded** | 7 suitable / 28 | 7 suitable / 10 | Source shortage versus selector loss cannot be separated yet. Required-theme admission removes eighteen selected works, but no selected suitable work. |
| H04 · Tales of the City 1993 | 4 suitable / 60 | 3 suitable / 30 | 3 suitable / 19 | Full-pool shortage is definitive; selection loses one of four. |
| H05 · Tales of the City 2019 | 14 suitable / 60 | 11 suitable / 30 | 9 suitable / 21 | Selection loses three suitable works; seed cosine cutoff loses two more. The pool itself could support ten. |
| H06 · LGBTQ+, no teen dramas | 5 suitable / 20 | 5 suitable / 20 | 5 suitable / 12 | Full-pool shortage is definitive; selection and hard admission lose no suitable work. |
| H08 · adult coming of age | 2 suitable / 20 | 2 suitable / 20 | 1 suitable / 7 | Full-pool shortage is definitive; admission loses one additional suitable work. Fewer than ten returned makes P@10 undefined. |

The single captured topic page in H06/H08 had only twenty candidates. A
second exact topic source could increase the pool while preserving the
sixty-summary, thirty-detail and 41-upstream-call caps, but the captured
data cannot establish that it would add suitable works. Do not substitute a
broad unrelated fallback. H01 and H03 need full-pool labels before choosing
between a source change and a shallow-selector change. H05 needs a fresh
seed analogue with a rich labeled pool to assess the selector and cutoff;
changing the 0.48 cutoff from H05's consumed result would be retuning.
Combined mode must still apply required-theme admission to every candidate,
including seed-path candidates. Missing detail fields must continue to fail
the corresponding hard filter.

For a bounded next validation, freeze candidate identities and provenance
for one new sparse-theme page expansion, one new combined query and one new
rich seed pool before ordering. Independently assign source-backed grades to
the **entire compared union**, with provenance and uncertainty, then compare
pool suitability, shallow selection, detail eligibility and returned ranking
under one common ideal. Keep the already frozen 41-call and 15s/10s/4s
budgets. This isolates the failing stage without an alias sweep or reuse of
the H outcomes as a tuning set.

## Data limit and implementation recommendation

The authorized captures cannot supply that fresh comparison. Complete
seed-related page triplets exist only for Looking, Bad Buddy, Star Trek: The
Next Generation and House of Cards (all already used for tuning), plus the
1993 and 2019 Tales of the City seeds (consumed H04/H05). The captured theme
pages are also the pages used by M0c's tuning and held-out routes. Rewording
those queries or rescoring their orders cannot create an unseen seed or a
title-disjoint validation set. No additional provider call was attempted
through the credential path rejected by automatic approval review.

The frozen focused seed-source rule is ready as a **candidate design**, with
exact keyword ID/name verification and fixed quotas; it is not ready to ship.
Theme/combined candidate coverage remains unresolved for H01/H03 and
definitively sparse for H06/H08; H04 has a seed full-pool shortage, and H05
loses suitable works in selection and at the semantic cutoff. The smallest
remaining data blocker to an M1 decision is an authorized capture of new
provider candidates and details for a sparse theme, a combined query and a
rich seed, followed by complete compared-union source-backed judgments
frozen before model order. Actual endpoint/provider/cache timing and runtime
keyword verification remain separate integration checks after that quality
gate. The current feature implementation should therefore remain gated.
