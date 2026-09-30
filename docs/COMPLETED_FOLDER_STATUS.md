# Completed-folder accounting — delivery status

**Status:** storage management review approved; final CI and merge tracked in GitHub · **Updated:** 2026-09-29 · **Target:** v0.33.1

Companion to [usage](usage.md#completed-folders--accounting-survives-an-empty-queue)
and [settings](settings.md#completed-folder-accounting). This page records work
and verification; the user-facing behavior lives in those documents.

## Storage management follow-up — v0.33.1

Work continues in an independent clone. The user's existing checkouts are not
used for development. All changes will be delivered in one PR; adversarial
review comes before the final test gate and merge.

| Work | State | Evidence / next action |
|---|---|---|
| Layout and navigation | Implemented | Responsive rows; 25 entries per page; search, status filtering, name/size sorting; pagination above and below the list. |
| Bulk management | Implemented | Select page or all matching eligible entries; confirm paths and logical size; per-entry deletion outcomes and stale-fingerprint protection. |
| Scan timeout | Implemented | Removed the background scan's fixed 30-second cutoff; cancellation and the entry limit remain. Delete verification visits only the selected payload. |
| Import review | Fixed | Selecting another payload resets the manual-import form to its path. |
| Adversarial review | Approved after fixes | Shared deletion state now survives route/responsive remounts; confirmation uses a focused dialog with Escape and focus restoration. Reviewer approved both fixes before the final test gate. |
| Final verification | PR checks are authoritative | The final Go/web unit, race/coverage, mobile, browser, compatibility, Docker, and lint gates run in CI after review. No duplicate local unit-test run. |
| PR and merge | Tracked by GitHub | One PR from `codex/download-storage-management`; merge only after all checks pass. The PR body carries the live delivery checklist. |
| Cleanup | Protected worktree retained | Codex refused archival because the prior worktree is protected by a pinned chat/workspace. Remove the independent temporary clone and task artifacts after verified merge. |

**Decisions:** bulk import remains an individual review because payloads need
library destinations. Bulk deletion reuses the existing server ownership and
fingerprint checks. Progress remains visible after navigating within the app;
the browser tab must stay open until deletion finishes. No feature toggle or enablement gate was introduced.
Production storage has not been modified; deployment is outside this PR.

## Original accounting delivery — v0.33.0

| Work | State | Evidence / next action |
|---|---|---|
| Isolated clone | Complete | Based on GitHub main `9f1dff9`; branch `codex/completed-folder-accounting`. Original checkout's source edits restored to their initial state. |
| Durable path associations | Implemented | Migration 0035 backfills receipts and preserves them independently of Activity deletion. Download generation prevents reused IDs from reviving old ownership. |
| Disk inventory | Implemented | Startup + five-minute task; every top-level entry measured, nested files included; cached complete scans survive scan errors. |
| Activity and settings | Implemented | Measured totals, classifications, download associations, manual-import preview, explicit scan and cleanup actions, configurable roots. No feature enable gate. |
| Cleanup | Implemented | Verify local disappearance; rotate attempts; respect Curator-owned versus Runner-owned storage; protect reused paths and overlapping ownership. |
| Reviewed deletion | Implemented | Explicit per-entry confirmation; rechecks live work, ownership, metadata and root identity; records intent and result. |
| Regression tests | Passed | Empty queue, unknown files, stale/replaced mounts, cleared history, ID reuse, false receipts, concurrent admission, protected storage, API, browser flow. |
| Adversarial review | Addressed | Six findings fixed: missing-parent cleanup receipts, import admission races, changed mount identity, library bind aliases, symlink unlink ownership, and duplicate roots. Regression cases added before final tests. |
| Final gates | Passed locally | Full Go suite; 86.1% coverage against unchanged 86.0% floor; race-checked regressions; 119 web tests; 118 browser tests plus final storage retest; lint clean; generation and build succeeded. |
| PR and merge | GitHub delivery | [PR #49](https://github.com/pjunod/curator/pull/49) contains the batched commits; its checks and merge record are the authoritative delivery status. |

## Decisions and limits

- Unknown files and paths whose ownership ended stay visible for review. Age
  or a matching directory name does not authorize deletion.
- Ownership clarified by the user: Curator owns `/mnt/qnap/working/monarr`,
  mounted at `/working/monarr`; `completed` is also mounted at `/downloads`.
  Curator cleanup applies there regardless of the download client. Runner
  owns its separate `/processing` and recovery trees. ADR 0021 is amended
  to state that boundary.
- Sizes are logical bytes, including hardlink names. They are not promised
  physical reclamation. Symlink targets are excluded and visibly flagged.
- Live read-only inspection on nuc3: 842,592,360 KiB (about 804 GiB),
  74 completed entries, zero downloads in the database; all 74 lack an
  Activity association. No production deletion or deployment performed.
- Initial work used the supplied older checkout before the user required
  isolated clones. Those feature edits were extracted and removed without
  disturbing the pre-existing source changes. All subsequent work is here.

A changed mount identity preserves the last successful counts and blocks deletion.
Restore the mount, or explicitly save the storage roots again to accept a replacement;
the next complete scan establishes its baseline.

The first full race run exposed one legacy assertion expecting an absent payload
to remain pending. The assertion now matches verified absence with a reachable
parent; missing-parent and changed-mount cases remain pending. Added API, health,
and reconfiguration cases raised coverage from 85.9% to 86.1% without lowering
the gate. The final complete Go run and focused race regressions pass.

CI confirmed the full race/coverage gate, Docker, lint, and compatibility browser
checks. Its mobile job passed 94 tests but found existing Expo patch-version
drift. The three direct Expo packages and their core dependency were updated;
94 mobile tests, all 20 Expo diagnostics, and both platform bundle exports pass
locally. The final commit is being checked by the same CI gates.
