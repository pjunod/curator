# ADR 0017 — Audiobook acquisition and curation

- **Status:** Accepted
- **Date:** 2026-08-11

## Context

ADR 0006 shipped ebooks and audiobooks under one `book` media kind, with
their format family selected by a quality profile. That was sufficient for
the decision engine but not for a usable audiobook workflow. The add and
library screens called every edition “Book,” default search mixed ebook and
audiobook categories, and an MP3 release with several tracks imported only
its first file because every later track competed for the same single-file
name.

The metadata identity is still one Open Library work. Splitting audiobooks
into another persistent media kind would duplicate metadata, roots, matching,
and most of the acquisition pipeline without solving the actual boundary:
search and file placement need to know the selected profile's format family.

## Decision

- Keep `book` as the persistent media kind. Derive a required-at-use-time
  subtype, `ebook` or `audiobook`, from the selected profile target.
- Expose that subtype in library API responses and accept it on book adds.
  An add without an explicit profile uses an independent default for that
  subtype. Existing `default_profile.book` remains the ebook setting for API
  and database compatibility.
- When an indexer has no explicit categories, route ebook searches to
  Newznab 7000/7020 and audiobook searches to 3030. Explicit indexer
  categories remain authoritative.
- Recognize MP3, WMA, AAC, OGG, Opus, M4A, M4B, FLAC, and WAV as audiobook
  formats. The extension remains the importer/scanner's format authority;
  Monarr does not invent codec or chapter metadata it has not probed.
- Treat a multi-file audiobook payload as one edition. Validate every part
  before placement, sort paths deterministically, and name them
  `Title - Author - 001.ext`, `002`, and so on. Apply upgrade/replacement to
  the batch rather than making its tracks compete with each other.
- Make Ebook and Audiobook distinct choices and library views in web and
  native clients. Profile pickers never offer a format family that the item
  cannot use, while a book detail screen may deliberately switch between the
  two book families.

## Consequences

- Existing ebook items, settings, paths, and API clients keep working.
- Audiobook searches spend indexer calls only in the relevant category and
  multipart releases can satisfy one wanted book.
- A book remains one curated edition. Keeping an ebook and audiobook of the
  same Open Library work simultaneously still needs an editions model and is
  explicitly deferred.
- Audio duration, codec/container inspection, embedded chapters, playback,
  resume, and progress belong to the media server/player. Monarr curates the
  files and format family; plurx supplies those playback concerns.
