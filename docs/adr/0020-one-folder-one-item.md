# ADR 0020 — One folder, one library item

**Status:** Accepted · 2026-09-25 · v0.28.1

## Context

A library item's folder was `Title (Year)` from the naming rules, and
nothing else. Two different works can share a title and a year — two films
called *Leviticus* came out in 2022, and the user wants both — so the second
add was pointed at the first one's folder.

That cannot work. The scan links the files in a folder to whichever item it
processes last, so one of the two is always a card that looks real and holds
nothing, and which one flips from scan to scan. Import is worse: both render
the same file name inside that folder, so grabbing the second film can
replace the first one's file.

Adoption (ADR 0010) already refused to adopt a folder another item held, and
the `library-folders` health check reported shared folders after the fact.
Neither stopped Add, a root-folder edit, a typed path, or a copy's folder
from producing one.

## Decision

1. **A folder belongs to at most one item.** Enforced by a unique index on
   `media_items.path` (empty excluded), created by migration 0032. Copy
   folders (`media_copies.path`) are checked in the service against every
   item and copy.
2. **Folder choice disambiguates instead of colliding.** When the naming
   rule's folder is held by another item, the item gets the same name plus
   a provider hint: `{tmdb-N}`, else `{tvdb-N}`, `{imdb-ttN}`, `{olid-…}`,
   and `{monarr-<id>}` for items no provider backs. A counter follows only
   if even that is taken. Two adds racing for one name are settled by the
   index and a single re-pick. The first item keeps the plain name; existing
   folders are never renamed.
3. **The brace form is Plex's.** `{tmdb-N}` is Plex's documented folder hint
   and Radarr's naming token, so the disambiguated folder is unambiguous to
   whatever scans the library after Monarr. Adoption parses it, and
   Jellyfin's `[tmdbid-N]`, and uses the id to pick the one candidate it
   names.
4. **A folder the user chooses is refused, not shared.** A typed path or a
   manual entry that another item holds answers `409 folder_conflict`,
   naming the holder. A copy folder takes the same disambiguated name the
   item would. Folder repair keeps its own, stricter overlap check (`400`),
   with the index behind it.
5. **Existing damage is repaired on upgrade.** For each folder more than one
   item holds, the item with the most files keeps it (the older item on a
   tie) and each other item is pointed at its disambiguated folder. Only
   `media_items.path` changes (stored spellings are also cleaned, so the
   byte-comparing index agrees with the service); nothing on disk moves, and
   import creates the new folder on first grab.

## Consequences

- The `library-folders` health check can no longer find two items on one
  item folder. It stays, labelling each holder `Title (item N)`, for copy
  folders written before the copy path checked other items.
- An item that had been shown against the other film's files now shows none
  until its own file is grabbed. That is the truth the shared folder hid.
- Migration 0032 is a Go migration (it needs the naming rules); it is
  reversible only in dropping the index, not in un-splitting folders.
