# Queue and Settings consolidation — renderings before implementation

**Status:** proposed for owner review; no application changes authorized yet
· **Written:** 2026-11-13 · **Reviewed baseline:** `7fadefc` (v0.48.0)

Companion to [Astra's source review](UI_CONSOLIDATION_ASTRA_REVIEW.md),
[the broader proposal](IMPROVEMENT_PROPOSAL.md) and the authoritative
[pipeline contract](PIPELINE_CONTRACT.md). This document specifies the first
UI cleanup slice. Preview content is illustrative, not household telemetry.

## 1. Proposed change

Use one **Activity** destination for the complete download-to-library journey,
with **In progress**, **Needs attention**, and **History** views. A job keeps
one row as it downloads, verifies, unpacks and imports. Imports-only filtering
and the existing release/title search remain available; `/imports` bookmarks
redirect to the unified page with the imports filter selected.

Remove the separate Import queue navigation entry and generic Resolve and
Dev settings tabs. Put a specific action beside the affected item. Move
occasional disk administration to **System → Storage**, and health/timing
information to **System → Diagnostics**. Settings contains preferences and
connection configuration, not another queue console.

This changes where controls appear and what they are called. It does not
retire necessary ownership, cancellation, publication or file-protection
rules, and does not delete historical records.

## 2. What moves, merges or disappears

| Current surface | Proposed location or disposition | Reason |
|---|---|---|
| Activity plus Import queue | One Activity page, with three views and an imports filter | Both represent the same acquisition attempts and share actions. Import history also includes dismissed downloads and unattributed placed imports, which must survive consolidation. |
| General Resolve launchers in Activity and Settings | Remove generic launchers; show the relevant title/file action in context | Both mount the same panel, combining three unrelated workflows. |
| Item → Import file | Keep on the library title | The title/copy provides the destination and identity context. |
| Loose completed files | System → Storage → Choose files to import | Files without a title need an explicit scoped preview. |
| Completed folders above active downloads | System → Storage | Disk inventory and cleanup obscure the live queue. |
| Old delivery receipt archive | Conditional historical records under System | Retained records or unresolved obligations can still require inspection; they do not belong above every live job. |
| Retry Runner source cleanup | Remove the button | The current server always refuses it: Runner owns that cleanup. |
| Retry handoff | Relevant publication's retry action | The old handler now re-offers the migrated publication. |
| Curator cleanup and historical selected-file retries | Eligible Storage/record action only | These still require exact-file identity and historical approval gates. |
| Standing legacy backlog tab and CSV in daily UI | Remove from daily navigation; contextual changed-storage review | The original legacy cohort was retired; current storage replacement holds still require scoped review. |
| Dev → Runner path/mount/credential | Advanced settings on the Runner connection | This is connection configuration, not a developer feature. Preserve saved values. |
| Dev → automatic recovery refresh checkbox | Remove from the ordinary form | Its web staged-copy wizard is unmounted; refreshing it should not be presented as enabling the current pipeline. |
| Dev → Cinema feature support | Beside that saved media-server connection's Test | It is read-only capability information, including unknown/unsupported state and last-test time. |
| System pipeline | System → Diagnostics | Operational telemetry is useful and distinct from an acquisition queue. |

Astra traced these dispositions to the current components, handlers, migrations
and signed-off pipeline decisions. See the [evidence matrix](UI_CONSOLIDATION_ASTRA_REVIEW.md#2-disposition-matrix).

## 3. What the daily views show

**In progress:** title first, episode/edition/copy when relevant, current stage,
measured progress and ETA when available, or an honest wait reason. Automatic
failure/regrab handling stays on the item's trace; it does not create a
permanent manual-recovery checklist. A finished failed attempt can link to
its current replacement.

**Needs attention:** only an actionable choice, stated directly: choose a
library title, approve an explicitly held import, assign a destination, or
review changed storage. The selected item explains what happened, which
files are affected, what is protected, and the precise next action. An offline
mount that retries automatically remains a waiting item unless configuration
or an explicit decision is actually required.

**History:** finished and failed attempts plus retained import history.
Historical records with no mutable download identity stay read-only. Preserve
salvage publication evidence separately from a failed terminal attempt.

Default phone navigation is Library, Discover, Wanted, Activity and More.
Calendar, Settings and System remain reachable through More. This cleanup
does not add a nonfunctional Requests tab; household requests follow next.

## 4. Renderings

The interactive preview offers Activity, Settings and System, including
an actionable manual import and a conditional historical-storage section.
Controls navigate local sample states only; no backend operations run.

| View | Desktop | Phone |
|---|---|---|
| Unified Activity | [Activity](renderings/ui-consolidation/activity-desktop.png) | [Activity](renderings/ui-consolidation/activity-mobile.png) |
| Item needing a decision | [Selected item](renderings/ui-consolidation/attention-desktop.png) | [Selected item](renderings/ui-consolidation/attention-mobile.png) |
| Cleaned Settings connections | [Settings](renderings/ui-consolidation/settings-desktop.png) | [Settings](renderings/ui-consolidation/settings-mobile.png) |
| Occasional storage work | [System Storage](renderings/ui-consolidation/storage-desktop.png) | Same responsive surface |

Editable [preview source](renderings/ui-consolidation/proposal.fragment.html).
The renderings show the core hierarchy, not every existing preference or
exception. Implementation retains search, imports-only filtering, pagination,
explicit cancellation semantics, selected retry results, stale-source warnings
and exact-file confirmations. Those are acceptance requirements, not silent
removals because an abbreviated rendering omits them.

## 5. Implementation constraints after approval

1. **One authoritative projection.** Join live downloads, import work and
   retained import history without double-counting attempts. A browser merge
   of two independently paged results is insufficient. Keep mutation identity
   and import epoch/generation checks; do not make historical negative IDs
   mutable.
2. **Complete counts and paging.** Cover more than 200 active attempts,
   imports-only views, overlapping history and search. Counts and displayed
   results must refer to the same scope.
3. **Honest refresh states.** Show loading, stale and unavailable separately
   from an empty queue. A provider refresh failure preserves the last snapshot
   and its timestamp, names the affected source and limits stale mutations.
4. **Contextual actions.** Distinguish cancel import, cancel download and
   blocklist, and dismiss history. While a writer is stopping, suppress retry
   or disposal until the durable state permits it. Keep approval and cleanup
   rules behind exact affected files, never a generic retry-all operation.
5. **Accessible phone flow.** Long names/paths wrap, the selected item has a
   visible close/back action, results stay visible, focus returns to the
   original row or stable heading, and navigation does not cover decisions.
6. **No accidental retirement.** Check real installations for retained
   obligations before retiring receipt data, staged-copy APIs or historical
   cleanup approval flows. This UI review did not inspect the household
   deployment. Conditional archival access is the proposed interim choice.
7. **Validation.** Run affected domain/API tests for the combined projection,
   web/mobile tests for navigation and statuses, and browser checks for active,
   approval-held, automatic retry, history, empty, stale and error states.

No database split, authentication rewrite, playback changes or queue-policy
redesign belongs in this UI slice. Existing safeguards remain authoritative.
Security findings are being addressed in the separately requested session.

## 6. Owner review and next priorities

The owner must see and approve these renderings before application UI changes.
The decision sought is the hierarchy and disposition above, especially one
Activity page, contextual decisions, and Storage/Diagnostics outside daily
queue work. Receipt data/API retirement would be a separate decision after a
usage and obligation check.

After cleanup, prioritize household requests and library-aware discovery for
this household. Setup and migration stay deferred. Future household/discovery
UI changes also require renderings before implementation.

## 7. Preview verification

The prototype was rendered with Chrome at desktop and 390-pixel phone widths;
390- and 320-pixel overflow checks passed. Activity views, item review, import
preview, Settings connections and System storage navigation were exercised.
The screenshots were visually inspected. No application code, live queue,
credentials, media files or backend behavior changed.
