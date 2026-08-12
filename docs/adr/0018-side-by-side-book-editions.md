# ADR 0018 — Ebook and audiobook editions coexist

- **Status:** Accepted
- **Date:** 2026-08-12
- **Supersedes:** [ADR 0017](0017-audiobook-curation.md) where it derives a
  book's medium from its quality profile and limits one work to one edition

## Context

ADR 0017 made audiobook acquisition usable, but modeled `ebook` and
`audiobook` as profile families. That made the selected profile answer two
different questions: what medium the user wanted and which formats they
preferred within that medium. It also made the editions mutually exclusive.
Changing an ebook's profile to Audiobook did not add an audiobook; it changed
the identity of the only copy and displaced the ebook.

The existing additional-copy pipeline already gives a target its own profile,
monitoring, wanted identity, downloads, files, imports, and upgrades. The
missing piece is an explicit book-medium identity that the profile cannot
change.

## Decision

- One Open Library work remains one `media_items` aggregate. It can own at
  most one `ebook` edition and one `audiobook` edition at the same time.
- Persist `book_type` independently on the primary item and on additional
  book targets in `media_copies`. The latter is an implementation reuse of the
  acquisition-target lifecycle; product surfaces call these **editions**, not
  quality copies.
- A quality profile is policy within an edition. Ebook editions accept only
  ebook profiles and audiobook editions accept only audiobook profiles.
  Changing a profile can change preferred formats, but never the medium.
- Adding an already-owned Open Library work adds the requested missing
  edition. Adding a medium already present remains a duplicate. The detail
  screen also offers the missing edition directly.
- Search, grab, wanted, download, import, monitoring, files, and upgrades are
  resolved per edition. Existing `copyId` scoping names a non-primary edition
  through those flows; the primary remains `copyId = 0`.
- API book rows expose `bookTypes` for every owned edition and retain
  `bookType` for the primary edition. Book copy rows expose their own
  `bookType`.
- Editions share the normal `{Author}/{Title}` folder unless a different root
  is deliberately selected. In a shared folder, scans use the file extension
  to attribute ebook and audiobook files to the matching edition and may
  repair an older primary-only attribution.
- Migration 0029 backfills existing books once from their current profile.
  Thereafter the persisted medium is authoritative.

## Consequences

- Users can acquire and keep an ebook and audiobook side by side without
  duplicating metadata or losing either edition.
- Ebook and Audiobook library sections can both contain the same work. Their
  counts describe editions, while the All section still counts the work once.
- Automatic search can process both missing editions in one pass; interactive
  search and manual grabs remain scoped to the edition whose control was used.
- The database continues to call the secondary target a media copy. Keeping
  that storage vocabulary avoids a parallel acquisition pipeline, but domain,
  API, and UI code must use edition language for books.
- Audio duration, codec/container, chapters, playback, resume, and listening
  progress remain Plurx responsibilities. Monarr manages acquisition,
  placement, format preference, and edition completeness.
