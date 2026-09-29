# Series monitoring — delivery status

**Status:** review fixes complete; final checks next · **Updated:** 2026-09-29 · **Target:** v0.34.0

Companion to [usage](usage.md#monitoring-series-seasons-episodes). This page
tracks delivery of persistent monitoring policies and automatic episode
search for new seasons, including the reported South Park season 29 case.

| Work | State | Evidence / next action |
|---|---|---|
| Isolated clone | Complete | `/private/tmp/curator-monitoring-agent`, based on main `da73227`; branch `codex/series-monitoring`. No further development in the user's checkout. |
| Monitoring policies | Implemented | All, latest and future seasons, future episodes, new seasons, manual selection; preserve explicit overrides and pause independently. |
| Automatic search | Implemented | Eligible packs first, then wanted aired episodes; account for copies and active downloads. |
| Regression coverage | Prepared, not run | Policy persistence, pause/resume, manual overrides, migration, API validation, pack fallback, copies, air dates, and browser controls. |
| Adversarial agent review | Addressed | Fixed the stale season-pack reservation window: revalidate child monitoring, dates, quality and same-copy downloads after acquiring the lock and immediately before grabbing. Regression cases cover both boundaries and independent copies. |
| Final tests and CI | Pending | Run after review fixes; rerun only for failures or changed code. Earlier tests on the old checkout are not evidence for this branch. |
| PR and merge | Pending | One PR containing the full batch of proper commits; merge after checks pass. |
| Cleanup | In progress | Earlier task source edits and eight new files removed from the user checkout after verified extraction; pre-existing edits preserved. Remove clone/scratch artifacts after durable delivery. |

## Decisions

- The explicit user workflow supersedes CLAUDE.md's direct-checkout delivery
  and repeated pre-handoff tests. No feature enable gate is being added.
- Main already contains release identity, grouped wanted searches, language
  profiles, and completed-folder accounting. The port must preserve those.
- Existing checkbox choices are retained during upgrade; an already-disabled
  season is repaired by applying the desired policy or selecting that season.
- No deployment or live downloads are part of this change.

The pinned sqlc generator requires Go 1.26 or newer; code generation uses
automatic toolchain selection. Application verification remains on the
repository toolchain. Migration 0036 follows current main migration 0035.
