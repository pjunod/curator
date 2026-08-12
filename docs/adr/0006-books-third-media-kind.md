# ADR 0006 — Books as a third media kind (ebooks + audiobooks, Phase 2.5)

- **Status:** Accepted
- **Date:** 2026-07-17

Audiobook subtype curation and multipart import were refined by
[ADR 0017](0017-audiobook-curation.md); side-by-side edition identity is
defined by [ADR 0018](0018-side-by-side-book-editions.md).

## Context

The v0.2 blueprint listed books as a non-goal ("Readarr territory"). That territory no longer
has an occupant: **Readarr was retired by the Servarr team**, and as of mid-2026 no maintained
*arr-class replacement exists — the community pieces together LazyLibrarian, shelfmark, and
metadata shims like rreading-glasses. The gap is real and sits directly adjacent to Monarr's
mission.

Architecturally, the acquisition pipeline operates on the abstract **Wantable** and
quarantines media-kind knowledge behind `SearchPlanner` and `ReleaseMatcher` (ADR 0002), so a
third kind is a bounded addition, not a rewrite. A book is in fact the *simplest* Wantable —
standalone, typically one file, the movie-degenerate case.

Readarr's fatal flaw was its metadata backend (a bespoke Goodreads-scraping server the team
had to operate), not its pipeline. That is a warning to design around, not a reason to
abstain.

## Decision

- The media **`kind` enum gains `book`**, reserved in the Phase 1 schema from the very first
  migration (`kind TEXT CHECK (kind IN ('movie','series','book'))`) so no kind-widening
  migration is ever needed.
- **A book is a standalone Wantable** (movie-degenerate case): one book ↔ typically one file;
  the n:m file↔child link machinery still applies if omnibus/anthology support is ever wanted.
  **Author-level monitoring** ("grab everything by this author" — the series-like shape) is
  deferred; author is metadata on the book for now.
- **Ebooks and audiobooks ship together**, modeled as **one `book` kind** whose quality
  ladder spans formats: ebook qualities (EPUB, AZW3, MOBI, PDF) and audiobook qualities
  (M4B, MP3) are ordinary quality groups in ordinary QualityProfiles. "Ebook", "Audiobook",
  or "Either" is a profile choice rather than a new persistent media kind. Collecting *both* editions of
  one book is explicitly out of scope for v1 (one edition per book per profile cutoff; an
  editions model can be revisited on demand).
- **External IDs extended**: `isbn10`/`isbn13`, `olid` (Open Library), `asin` (Audible)
  alongside tmdb/imdb/tvdb.
- **Metadata provider chosen at implementation time via a bake-off** between Open Library,
  Google Books, and Hardcover, behind the existing kind-agnostic `MetadataProvider` port
  (TMDB obviously cannot serve books — the port earns its keep here). Hard constraint learned
  from Readarr's death: **no bespoke scraping middleman we have to operate**; the adapter
  speaks a public API directly and caches aggressively.
- **Search & parsing**: Torznab book categories (7000-series ebooks, 3030 audiobooks),
  book-mode parser rules, and Readarr's GPL parser test corpus ported into
  `testdata/releases/` (same license lineage as ADR 0001).
- **Naming**: an `{Author Name}/{Book Title}`-family template set with a Calibre-friendly
  default layout.
- **Timing: Phase 2.5** — immediately after the acquisition core (Phase 2) works for movies
  and TV. Books then become the first proof that the pipeline is genuinely kind-agnostic,
  while the ecosystem gap is still open.

## Consequences

- Phase 1 schema and ports are book-aware from day one at near-zero cost (a CHECK constraint
  and a wider ids set), avoiding the painful-migration trap called out in ADR 0002.
- Monarr's pitch strengthens: the only maintained *arr-class app covering movies + TV + books.
- Two new risks enter the register (§11 items 8–9): **book metadata source quality** (the
  thing that killed Readarr) and **book release naming/matching** being far less standardized
  than scene TV/movie naming. Mitigations: provider bake-off + caching + no operated
  middleman; conservative defaults for books at launch (interactive-search-first before
  trusting auto-grab).
- Music and comics remain non-goals. The Wantable shape (an album covering tracks is the same
  shape as a season covering episodes) still leaves that door open, but nothing is promised.
