# Storage associations — shared-folder receipts must not claim every payload

**Status:** ready for adversarial review · **Updated:** 2026-10-02 · **Version:** 0.36.2

Companion to [usage](usage.md) (storage explanations) and
[ADR 0021](adr/0021-file-lifecycle-recovery.md) (durable custody). This page
records the reported Monster history association, its correction and delivery.

## Diagnosis and correction

Storage inventory matched receipts in both ancestor directions. A historical
receipt at `/working/monarr/completed` therefore matched an unrelated Avatar
payload and every sibling, yielding two associations and an ambiguous entry.
The screenshot is consistent with this code path; live database contents have
not been inspected.

Inventory now associates only receipts at or below a payload. Historical
receipts at shared storage containers no longer veto unrelated imported-payload
cleanup. Receipt records remain durable. Live broad paths, specific historical
payload paths, library associations, unresolved placements and mount identities
continue to protect bytes. No live files or database records are modified by
this delivery. The next successful scan corrects cached associations.

## Progress and delivery

- [x] Independent clone of main (`a83dfac`); read repository rules and ADR 0021.
- [x] Narrow inventory matching and historical container cleanup protection.
- [x] Regression fixtures for shared roots, sibling payloads and true ambiguity.
- [ ] Adversarial review and finding resolution.
- [ ] Final repository gates and required CI.
- [ ] PR, merge and temporary-asset cleanup.

PR: pending. Deployment is excluded unless separately authorized.

## Validation

Tests are written but have not run, following the repository review-first rule.
Final commands: `make lint`, `make test`, `make test-web`, `make test-e2e`;
CI also enforces race coverage, generation, vet, compatibility and mobile export.
