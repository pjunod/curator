# Season acquisition — implementation and verification status

**Status:** implementation verified · **Updated:** 2026-10-01 (ET) · **Baseline:** `08af106`

Companion to [the implementation contract](plan-season-acquisition.md) and
[the specification review](plan-season-acquisition-review.md). This page records
actual implementation, review, checks, and delivery. Deployment is excluded.

## Delivery

- Independent clone: `/private/tmp/monarr-season-acquisition-build-20261001`.
- Branch: `codex/season-acquisition-planner`.
- PR: [#59](https://github.com/pjunod/curator/pull/59) is the live record of required
  CI and merge state. All local gates are complete; merge waits for final-head CI.
- The independent clone is temporary and removed after merge and evidence export.
  The final delivery record records the actual merge and cleanup outcome.
- Authoritative round-3 documents copied unchanged from the documentation clone.
- Fresh main changed to `0fa405e` during final gates; its alternate-name UI was
  integrated. Migration/ADR numbers remain 0038/0024; VERSION is 0.36.0.
- Direct implementation/PR/merge authorization supersedes the documents' earlier
  document-only authorization statements; no live acquisition or deployment.

## Milestones

- [x] Read repository instructions; independent clone and authoritative documents.
- [x] A: pure policy optimizer and independent oracle/property fixtures.
- [x] B: measured import protection and immutable provider allowlists.
- [x] C: bounded discovery, request ledger, pacing, feasible scheduler admission.
- [x] D: atomic execution, reservation settlement, uncertainty and stall recovery.
- [x] E: automatic callers, API/mobile/Activity explanations and visible actions.
- [x] F: final documentation, adversarial review and local validation.
- Delivery: required final-head CI, merge and cleanup are recorded by the linked PR
  and final delivery record.

## Final check ledger

No runtime test suites run during construction. Changed backend packages compile.
The pinned sqlc v1.31.1 requires Go 1.26; generation uses its required toolchain,
while application checks remain pinned to Go 1.25.7. Final checks follow adversarial review.

| Check | Result | Validated revision |
|---|---|---|
| make lint (v2.12.2; isolated cache) | pass: zero issues | final boundary fixture commit |
| make test / full race (Go 1.25.7) | pass; local coverage 86.2%, unchanged floor 86.0% | final boundary fixture commit |
| make test-web | pass: 120 tests / 13 files | d3d29f6; subsequent changes are Go fixtures |
| make test-e2e (Go 1.25.7 / Node 22.13) | pass: 154 tests; six season actions at phone/desktop widths | d3d29f6; subsequent changes are Go fixtures |
| Mobile check / doctor / export | pass: 94 tests / 16 files, typecheck, iOS + Android bundles | d3d29f6; subsequent changes are Go fixtures |
| go vet / generation | pass; generated consumers unchanged | 00fcdb3 |
| Required CI | see linked PR for final-head results | final documentation commit |

## Adversarial review

Two independent adversarial reviews inspected `fd8c63a`, then verified fixes
through `66358dc`. Both report all merge blockers resolved and no further
blockers in the fixes. No tests ran during their review. Final gates exposed and resolved generated SQL, polling, and import compatibility regressions. Follow-up reviewers cleared the terminal custody trigger, measured replacement checks, path aliases and complete unresolved reference guards. They also cleared unplanned
uncertain custody acknowledgment and the shared import publication fence; later
commits add boundary fixtures without changing production behavior.

| Finding | Resolution |
|---|---|
| Lost receipt triggers on table rebuild | Restore triggers; prior-schema migration/receipt regression. |
| Multipart and unassigned/untracked target protection | Same-attempt digest exclusions; affirmative destination replacement authority. |
| Fallback/manual reservation races | Common import/admission mutex; symmetric overlap and conditional pending supersession. |
| Manual selection cancelled submitted siblings | Narrow supersession preserves unaffected submitted import authority. |
| Cancellation fence erased by polling | Preserve operator fence and reject late automatic publication. |
| Uncertain/terminal row erased while placement unresolved | Persist placement download association; atomic deletion guards. |
| Deterministic refusal orphaned placement journal | Validate before journal; identity-checked rollback on resumed refusal. |
| Successful empty-handle qBittorrent submission never learned hash | Persist unique matched handle and retire exact current handle. |
| Caps outages vetoed healthy providers | Durable initialization retries and strict automatic capability evidence. |
| Checkpoint overflow repeated failed request | Explicit partial scope with bounded retained evidence. |
| Pagination used filtered candidate count | Cursor advances by raw wire item count. |
| Cap field rendered on clients | Correct Add indexer form and saved cap dialog. |
| Unsafe partial cleanup starved later rows | Rotate failed attempts without releasing custody. |

## Final-gate corrections

- Linux CI initially measured 85.9% against the unchanged 86.0% floor after
  passing all Go tests. Additional cancellation and retained-source validation
  fixtures raise the fresh local race profile to 86.2%; final-head Linux CI is
  recorded in the linked PR. No production behavior or coverage floor changed.

- Terminal custody clears atomically in a SQLite trigger; uncertainty and operator
  cancellation remain fenced. Repeated named SQL expressions are avoided because
  the pinned generator emitted invalid runtime SQL.
- Fresh fills retain measured facts; destruction requires measured policy approval.
  Same-quality manual imports choose a stable digest suffix beside existing bytes.
- Polling skips active imports and locks only possibly sent submissions for
  uncertainty observation.
- Cleanup normalizes parent aliases, protects every library file association, and
  checks all unresolved placements; the recovery batch limit is never absence proof.
- Browser fixtures exercise an eligible persisted RSS due time and stable torrent
  hashes. New custody dialogs are checked after scrolling at phone and desktop widths.

## Recorded implementation decisions

- A single admitted discovery comparison conservatively protects evidence freshness;
  unopened jobs have no clock. Genuinely oversized scopes allow completed singles.
- Complete absence evidence is an adapter capability. Bounded SABnzbd/nzbd history
  remains uncertainty, requiring explicit custody review rather than assumed absence.
- Operator cancellation is a persisted reservation fence; explicit risk acknowledgment
  retains the original identity, warning, and late-publication prohibition.
- Deliberate custody acknowledgment also covers unplanned uncertain rows; it
  never queues an automatic replacement. Every persisted import re-reads the
  supersession/cancellation fence under the publication lock.
- No live provider/client credentials or configuration were used. Throughput evidence
  uses hermetic rolling-budget/capability/pagination fixtures; live transport coverage
  remains outside this implementation's verification.
- Application Go 1.25.7 and CI Node 22.13.0 are preserved; sqlc's own Go 1.26
  requirement is isolated to generation.
