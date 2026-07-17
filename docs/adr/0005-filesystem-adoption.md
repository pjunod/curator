# ADR 0005 — Migration via filesystem adoption; *arr DB import is a stretch item

- **Status:** Accepted
- **Date:** 2026-07-17

## Context

Adoption depends on not starting from zero: existing Sonarr/Radarr users have large curated
libraries. Two possible migration mechanisms: adopt the filesystem (point Monarr at existing
root folders and rebuild the library by scanning + metadata matching), or import the upstream
SQLite databases directly (carrying over monitoring state, tags, history, blocklist).

## Decision

- **Filesystem adoption is the committed path.** Library reconcile walks existing root
  folders, parses on-disk files, and matches against TMDB. Because the Renamer is
  token-compatible with Sonarr/Radarr naming schemes, existing layouts adopt **with zero
  renames**.
- **Direct *arr DB import is a Phase 4 stretch item**, demand-driven, off the critical path.

## Consequences

- Accepted cost of the primary path: per-episode monitoring nuance, history, and blocklist do
  not carry over.
- The reconcile loop that migration requires is the same one that makes Monarr robust against
  humans touching the filesystem — one mechanism, two payoffs.
- Known lossy edge: TVDB→TMDB episode-ordering differences (daily shows and anime are the
  worst offenders). Reconcile **flags** series whose on-disk structure disagrees with TMDB for
  manual review rather than silently mis-mapping.
