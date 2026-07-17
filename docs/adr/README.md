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

Context for all of these lives in [../architecture.md](../architecture.md).
