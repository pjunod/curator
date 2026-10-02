# Season acquisition — implementation and verification status

**Status:** building · **Updated:** 2026-10-01 · **Baseline:** `08af106`

Companion to [the implementation contract](plan-season-acquisition.md) and
[the specification review](plan-season-acquisition-review.md). This page records
actual implementation, review, checks, and delivery. Deployment is excluded.

## Delivery

- Independent clone: `/private/tmp/monarr-season-acquisition-build-20261001`.
- Branch: `codex/season-acquisition-planner`.
- PR: not created yet. Merge: pending. Cleanup: retained while building.
- Authoritative round-3 documents copied unchanged from the documentation clone.
- Fresh main matches the inspected baseline; next migration/ADR: 0038/0024.
- Direct implementation/PR/merge authorization supersedes the documents' earlier
  document-only authorization statements; no live acquisition or deployment.

## Milestones

- [x] Read repository instructions; independent clone and authoritative documents.
- [x] A: pure policy optimizer and independent oracle/property fixtures.
- [x] B: measured import protection and immutable provider allowlists.
- [x] C: bounded discovery, request ledger, pacing, feasible scheduler admission.
- [x] D: atomic execution, reservation settlement, uncertainty and stall recovery.
- [x] E: automatic callers, API/mobile/Activity explanations and visible actions.
- [ ] F: final documentation, adversarial review, checks, merge and cleanup.

## Final check ledger

No runtime test suites run during construction. Changed backend packages compile.
The pinned sqlc v1.31.1 requires Go 1.26; generation uses its required toolchain,
while application checks remain pinned to Go 1.25.7. Final checks follow adversarial review.

| Check | Result | Validated revision |
|---|---|---|
| make lint (Go 1.25.7) | pending | — |
| make test (Go 1.25.7) | pending | — |
| make test-web | pending | — |
| make test-e2e (Go 1.25.7) | pending | — |
| Required CI / generated consumers | pending | — |

## Adversarial review

Pending the complete reviewable PR; no implementation review requested yet.

## Recorded implementation decisions

- A single admitted discovery comparison conservatively protects evidence freshness;
  unopened jobs have no clock. Genuinely oversized scopes allow completed singles.
- Complete absence evidence is an adapter capability. Bounded SABnzbd/nzbd history
  remains uncertainty, requiring explicit custody review rather than assumed absence.
- Operator cancellation is a persisted reservation fence; explicit risk acknowledgment
  retains the original identity, warning, and late-publication prohibition.
- No live provider/client credentials or configuration were used. Throughput evidence
  uses hermetic rolling-budget/capability/pagination fixtures; live transport coverage
  remains outside this implementation's verification.
- Application Go 1.25.7 and CI Node 22.13.0 are preserved; sqlc's own Go 1.26
  requirement is isolated to generation.
