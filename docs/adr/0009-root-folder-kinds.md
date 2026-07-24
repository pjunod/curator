# ADR 0009 — Root folders carry a media kind; base directories are a picker, not an entity

- **Status:** Proposed
- **Date:** 2026-07-24
- **Relates to:** ADR [0002](0002-single-media-table.md) (one table, `kind`
  discriminates), ADR [0003](0003-compat-personalities.md) (Sonarr/Radarr
  personalities), ADR [0005](0005-filesystem-adoption.md) (adoption by
  scanning existing roots — the feature this ADR scopes)

## Context

Sonarr and Radarr never had to ask what a root folder contained: the
*application* was the answer. Radarr's roots hold movies because Radarr only
knows movies. Monarr merged the two applications into one process with a
`kind` column on `media_items` (ADR 0002) — and the discriminator was
removed from the *application* without ever being added to the *root
folder*. `root_folders` is `(id, path, added_at)` and nothing else
([0002_library.sql](../../internal/infra/sqlite/migrations/0002_library.sql)).

Three things follow, and only the third is cosmetic:

- **Adoption cannot know what it is looking at.** A candidate directory
  `Arrival (2016)` and a candidate directory `Severance` are the same shape.
  Scan surfaces both as `UnmatchedDir` and the matching UI has to ask a
  human which metadata provider to search — per candidate, forever.
- **The compat layer actively misroutes.** `rootFolders` in
  [compat.go](../../internal/compat/compat.go) returns *every* root to *both*
  personalities. A Radarr client is therefore offered the TV root as a movie
  destination, and when it POSTs that `rootFolderPath`, `rootIDByPath`
  ([sonarr.go](../../internal/compat/sonarr.go)) matches it by string and
  assigns it. The movie lands under the series tree with no error at any
  layer. This is a correctness bug today, not a future concern.
- **Free-space and UI grouping are undifferentiated.** Minor.

### What is already true, so this is not over-corrected

The fear that a root folder means "scan everything beneath it" is worth
checking against the code before designing around it, because it is mostly
not the case:

- Root discovery uses `os.ReadDir(root.Path)` — **one level, not
  recursive**. Only immediate children become adoption candidates.
- `filepath.WalkDir` is recursive, but it runs in `scanItem` — *inside*
  folders already claimed by an existing item. That recursion is wanted:
  it is how season folders and multi-file releases are found.

So Monarr does not currently spelunk an entire tree hunting for things to
import. The real gaps are narrower and sharper:

1. There is no kind, so nothing can be routed or pre-matched by it.
2. Registering a *base* (`/media`, containing `Movies/` and `TV/`) surfaces
   `Movies` and `TV` themselves as adoption candidates — the wrong depth,
   with no way to say "descend one more level here."
3. **Dismissing a candidate does not persist.** There is no exclusion or
   ignore table anywhere in the schema. Every non-media directory under a
   root is re-offered on every scan, forever.
4. Nested roots are permitted. `AddRootFolder`
   ([library.go](../../internal/app/library/library.go)) checks absolute,
   exists, is-a-directory, and path-unique — nothing stops registering both
   `/media` and `/media/Movies`, after which every movie folder is offered
   twice.

Gap 3 is the one that matches "things in there that aren't movies that you
don't want to try to import," and it is entirely missing rather than merely
under-designed.

## Decision

### 1. `root_folders` gains a `kind`, including an explicit `mixed`

```sql
ALTER TABLE root_folders ADD COLUMN kind TEXT NOT NULL DEFAULT 'mixed'
    CHECK (kind IN ('movie', 'series', 'book', 'mixed'));
```

`mixed` is not a cop-out, it is the honest value for `/media/Anime` holding
both series and films, and it means exactly one thing: **ask, do not
assume.** A `mixed` root behaves precisely as every root behaves today, so
the value has a defined meaning rather than being a null in disguise.

Migration backfills by inference: a root whose existing `media_items` are
all one kind takes that kind; a root that is empty or spans kinds becomes
`mixed`. No existing install changes behavior on upgrade — it only gains the
ability to be more specific.

A typed root then buys three things at once: adoption searches only the
right provider, the Add flow can default the destination correctly, and
compat can stop lying.

### 2. Base directories are a browse affordance, not a stored entity

The ask — "specify a base, then select the movie and TV folders under it" —
is a *registration flow*, and it does not require the base to exist in the
schema. The flow is: browse to `/media`, see its children, tick `Movies` as
`movie` and `TV` as `series`, and Monarr stores two typed roots.
`/media` itself is never stored and never scanned.

This is the right default because the compat surface is flat: Sonarr and
Radarr both expose `GET /api/v3/rootfolder` as a flat list, so any hierarchy
Monarr invents has to be collapsed again for every *arr client. A hierarchy
that exists only in Monarr's own UI, and must be flattened at the boundary,
should be justified by more than one screen's convenience.

**What would flip this:** wanting Monarr to *notice* new kind-folders
appearing under the base later — `/media/Anime` shows up in six months and
Monarr offers it. That requires remembering the base, and is a
`library_bases` table added on top of this decision without disturbing it.
It is deliberately not built now, because the browse flow delivers the
stated requirement and the watching behavior has not been asked for.

The browse endpoint is new work: a directory lister scoped to safe roots,
which the Add-root screen drives. It does not exist today.

### 3. Nested roots are rejected at registration

`AddRootFolder` gains one check: reject a path that is an ancestor or a
descendant of an already-registered root, naming the conflicting root in the
error. This is the cheapest possible fix for "don't scan everything beneath
it," because the base-as-root mistake is what produces that complaint in the
first place, and the error message is where the user learns the model.

### 4. Two exclusion mechanisms, because there are two distinct needs

They are frequently conflated and should not be:

| | Prospective | Retrospective |
|---|---|---|
| **Question** | "never look at things shaped like this" | "I looked, it's not media, stop asking" |
| **Keyed by** | name pattern | exact path |
| **Set by** | defaults + user patterns | dismissing a candidate in the UI |
| **Scope** | global or per-root | one directory |

**Prospective — skip patterns.** A per-root list plus built-in defaults,
applied during the `ReadDir` sweep. The defaults matter more than they look,
because they are the folders real libraries actually contain: dot-
directories, `@eaDir` and `#recycle` (Synology), `lost+found`,
`$RECYCLE.BIN`, `System Volume Information`, and `extras`/`featurettes`.

**Retrospective — dismissals.** An `ignored_paths` table, so that
dismissing an adoption candidate keeps it dismissed. Without this, every
scan re-offers every non-media folder and the candidate list is permanently
noisy — which is the actual lived complaint, and it is unsolved in the
*arrs too.

Both are additive to the scan sweep and neither changes the recursion depth,
which stays at one.

### 5. Compat filters roots by personality, and refuses mismatched writes

`rootFolders` returns roots whose kind matches the personality (`movie` and
`mixed` for Radarr, `series` and `mixed` for Sonarr). `rootIDByPath`
additionally *rejects* a path whose root kind contradicts the personality
rather than resolving it — otherwise a client that remembered an old path,
or one that was configured before this ADR, still writes a movie into the
series tree. Filtering the list is the affordance; refusing the write is the
guarantee.

This is kept honest by the existing contract tests in
[`internal/compat`](../../internal/compat), which should gain a case
asserting that a Sonarr-personality add against a `movie` root fails.

## Consequences

- One migration (`0014`), one new column, one new table, and one new browse
  endpoint. No change to `media_items`, and no change to how any existing
  item is stored.
- Upgrades are behaviour-neutral by construction: everything backfills to
  `mixed` or to its inferred kind, and `mixed` is defined as today's
  behaviour. Nobody's library re-scans differently the morning after.
- The Add-media and adoption UIs get simpler, not more complex — a typed
  root removes a question rather than adding a field.
- `mixed` will be the value most existing installs land on, and its
  usefulness therefore depends on the UI nudging users to narrow it. A
  `mixed` root that nobody ever types is exactly today's experience, which
  is acceptable as a floor but should not be mistaken for the goal.
- Adoption for `book` roots benefits most, because book folders are the
  hardest to tell apart from arbitrary directories by shape alone.
- Not solved here: a single directory holding both a movie and its own
  extras subtree still needs per-candidate judgement. Kind narrows the
  search space; it does not parse layouts.

## Alternatives considered

- **A `library_bases` parent table (the literal ask).** Stores the base,
  hangs typed children off it, and can notice new subfolders later.
  Rejected *for now* because the flat compat surface has to collapse it
  anyway, and because everything the requirement stated is delivered by a
  browse-and-tick flow over one flat table. Explicitly the right upgrade if
  base-watching is ever wanted — this ADR is built so that adding it later
  changes no existing row.
- **Infer kind per candidate instead of storing it.** Guess from the folder
  shape: season directories imply series, a single video file implies a
  movie. Works often, fails on exactly the libraries that need help most
  (one-season shows, movie collections in nested folders, anime), and a
  wrong guess writes a wrong metadata match that a human then has to unpick.
  A stored kind is one click that prevents a whole class of bad matches.
  Worth doing *within* a `mixed` root, where there is nothing better — but
  as a fallback, never as the design.
- **Per-root include-lists of subfolders.** Register `/media` and enumerate
  which children are libraries. Equivalent in power to typed roots, but it
  puts the scoping information in a list that has to be maintained as the
  library grows, rather than in the roots themselves. Typed roots make new
  titles work automatically; include-lists make them invisible until
  someone edits the list.
- **Keep roots untyped and filter compat by heuristics.** Guess a root's
  kind from the items already in it, at request time. Cheap, and wrong on
  empty roots, which is precisely when a client is being configured.
- **Do nothing.** Defensible only if every install has exactly one root per
  kind and never uses the compat API. The misrouting bug in §Context is
  reachable with a stock Radarr client against a stock two-root Monarr, so
  the status quo is not neutral.
