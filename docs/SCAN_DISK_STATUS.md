# Disk scan status — visible progress through durable completion

**Status:** implementation verified; see PR for live delivery status · **Updated:** 2026-10-26 · **Version:** 0.38.1

Companion to [usage](usage.md#adopting-an-existing-library) and
[settings](settings.md#disk-scan--repairing-folders-with-recorded-media).
This page records the scan feedback fix, review, validation, and delivery.

## Behavior and implementation

The previous buttons queued work silently and refreshed once after 1.5 seconds.
The scheduler only enqueues the actual scan, so its completion cannot establish
that the filesystem reconciliation finished.

Library and Settings now open the shared action dialog immediately. Status
comes from the latest durable `library.scan` job: queued · running · retrying ·
complete · failed. The active button reopens progress without another request.
Completion refreshes the library, report, and review counts. Background polling
does not move scroll or focus. Closing the dialog restores focus to its opener.
Adoption and file probing remain separate jobs after scan completion.

The request enqueues directly so enqueue failures reach the initiating action.
The status lookup uses a `(kind, id)` index in migration 0062. No media files or
settings are changed by this status reporting.

## Review and verification

- [x] Port the earlier isolated fix onto main `6942993` in an independent clone.
- [x] Preserve current Settings recovery routing and regenerate the API bindings.
- [x] Bump from 0.38.0 to 0.38.1 and update usage/settings documentation.
- [x] Adversarial agent review; three actionable findings corrected.
  - Polling-confirmed jobs supersede a lost POST response error.
  - Report timestamps must belong to the completed job before counts appear;
    report errors remain visible and results keep retrying.
  - Status reads cannot delay submission of the scan request.
  - Unknown initial status cannot let historical completion hide a rejection.
  - The rejected-request fixture changes GET state only after an accepted POST.
- [x] Focused final rereview approved with no remaining actionable findings.
- [x] Final `make lint` and full `make test` passed with Go 1.25.7.
- [x] Node 22 web gate: 124 unit tests passed; build/type checking passed.
- [x] Full browser gate: 185 tests passed, including 390px/1280px geometry.
- [x] Generated SQL/API bindings verified with the pinned generator versions.
  The pinned sqlc requires a newer Go compiler; generation used automatic
  toolchain selection, while application validation used Go 1.25.7.
- [x] Lint passed with a registered reusable linter cache slot after the global
  cache returned diagnostics referencing deleted temporary checkouts. The
  shared compiler cache was preserved.
CI, merge, and cleanup are tracked in the live [PR #72](https://github.com/pjunod/curator/pull/72)
delivery checklist; no branch protection bypass is authorized.

Browser coverage includes desktop and phone geometry, immediate request
feedback, queued and long-running scans, dismissal and reopening, focus return,
background polling, request rejection/retry, and durable worker failure.

## Coverage gate follow-up

All CI Go tests, including race instrumentation, passed at `7c9e382`.
Browser, mobile, compatibility, Docker, generation, vet, and lint also passed.
The unit job failed only at coverage: 85.1% against the unchanged 86.0% floor.
Main's existing unit job likewise fails that floor at its published 85.2%.

Three additional reviewed API regressions cover unavailable status storage,
a durable enqueue followed by status-read failure, and a corrupt saved report.
The full local Go suite and lint pass after these additions; measured local
coverage is 85.2%. The floor and exclusions remain unchanged. Final CI results
and any explicit user decision about the inherited gap belong in the PR's
live delivery checklist.

## Delivery and cleanup

PR: [#72 — visible disk scan progress and results](https://github.com/pjunod/curator/pull/72).
The PR conversation/body is the live record for CI, merge, and final cleanup;
this source page records the reviewed implementation and local verification.

After merge, remove the task-owned remote branch and independent clone. Remove
only the earlier agent-created copies and temporary artifacts when their bytes
still match this task's originals; preserve all unrelated owner work. The app
refused archival of the earlier worktree because it treats it as the chat's
primary checkout; retry through the owning app and retain it if still refused.
