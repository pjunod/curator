# Architecture Decision Records

Decisions that shape Monarr, in the order they were made. Each record is immutable once
accepted; a change of course gets a new ADR that supersedes the old one.

| # | Title | Status |
|---|---|---|
| [0001](0001-name-and-license.md) | Name "Monarr", license GPL-3.0 | Accepted |
| [0002](0002-single-media-table.md) | One `media_items` table; files link to episodes n:m | Accepted |
| [0003](0003-compat-personalities.md) | Sonarr/Radarr v3 compat as URL-base personalities | Accepted |
| [0004](0004-sqlite-only.md) | SQLite only (modernc, WAL, single writer); no Postgres | Accepted |
| [0005](0005-filesystem-adoption.md) | Migration via filesystem adoption; *arr DB import is a stretch item | Accepted |
| [0006](0006-books-third-media-kind.md) | Books as a third media kind (ebooks + audiobooks, Phase 2.5) | Accepted |
| [0007](0007-remote-database-postgres.md) | Remote database (Postgres): evaluated for cluster HA, deferred with triggers | Superseded by [0008](0008-distributed-execution.md) |
| [0008](0008-distributed-execution.md) | Multi-host execution: leased job queue, mount-aware routing, Postgres for clustered mode | Accepted (step 1 built) |
| [0009](0009-root-folder-kinds.md) | Root folders carry a media kind; base directories are a picker, not an entity | Accepted |
| [0010](0010-scan-adopts.md) | Scan proposes matches; adoption is bulk work, not a to-do list | Accepted |
| [0011](0011-series-metadata-provider.md) | Series metadata is a chain: TheTVDB when keyed, TVmaze free, TMDB always | Accepted |
| [0012](0012-manual-entries.md) | Manual entries: a library record no provider backs, episodes read off the disk | Accepted |
| [0013](0013-measured-quality.md) | On-disk quality is measured (native probe), not inferred from names | Accepted |
| [0014](0014-target-profiles.md) | A quality profile is a target, not a list with a cutoff | Accepted |
| [0015](0015-discovery.md) | Discovery is a read-only browse surface: TMDB always, Trakt when keyed, nothing stored | Accepted |
| [0016](0016-air-times.md) | Air times from TVmaze's show-level schedule, composed to UTC per date at read time | Accepted |
| [0017](0017-audiobook-curation.md) | Audiobook acquisition, format routing, and multipart import | Superseded by [0018](0018-side-by-side-book-editions.md) |
| [0018](0018-side-by-side-book-editions.md) | Ebook and audiobook editions coexist under one book work | Accepted |
| [0019](0019-release-identity-evidence.md) | Release identity is evidence, not a normalized title | Accepted |
| [0020](0020-one-folder-one-item.md) | One folder, one library item: same-named works get `{tmdb-N}` folders | Accepted |

Context for all of these lives in [../architecture.md](../architecture.md).

- [0021: Durable Runner recovery handoff](0021-file-lifecycle-recovery.md) — placement journal, target revision and receipt-driven cleanup.
