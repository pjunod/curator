# Wanted groups — implementation status

**Status:** in progress · **Branch:** `codex/wanted-groups-searches` ·
**Started:** 2026-09-19 · **Baseline:** `58cf1e6`

Companion to [usage.md](usage.md) (the operator workflow) and
[architecture.md](architecture.md) (the acquisition design). This page is the
live implementation record for grouped Wanted titles and durable exact-scope
search runs. It records completed work, the next concrete step, and final
verification evidence so progress is visible without reading commit history.

## Progress — one contract at a time

- [x] Isolated clone created from a descendant of `8ce3590`; the user's
  existing checkout remains untouched.
- [x] Implementation branch created from current `origin/main` at `58cf1e6`.
- [x] Source contracts and migration numbering reconciled against current
  main; migration `0031` owns durable Wanted runs.
- [x] Structured Wanted fields and authoritative scope selection.
- [x] Exact-target execution, reservations, pack rejection, and copy-aware
  regrab coverage.
- [x] Durable run storage, one-target queue chunks, recovery, cancellation,
  and shared queue retention.
- [x] OpenAPI endpoints, generated types, and native API handlers.
- [x] Grouped Wanted UI, visible reason actions, progress, persisted reload
  reconnection, and per-target results.
- [x] Usage/architecture documentation and version `0.25.3` update.
- [~] Acceptance fixtures cover scope membership, grouping, pack rejection,
  copy-aware season coverage, more than 20 targets, cancellation, API
  validation, and storage checkpointing. The final test lane has not run.
- [ ] Adversarial review after the complete change is merge-ready.
- [ ] One final test lane after review; exact command scope awaits resolution
  of the repository full-gate rule versus the requested fast-lane-only policy.
- [ ] Pull request merged into `main`.

## Decisions — explicit scope prevents accidental broadening

1. **The existing flat Wanted response stays compatible.** Structured fields
   are additive; dashboard and mobile consumers must continue to work.
2. **Bulk selection belongs to the server.** The browser sends a scope, never
   a page-derived target list, so collapsed groups and later pages cannot be
   silently omitted.
3. **This feature ships enabled.** No runtime feature flag or safety gate will
   hide it. The advanced pacing control is advisory and remains user-settable.
4. **The final review happens once.** An adversarial agent will inspect the
   merge-ready branch before the final test run, matching the requested CI/CD
   sequence and avoiding repeated full-tree test cost.

## Verification — evidence is added only when run

No implementation tests have been run yet. This is intentional: the requested
workflow defers tests until after the merge-ready adversarial review. Compile
contracts have been checked with `go build ./...` and
`npm --prefix web run typecheck`; both passed on 2026-09-19. Code generation
completed with the pinned SQLC and OpenAPI generators. The final section will
name every test command, result, and any check left to later CI.

## Open item — final local gate needs one policy choice

The repository requires `make lint`, `make test`, `make test-web`, and
`make test-e2e` before handoff. The requested workflow says to run only the
fast lane once and leave the broader unit-test pass to another process. Work
continues while that conflict awaits a decision; it blocks only the final gate.
