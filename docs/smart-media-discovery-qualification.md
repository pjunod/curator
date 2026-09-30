# Smart media discovery — delivered verification and its limits

Companion to [ADR 0023](adr/0023-smart-media-discovery.md),
[usage](usage.md#describing-a-series-or-finding-similar-shows), and
[settings](settings.md#local-semantic-ranking-optional).

## Implemented and integrated on 2026-09-30

Version 0.35.0 includes bounded TV description search, seed recommendations,
evidence and filters, library ownership, optional local ranking, explicit
verified model installation, settings, web preview/Add, and production
helper packaging. Feature commit `36b8886` was integrated into the owner's
working tree based on `2cd269e`, preserving its newer series monitoring,
storage behavior, and unrelated acquisition fixes. Earlier milestone holds
were superseded by direct owner authorization to complete the feature.

## Required checks passed on the delivered tree

| Check | Result |
|---|---|
| `make lint` | Pinned linter: zero issues; isolated cache avoids other worktrees' stale paths |
| `make test` | All Go packages pass |
| `make test-web` | 119 tests pass |
| `make test-e2e` | 127 tests pass, desktop and phone, including existing monitoring behavior |
| `make embed-check` | Pinned Rust fmt, locked tests, and clippy pass |
| Focused repeated race tests | Service, embedding, domain, TMDB, SQLite and API exercised by implementation and advisory review |
| Real model through Go supervisor | Verified pinned files encode successfully; disable kills and reaps helper |

Browser coverage includes inferred-theme changes, explicit filter overrides,
fresh title seeds, preview return, cancellation/late response suppression,
390-pixel layout with long root paths, real Add and fresh ownership on cached
recommendations. Service tests exercise actual four-caller and two-reader
limits, last-caller cancellation, provider/model publication fences, bounded
pool enrichment, partial failure, cache accounting, and prose-free entries.

## Full Linux image and API resource measurement

The production Dockerfile built the final integrated source on **nuc3 Linux
x64**. The app and real pinned encoder ran together in one non-root container
limited to **two CPUs and 512 MiB**, with a sanitized captured-provider replay.
Five distinct cold request keys were measured per mode. This isolates provider
dispatch, actual service selection/detail handling, API serialization and
model work; it does **not** measure live provider network latency or relevance.

| Mode | Maximum observed duration | Maximum provider reads |
|---|---:|---:|
| Theme (partial: 29 of 30 details available) | 5.2411 s | 39 |
| Seed | 4.9318 s | 34 |
| Combined | 5.1954 s | 41 |

Every theme sample attempted 30 details and successfully checked 29; one
replay detail was unavailable. These are partial-search timings, not a
complete-search latency claim. Seed and combined samples checked their full
30- and 28-detail envelopes. The original runner omitted the response coverage
field; the saved report explicitly marks this audit as inferred from the
attempted-read envelope and successful checked count. The earlier exact-mode
feasibility results remain separate from this integrated partial measurement.

Container cgroup peak was **208,551,936 bytes (198.89 MiB)**, with zero OOM
events. Disabling ranking removed `curator-embed` from the container's process
list and reduced current memory to **21,700,608 bytes (20.70 MiB)**. The
container, provider process, temporary image, and remote test directory were
removed afterward. Local browser servers and helper processes were also
checked stopped.

Exact samples, counters, process checks, source/image versions and capture
digest are in [integrated resource results](smart-media-discovery-integrated-resources.json).

## Capture and quality interpretation

The owner authorized the bounded capture on nuc3. It completed **146 GETs**,
with no retries, and retained sanitized responses for three diagnostic pools
of **36, 60 and 60** candidates. Credentials stayed in process memory and are
absent from [the saved capture](smart-media-discovery-fresh-capture.json).
The remote temporary capture was removed after retrieval.

The capture uses page-two diagnostics and is not an independent human-labeled
quality evaluation. The coming-of-age/LGBTQ+ combination is a diagnostic pool;
the shipped API asks for refinement when multiple required themes are given.
Earlier M0 labels were not independent human annotations, and the original
held-out set was consumed. Those results remain provisional. No catalog-wide
recall or 80% relevance claim is made. The feature reports the checked set,
metadata evidence, partial coverage and unavailable similarity explicitly.
