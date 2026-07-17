# ADR 0002 — One `media_items` table; files link to episodes n:m

- **Status:** Accepted
- **Date:** 2026-07-17

## Context

The unification could be modeled with parallel `movies` and `series` tables (as the two
upstream apps effectively have), or with a single aggregate-root table plus kind-specific
children. Every downstream system — tags, root folders, history, search, the UI list
endpoints — either branches on media kind forever or doesn't, depending on this choice.

Separately, media files must relate to episodes: season packs produce files spanning many
episodes, and multi-episode files (`S01E01E02`) are real. Radarr's 1:1 movie↔file link is the
degenerate case of a more general relationship.

## Decision

- **One `media_items` table** with a `kind` column (`movie` | `series`) and a small number of
  nullable kind-specific columns. Seasons and episodes live in child tables that are simply
  empty for movies.
- **`media_files` link to episodes many-to-many** via a link table. A movie file links to one
  media item; an episode file links to 1..n episodes.
- The acquisition pipeline operates on the abstract **Wantable** (movie, episode, or season),
  and media-kind knowledge is quarantined behind exactly two interfaces: `SearchPlanner` and
  `ReleaseMatcher` (blueprint §4.1).

## Consequences

- One tag system, one root-folder system, one history FK, one search index, one UI list
  endpoint — ~85% of the code never branches on kind.
- Season-pack fan-out (one download satisfying many episode Wantables) is representable from
  day one; getting this wrong later would mean painful migrations.
- Slightly wider `media_items` rows (a few nullable columns) — an acceptable cost in SQLite.
