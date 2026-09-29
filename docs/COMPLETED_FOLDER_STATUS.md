# Completed-folder accounting — delivery status

**Status:** implementation ready for adversarial review · **Updated:** 2026-09-29 · **Target:** v0.33.0

Companion to [usage](usage.md#completed-folders--accounting-survives-an-empty-queue)
and [settings](settings.md#completed-folder-accounting). This page records work
and verification; the user-facing behavior lives in those documents.

| Work | State | Evidence / next action |
|---|---|---|
| Isolated clone | Complete | Based on GitHub main `9f1dff9`; branch `codex/completed-folder-accounting`. Original checkout's source edits restored to their initial state. |
| Durable path associations | Implemented | Migration 0035 backfills receipts and preserves them independently of Activity deletion. Download generation prevents reused IDs from reviving old ownership. |
| Disk inventory | Implemented | Startup + five-minute task; every top-level entry measured, nested files included; cached complete scans survive scan errors. |
| Activity and settings | Implemented | Measured totals, classifications, download associations, manual-import preview, explicit scan and cleanup actions, configurable roots. No feature enable gate. |
| Cleanup | Implemented | Verify local disappearance; rotate attempts; preserve Runner deletion authority; review safety of reused paths and overlapping ownership. |
| Reviewed deletion | Implemented | Explicit per-entry confirmation; rechecks live work, ownership, metadata and root identity; records intent and result. |
| Regression tests | Written, final run deferred | Empty queue, unknown files, stale scans, cleared history, ID reuse, false removal receipts, API, browser flow. |
| Adversarial review | Ready to request | Request only when the full PR is ready to merge, per the user's workflow. |
| Final gates | Pending | Run after review fixes. Earlier prototype checks are not verification of this rebased implementation. |
| PR and merge | Pending | Batch implementation commits into one PR; merge after review and green checks. |

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
