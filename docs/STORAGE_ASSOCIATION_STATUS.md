# Storage associations — shared-folder receipts must not claim every payload

**Status:** locally verified; CI pending · **Updated:** 2026-10-02 · **Version:** 0.36.2

Companion to [usage](usage.md) (storage explanations) and
[ADR 0021](adr/0021-file-lifecycle-recovery.md) (durable custody). This page
records the reported Monster history association, its correction and delivery.

## Diagnosis and correction

Storage inventory matched receipts in both ancestor directions. A historical
receipt at `/working/monarr/completed` therefore matched an unrelated Avatar
payload and every sibling, yielding two associations and an ambiguous entry.
Read-only diagnosis on nuc3 confirmed version 0.36.1 (`a83dfac`): Monster
receipt #11 is historical and points to `/working/monarr/completed`; Avatar
has a separate live receipt at its specific payload folder.

Inventory now associates only receipts at or below a payload. Historical
receipts at shared storage containers no longer veto unrelated imported-payload
cleanup. Receipt records remain durable. Live broad paths, specific historical
payload paths, library associations, unresolved placements and mount identities
continue to protect bytes. No live receipt or media file was manually changed as a workaround. The next
successful scan after installing the fix corrects cached associations.

## Progress and delivery

- [x] Independent clone of main (`a83dfac`); read repository rules and ADR 0021.
- [x] Narrow inventory matching and historical container cleanup protection.
- [x] Regression fixtures for shared roots, sibling payloads and true ambiguity.
- [x] Adversarial review: no actionable findings at `449ff4b`.
- [x] Final local repository gates; Go race coverage 86.2% (floor 86.0%).
- [ ] Required final-head CI.
- [ ] PR, merge and temporary-asset cleanup.

PR: pending. Deployment is excluded unless separately authorized.

## Validation

Review preceded tests. Focused regression fixtures, lint (zero issues), web
(120 tests / 13 files), and browser acceptance (160 tests) pass. The full Go
race/coverage run and vet pass, with coverage 86.2% against the unchanged
86.0% floor. The first invocation had a fixture reference type error, corrected
before the focused, lint and full Go reruns.
Final commands: `make lint`, `make test`, `make test-web`, `make test-e2e`;
CI also enforces race coverage, generation, vet, compatibility and mobile export.

## Diagnostic command correction

An unsupported `--version` invocation briefly started an extra Curator process
on nuc3. It was stopped; the original process (host PID 2899985) remained in
place and its API returned HTTP 200. Ordinary startup workers briefly ran in
the extra process. Subsequent receipt diagnosis used SQLite `mode=ro` only.
No deployment, service restart, receipt deletion or manual media cleanup was
performed.
