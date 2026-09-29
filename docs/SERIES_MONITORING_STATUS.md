# Series monitoring — delivery status

**Status:** porting to current main · **Updated:** 2026-09-29 · **Target:** v0.34.0

Companion to [usage](usage.md#monitoring-series-seasons-episodes). This page
tracks delivery of persistent monitoring policies and automatic episode
search for new seasons, including the reported South Park season 29 case.

| Work | State | Evidence / next action |
|---|---|---|
| Isolated clone | Complete | `/private/tmp/curator-monitoring-agent`, based on main `da73227`; branch `codex/series-monitoring`. No further development in the user's checkout. |
| Monitoring policies | Porting | All, latest and future seasons, future episodes, new seasons, manual selection; preserve explicit overrides and pause independently. |
| Automatic search | Porting | Eligible packs first, then wanted aired episodes; account for copies and active downloads. |
| Regression coverage | Preparing | Port existing regression cases and adapt to current acquisition/language/identity behavior. |
| Adversarial agent review | Pending | Request only once the complete batch is ready to merge; address findings before testing. |
| Final tests and CI | Pending | Run after review fixes; rerun only for failures or changed code. Earlier tests on the old checkout are not evidence for this branch. |
| PR and merge | Pending | One PR containing the full batch of proper commits; merge after checks pass. |
| Cleanup | Pending | Remove task scratch artifacts after durable delivery; preserve unrelated user work. |

## Decisions

- The explicit user workflow supersedes CLAUDE.md's direct-checkout delivery
  and repeated pre-handoff tests. No feature enable gate is being added.
- Main already contains release identity, grouped wanted searches, language
  profiles, and completed-folder accounting. The port must preserve those.
- Existing checkbox choices are retained during upgrade; an already-disabled
  season is repaired by applying the desired policy or selecting that season.
- No deployment or live downloads are part of this change.
