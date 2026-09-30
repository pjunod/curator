# ADR 0023 — Bounded series recommendations with optional local ranking

- **Status:** Accepted; implementation authorized by the owner 2026-09-30.
- **Relates to:** [0015](0015-discovery.md), [0011](0011-series-metadata-provider.md).
- **Companions:** [usage](../usage.md), [settings](../settings.md),
  [implementation plan](../plan-smart-media-discovery.md),
  [delivered qualification](../smart-media-discovery-qualification.md).

## Decision

Add `POST /api/v1/metadata/recommendations` for TV series descriptions,
seed titles, or both. Required themes use a finite vocabulary and explicit
TMDB keyword/synopsis evidence. Original language, years, genres, central
theme and known teen focus are filtered before final scoring. Embeddings
rank eligible titles; they never establish theme evidence.

Theme retrieval resolves exact keyword aliases and reads fixed first-page
lanes. Seed retrieval reads recommendations pages 1–2 and similar page 1.
Verified boys' love and space-travel seed keywords permit focused topic
expansion. Combined searches retain at most 60 candidates with balanced
theme/seed contributions. Details enrich at most 30 candidates, or 28 for
combined searches; TVDB seed mapping reduces the detail allowance by one.
Four concurrent reads share the ordinary TMDB 10/second limiter, with an
additional recommendation 8/second limiter. A search cannot exceed 41
provider reads. Shared 429 cooldown has no automatic retry.

One cold search runs at a time, with at most four identical attached callers.
Two cached readers may proceed concurrently. Cancelling the last attached
caller cancels work. Budgets are 15 seconds total, 10 seconds for provider
work, 4 cumulative seconds for encoding, and 1 second for ownership. Partial
provider failure returns usable eligible results or an unavailable error.
Complete empty retrieval returns an empty bounded search.

## Local model lifecycle

Ranking defaults off and uses the independently packaged Rust Candle helper.
Explicit installation verifies pinned file sizes and SHA-256 hashes before
atomic publication. The helper exchanges bounded JSON lines, 384-dimensional
normalized vectors, and batches of at most eight texts. Invalid replies,
crashes, or deadlines kill and reap it. Runtime failure preserves usable
metadata results and imposes a 30-second cooldown. Disabling cancels work and
reaps the helper. Model/configuration changes invalidate in-flight results;
runtime availability changes allow the metadata fallback.

The production image contains the executable and its required runtime
libraries, with no model download at build or startup. Cinema is independent.

## Ownership, privacy and caching

Ownership uses at most three narrow SQLite queries over the input IDs.
Cross-namespace conflicts remain visible as ambiguous and disable Add.
Unknown ownership is visible. Add uses the existing library identity checks.
Recommendation work performs no library writes or acquisition actions.

Only ranked results are cached: at most 128 entries, 64 MiB, and five minutes
of usable lifetime. Raw description and ranking text are omitted from cache
payloads and restored from the current request. Provider credentials and model
configuration fence cached and in-flight publication. Ownership is refreshed
on every response, and expiry is rechecked after that refresh. Descriptions
are POST bodies, absent from browser URLs and persistent browser storage.
This implementation deliberately uses one ranked cache rather than separate
raw-provider and vector caches proposed in the original plan.

## Quality limits and verification

Results cover the checked candidates, not the entire catalog. Sparse or
misleading metadata can miss suitable series or misclassify their focus.
Known teen exclusion retains unclassified shows. Main-story evidence is a
conservative synopsis heuristic, rather than an independently annotated fact.

M0 ranking comparisons and mixed-source tuning are provisional; annotations
were not independent human labels, and the original held-out set was
consumed. The fresh 146-read capture is a provenance/coverage diagnostic,
not an independently labeled relevance qualification. No 80% relevance claim
is made by this feature.

Regression coverage lives in domain, service, TMDB, SQLite, API and embedding
tests and `test/e2e/tests/zz-recommendations.spec.ts`. Required release checks
are `make lint`, `make test`, `make test-web`, and `make test-e2e`. Real model
qualification uses explicit `MONARR_TEST_MODEL_DIR` and
`MONARR_TEST_EMBED_BINARY` paths and checks helper shutdown. Resource results
are recorded separately with hardware and container limits.
