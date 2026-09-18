# ADR 0019 — Release identity is evidence, not a normalized title

- **Status:** Accepted
- **Date:** 2026-09-17
- **Relates to:** [ADR 0011](0011-series-metadata-provider.md) (series metadata
  source and episode numbering), [ADR 0003](0003-compat-personalities.md)
  (Sonarr/Radarr lookup contracts)

## Context

Canonical names are not unique. A release may use a regional qualifier,
historical title, translated title, or an indexer-supplied external ID. Removing
words such as `US` globally fixes one example by making a different pair of
works indistinguishable. Title-only search also wastes better evidence that
TMDB, TVDB, IMDb, Newznab, and Torznab already provide.

## Decision

Each video item owns a persisted identity snapshot: canonical title and year,
all known external IDs, typed aliases, country evidence, and provider snapshot
health. Matching ranks exact IDs and exact titles before aliases and only
elides country qualifiers when fresh evidence proves which work owns the
qualified spelling. Explicit ID, country, year, kind, or episode conflicts are
always rejections.

Indexer capabilities are cached and searches are bounded. Automation sends at
most one ID query, the canonical title, and one alias; interactive search may
send two ID queries, the canonical title, and two aliases. Unknown capability
documents get one generic canonical-title query. A query never becomes proof:
only evidence returned with a release can establish an ID match.

The same pure evaluator serves interactive search, RSS, backlog search, and the
final pre-grab check. Its versioned decision is stored on the download and
shown in Release Search and Activity. Manual grabs remain possible and are
recorded as manual overrides rather than disguised as automatic matches.

External-ID input accepts only `tmdb:<digits>`, `tvdb:<digits>`,
`imdb:tt<digits>`, or a bare IMDb title ID. Exact lookup resolves through the
metadata provider chain and refuses contradictory provider records. Identity
metadata never changes the item's selected episode provider or numbering.

## Consequences

- Renames and regional variants are explainable without weakening global title
  normalization.
- Provider and indexer outages can leave identity evidence incomplete; that is
  visible and advisory, while unsafe automatic matches remain rejected.
- Alias and snapshot history add schema and refresh work, but matching remains
  pure and makes no network calls per release.
- Older binaries cannot open schema version 30. Upgrade workers together and
  restore a pre-upgrade backup to downgrade.

The detailed rules, ambiguity matrix, budgets, and rollout checks are in
[the implementation plan](../plan-release-identity.md).
