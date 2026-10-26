# Ad-hoc job priority — delivery and verification

Companion to [settings.md](settings.md): this page records the queue fix.

**Status:** review approved; local gates passed; CI pending · **Written:** 2026-10-26

## Behavior

API-created jobs and manually triggered scheduler tasks use priority -100;
periodic jobs retain their positive priorities. Identity refreshes use request
origin, so automatic metadata sweeps keep their background priority. Wanted
search continuations retain interactive priority after restart. The queue runs
two general workers plus one reserved interactive worker. General
workers also select interactive jobs first. Automatic scan backfill keeps its
background priority.

A duplicate enqueue promotes a queued job to the higher requested priority.
It preserves the original payload, attempts, routing, and ready time. A running
job keeps its lease and is never restarted by a duplicate request. Provider
quota deadlines and retry backoff remain binding even for ad-hoc requests.

## Delivery checklist

- [x] Implement priority propagation, dedupe promotion, and reserved capacity.
- [x] Add regressions for saturation, promotion, routing, and scheduler origin.
- [x] Adversarial review approved after fixing queued-scan promotion in the UI,
  distinguishing request origin from the metadata force flag, and clearing stale
  promotion errors when the same job makes progress.
- [x] Lint and the full Go suite passed with the pinned toolchain. All 124 web
  unit tests and all 192 browser tests passed with Node 22.
- [x] Open [PR #73](https://github.com/pjunod/curator/pull/73).
- [ ] Complete CI, including the full race/coverage gate, before merge.
- [ ] Archive the task-owned managed worktree after completion.

The PR body is the live delivery record for final CI, merge, and cleanup.
The extra local race/coverage run uses the shared compiler cache; CI runs its
own full race/coverage gate with the repository's 30-minute test timeout.
Source work stays in the task-owned managed worktree until delivery completes.
