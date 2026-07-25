# ADR 0012 — Manual entries: a library record no provider backs, with episodes read off the disk

- **Status:** Accepted
- **Date:** 2026-07-25
- **Relates to:** ADR [0011](0011-series-metadata-provider.md) (the provider
  chain, which shrinks this problem without closing it), ADR
  [0002](0002-single-media-table.md) (one `media_items` table — a manual
  entry is a row in it, not a second kind of thing), ADR
  [0010](0010-scan-adopts.md) (review, which is where a user decides a folder
  needs this)

## Context

ADR 0011 adds TVmaze and optionally TheTVDB behind TMDB, which fixes the
class of failure that prompted it. It does not fix all of it. What is left:

- Programmes no database carries — regional broadcasts, older factual
  series, things that aired once.
- Home video, concert recordings, personal archives. Plex has a whole
  library type for these; monarr has no answer at all.
- Groupings all three providers get wrong for a particular library. Rare
  after 0011, not zero.
- Anything the user simply wants filed their own way.

Today the only exit from review is dismissal, relabelled "stop offering" but
still meaning "never ask me about this folder again". The folder stays on
disk, unmanaged: no monitoring, no episode tracking, no upgrades, absent
from the library. **A media manager that cannot represent media its provider
has never heard of is telling the user their files are the problem.**

The related idea of a *mapping* — recording that `Cunk on Earth` is really
season 2 of `tv/79063` — is examined under Alternatives. It is a different
shape and, after 0011, mostly a way to avoid this one.

## Decision

### 1. A manual entry is an ordinary row with no provider behind it

`media_items` gains a `source` discriminator (`tmdb` · `tvdb` · `tvmaze` ·
`openlibrary` · `manual`) rather than a separate table. ADR 0002's single
table is the right shape here for the same reason it was then: everything
downstream — files, episodes, quality profiles, naming, the library grid —
works on `media_items`, and a parallel table would need every one of those
paths duplicated and kept in step.

A manual entry carries a title, a year, a kind, a folder, and nothing it
cannot know: no external ids, no artwork unless the user supplies a path, no
overview unless typed.

### 2. Episodes are read off the disk, not typed in

The expensive part of defining a series by hand is its episode list, and the
files already state it. Point a manual series at its folder and monarr
parses what is there — `S01E03`, `1x03`, `Part 3`, a bare ordinal — and
creates exactly those episodes, numbered as the files are numbered.

This is the whole reason the feature is affordable, and it inverts the usual
relationship in a way that happens to be correct here: for a provider-backed
series the metadata is the truth and the files are matched to it; for a
manual entry **the files are the truth**. A rescan adds episodes for new
files and never removes one whose file has gone, because a missing file is
the normal state of a monitored episode.

The parser is already tuned for exactly these names
([parser.go](../../internal/domain/parser/parser.go)) and is the same one
acquisition uses, so numbering agrees with release matching by construction.

### 3. Metadata refresh skips them — there is nothing to refresh against

Nothing to refresh, and a refresh path that treats "no provider id" as "look
it up" will either error every cycle or overwrite what the user typed. The
scheduled refresh filters them out; the per-item Refresh button is absent
rather than present-and-failing.

### 4. Monitoring and acquisition are allowed, and start off

A manual entry has a title and episode numbers, which is all release
matching needs, so there is no reason to forbid searching for one. But title
matching without a provider's alternate names and year is materially weaker,
and the failure mode is grabbing something unrelated — so a manual entry is
created unmonitored, and turning it on is a deliberate act.

### 5. The compat personalities expose them with a zero id

A manual series has no TVDB id, and Sonarr's v3 API has a required
`tvdbId` field. The personalities report `0` rather than hiding the item:
Jellyseerr and friends will list it and cannot request it, which is
accurate — there is nothing to request it *by*. Hiding it would make the
compat view disagree with the library about what exists, and a compat layer
that lies about the collection is worse than one with a gap in it.

### 6. Review offers it as the third option, next to matching and dismissing

A row that cannot be matched currently offers search and dismissal. It gains
"add as a manual entry", pre-filled with the parsed title and year — which
for the umbrella case is exactly right, since the folder name is the show's
actual name and the provider is the thing that is wrong.

## Consequences

- A second class of item that most code must *not* special-case, and a few
  paths must. Every place that assumes a provider id is a place that needs a
  decision: refresh (§3), compat (§5), the id-collapse rule from ADR 0011 §4
  (two manual entries must never merge on "both have no ids").
- Users can now create junk the library will carry forever. Deletion already
  exists and leaves files alone, which is the mitigation; the risk is real
  and small.
- Artwork will be missing, and the library grid will look patchy where these
  entries land. A local poster path is the cheap fix and should ship with it
  rather than after.
- It undercuts the pressure to solve hard matching problems properly. That
  is the honest cost: an escape hatch gets used instead of a fix. Worth
  accepting because the alternative is telling users their files are wrong,
  and worth watching in the sense that a rise in manual entries for
  *provider-backed* content is a bug report about matching.
- This is the feature that makes monarr usable for content the *arrs never
  served. That is a larger consequence than the problem that prompted it.

## Alternatives considered

- **A mapping table: "this folder is season 2 of `tv/79063`".** The shape
  the *arr ecosystem uses for anime (xem, anime-lists), and the direct
  answer to "mark this oddball grouping". Rejected as the primary: it needs
  multi-folder series support to be useful at all (three Cunk folders, one
  item), the resulting library entry still shows the strand's title, poster
  and overview rather than the show's, and after ADR 0011 the population it
  serves is small. It remains the right answer for anime numbering
  specifically, which is a separate problem with its own existing data
  sources.
- **`.nfo` sidecar files, Kodi/Jellyfin style.** Metadata lives next to the
  media, survives a database loss, and is portable to other tools. Genuinely
  attractive, and it makes the filesystem an input monarr must parse, trust
  and reconcile against its own database — a second source of truth for
  every field. Worth revisiting as an *import* path (read once, on adoption)
  rather than as the storage format.
- **Let users edit any item's fields, provider-backed or not.** A superset,
  and it breaks refresh: the next scheduled refresh either discards the edit
  or has to track per-field overrides forever. Manual entries avoid this by
  having nothing to refresh against.
- **Do nothing; dismissal is the answer.** What ships today. Defensible only
  if every file a user owns is in a provider database, which is not true and
  is least true for the content people care most about keeping.
