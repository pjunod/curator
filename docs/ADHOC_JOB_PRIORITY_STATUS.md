# Ad-hoc job priority — delivery and verification

Companion to [settings.md](settings.md): this page records the queue fix.

**Status:** review approved; final validation running · **Written:** 2026-10-26

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
- [~] Lint passed; all 124 web unit tests and all 192 browser tests passed.
  The full Go race/coverage gate is running with Go 1.25.7.
- [ ] Open reviewed PR, validate CI, and merge through branch protection.
- [ ] Archive the task-owned managed worktree after completion.

PR and final validation will be recorded here before delivery.
