# Release identity status — implementation and delivery record

Companion to [the reviewed implementation plan](plan-release-identity.md) —
this page records what is built, reviewed, validated, and delivered. The plan
remains the behavioral contract and acceptance matrix.

**Updated:** 2026-09-17 · **Branch:** `codex/release-identity` · **State:**
review findings addressed; fast lane green · **Draft PR:**
[#27](https://github.com/pjunod/monarr/pull/27)

## Delivery state

| Stage | State | Evidence |
|---|---|---|
| Isolated clone | complete | `/private/tmp/monarr-release-identity`, baseline `0da1c75` |
| Plan and repository guidance | complete | Plan §1–§13 and `CLAUDE.md` read before implementation |
| M1 pure identity contracts | implemented; fast lane green | Strict ID grammar, raw-title/series-year parser evidence, qualifier variants, ranked evaluator, conflict/coverage safeguards, and UK/US fixtures |
| M2 persistence and enrichment | implemented; fast lane green | Schema 30, transactional identity storage/revision, TMDB/TVmaze aliases and snapshots, refresh jobs |
| M3 direct external-ID resolution | implemented; fast lane green | Strict TMDB/TVDB/IMDb lookup, conflict errors, all-ID dedup, Sonarr/Radarr reuse |
| M4 capability-aware query tiers | implemented; fast lane green | Cached capabilities, typed protocol errors, 3-query automatic and 5-query interactive budgets |
| M5 shared matching and provenance | implemented; fast lane green | One evaluator for interactive/RSS/backlog/pre-grab; versioned download evidence and revision cache |
| M6 UI, docs, and version | implemented; fast lane green | Web/mobile identity diagnostics and explanations, ADR 0019, operator docs, 0.25.0 version |
| Adversarial review | complete; findings addressed | One final review found provenance forgery, year bypass, cache/timeout/dedup/atomicity/fallback defects, a compile error, and two diagnostics gaps; all were corrected before validation |
| Fast-lane validation | green | Generated drift, Go vet/build, focused changed-package tests, web tests/build, and mobile typecheck/tests |
| Pull request and merge | draft PR open | One batched PR; mark ready and merge only after review and fast lane are green |

## Infrastructure notes

- Forgejo at `192.168.4.7` exposes no Monarr repository to the supplied token,
  and repository creation returned HTTP 403. The isolated clone therefore
  tracks the existing GitHub Monarr remote for the final PR.
- No test target ran during implementation. Per the delivery instruction,
  validation ran only after implementation and adversarial review fixes.
- This change adds no feature flag or runtime gate. Readiness information is
  diagnostic and advisory; missing identity evidence produces an explicit
  unresolved decision rather than silently enabling unsafe matching.

## Decisions to review at handoff

- The repository has no target named `fast-lane`. The selected lane verified
  generated SQL/OpenAPI drift, ran `go vet ./...`, tested the changed domain,
  adapter, SQLite, library, acquisition, API, and compatibility packages,
  built `./cmd/monarr`, ran all 78 web tests plus the production build, and ran
  mobile typecheck plus all 53 mobile tests. Full race/coverage, browser E2E,
  Docker, and mobile export remain for the separate batched CI process.
- The request mentioned a Plurx developer-settings enable section. This is a
  Monarr change, so no unrelated Plurx UI is modified. Monarr identity status
  will live on media/search diagnostics instead of gating the feature.

## Adversarial review resolution

- Search candidates now carry opaque, short-lived, bounded server tokens;
  clients cannot author persisted evidence, and rejected/expired selections
  are recorded as manual overrides.
- Explicit release years remain hard conflicts even when an external ID
  matches.
- Capability failures retry, honor rate-limit times, degrade to one generic
  query when safe, and runtime unsupported modes are invalidated.
- Each indexer gets one deadline across all tiers. Authentication and rate
  limits stop further requests.
- Releases deduplicate by indexer GUID or exact download URL. Repeated rows
  merge identity evidence and contradictory IDs become rejections.
- Provider metadata, verified IDs, historical canonical aliases, episodes,
  and identity revision invalidation commit atomically.
- Compatibility fallback is limited to unsupported/unconfigured provider
  paths. Partial search headers and the web notice distinguish incomplete
  results from an authoritative empty response.
