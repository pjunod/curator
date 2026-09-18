# Release identity status — implementation and delivery record

Companion to [the reviewed implementation plan](plan-release-identity.md) —
this page records what is built, reviewed, validated, and delivered. The plan
remains the behavioral contract and acceptance matrix.

**Updated:** 2026-09-17 · **Branch:** `codex/release-identity` · **State:** in
progress · **Draft PR:** [#27](https://github.com/pjunod/monarr/pull/27)

## Delivery state

| Stage | State | Evidence |
|---|---|---|
| Isolated clone | complete | `/private/tmp/monarr-release-identity`, baseline `0da1c75` |
| Plan and repository guidance | complete | Plan §1–§13 and `CLAUDE.md` read before implementation |
| M1 pure identity contracts | implemented; validation deferred | Strict ID grammar, raw-title/series-year parser evidence, qualifier variants, ranked evaluator, conflict/coverage safeguards, and UK/US fixtures |
| M2 persistence and enrichment | pending | Migration, storage, provider snapshots, and jobs pending |
| M3 direct external-ID resolution | pending | Native and compatibility paths pending |
| M4 capability-aware query tiers | pending | Three-query automatic and five-query interactive budgets pending |
| M5 shared matching and provenance | pending | Interactive/RSS/backlog parity and durable evidence pending |
| M6 UI, docs, and version | pending | Web/mobile diagnostics, ADR, usage/settings, and `VERSION` pending |
| Adversarial review | pending | Run once, after implementation is ready to merge |
| Fast-lane validation | pending | Run once after review findings are addressed |
| Pull request and merge | pending | Create one batched PR and merge only after review and fast lane are green |

## Infrastructure notes

- Forgejo at `192.168.4.7` exposes no Monarr repository to the supplied token,
  and repository creation returned HTTP 403. The isolated clone therefore
  tracks the existing GitHub Monarr remote for the final PR.
- No test target is run during implementation. Per the delivery instruction,
  validation is deferred until the implementation is complete and the
  adversarial review has been addressed.
- This change adds no feature flag or runtime gate. Readiness information is
  diagnostic and advisory; missing identity evidence produces an explicit
  unresolved decision rather than silently enabling unsafe matching.

## Decisions to review at handoff

- The repository has no target named `fast-lane`. Before merge, the smallest
  meaningful lane will be selected from its pinned lint, generation, focused
  Go, web, and mobile checks; the exact commands and outcomes will be recorded
  here.
- The request mentioned a Plurx developer-settings enable section. This is a
  Monarr change, so no unrelated Plurx UI is modified. Monarr identity status
  will live on media/search diagnostics instead of gating the feature.
