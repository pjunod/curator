# ADR 0010 — Scan proposes matches; adoption is bulk work, not a to-do list

- **Status:** Accepted — implemented
- **Date:** 2026-07-24
- **Relates to:** ADR [0005](0005-filesystem-adoption.md) (adoption is *the*
  migration path — this is the part of it that was never built), ADR
  [0009](0009-root-folder-kinds.md) (root kind, which this needs), ADR
  [0008](0008-distributed-execution.md) (the job queue, whose first real
  workload this is)

## Context

ADR 0005 committed filesystem adoption as the migration path, in these
words: "Library reconcile walks existing root folders, parses on-disk files,
and matches against TMDB." The first half shipped. The second did not.

What `Scan` actually does with a directory no item claims is append it to
`report.UnmatchedDirs` and move on
([scan.go](../../internal/app/library/scan.go)). The report is a list of
names. Every name is then matched by hand through the Add page.

The pieces to do better all exist and are simply not connected:

| Piece | Where | Used at scan time? |
|---|---|---|
| Folder name → title, year | `parser.Parse` ([parser.go](../../internal/domain/parser/parser.go)) | No |
| Title comparison, normalization | `matcher.TitleMatches` ([matcher.go](../../internal/domain/matcher/matcher.go)) | No |
| Provider search | `Library.Search(kind, query)` | No |

`matcher` is wired only into acquisition, where it scores *releases*
against *wanted items*. Nothing scores an *on-disk folder* against *the
metadata provider*, which is the one join adoption needs.

The consequence is the reported experience: point Monarr at a real library,
press Scan, receive several hundred prompts. ADR 0005's "adopts with zero
renames" is true about renames and quietly false about effort — the renames
were never the expensive part. **A migration path that costs one human
decision per title is not a migration path**, and it is the first thing a
prospective user hits.

## Decision

### 1. Scan proposes a match, never a bare name

For each unclaimed directory: `parser.Parse` the folder name for title and
year, search the provider for the root's kind (ADR 0009 — this is why root
kind is a prerequisite, not a nicety), score the results, and carry the
proposal into the report. A candidate becomes
`{path, parsedTitle, parsedYear, proposals[], confidence}` instead of a
string.

Even when nothing is auto-adopted, this alone converts the review from
"search for each of 600 titles" into "confirm or correct 600 pre-filled
answers." Those are different orders of work.

### 2. Auto-adopt only on an unambiguous match, and the bar differs by kind

**A wrong auto-match is worse than an unmatched folder.** An unmatched
folder is visible and annoying; a wrong match writes a path assignment and
metadata that looks right until someone notices the poster is wrong, and
then has to be unpicked by hand. The threshold should be set where false
positives are close to impossible, accepting that more folders land in
review.

- **Movies require title *and* year.** Normalized title equality plus year
  within ±1, and exactly one result clearing that bar. Movie folders almost
  always carry a year, and without one, remakes and same-title films are a
  coin flip.
- **Series may match on title alone**, but only when the search returns
  exactly one result whose normalized title is equal. Series folders
  routinely have no year (`Severance`, `Andor`), so requiring one would
  send every well-formed TV library to manual review.
- **Books require title and author** (ADR 0006), since title collisions are
  endemic and the parser already extracts an author when the name carries
  one.
- **Anything else goes to review** with its top proposals attached.

### 3. A `mixed` root never auto-adopts

Unknown kind means searching two or three providers and picking between
them, which is precisely the ambiguity §2 refuses to guess through. A
`mixed` root proposes across kinds and always asks.

This is deliberate: it gives ADR 0009's kind an immediate, visible payoff,
so typing a root is something the user is rewarded for rather than nagged
about.

### 4. Adoption is queued work, not an HTTP request

Six hundred directories is six hundred provider searches. That cannot live
inside the request that pressed Scan: it exceeds any sane timeout, it hits
provider rate limits, and a failure halfway through leaves nothing.

Enqueue one adoption job per candidate, with progress reported to the UI.
**This is ADR 0008's job queue earning its keep on a single instance**,
which is exactly the argument that ADR made for building step 1 before any
clustering — retries when TMDB rate-limits, visible per-item failure, and
resumption after a restart. Adoption is the concrete workload that makes
the queue worth building first.

Where the queue does not yet exist, adoption runs as a bounded background
task with the same reporting shape, so the UI does not change when the
queue lands.

### 5. First adoption of a root is reviewed; steady state is automatic

These are different situations and should behave differently:

- **Bulk adoption** — a root registered for the first time, hundreds of
  candidates. Propose everything, show what *would* be adopted, apply on
  confirmation. Once, at the moment the user is least able to spot a wrong
  match among hundreds, and most able to lose trust over one.
- **Incremental** — one new folder appears in an already-adopted root.
  Auto-adopt at the §2 bar without asking, because a single arrival is
  reviewable in the activity feed and asking makes the common case tedious.

The distinction is whether the root has been adopted before, which is one
boolean on the root.

## Consequences

- Adoption becomes the feature ADR 0005 described. That ADR needs no
  amendment — it is not being reversed, it is being finished.
- Provider quota becomes a real constraint at adoption time. TMDB's limits
  are generous and OMDb's free tier is 1000/day (see the Settings copy), so
  bulk adoption must be rate-aware and resumable rather than fast.
- Review is still required, and should be. The goal is to move the median
  library from "hundreds of decisions" to "a handful of genuinely ambiguous
  ones" — not to zero, which would require guessing.
- The `UnmatchedDir` shape in the scan report changes, and so does the
  Library folders panel that renders it. The three-way `movie · series ·
  book` links added when that panel was merged are an interim affordance;
  they disappear once a proposal carries its own kind.
- Anime and daily shows remain the known-lossy edge ADR 0005 already
  flagged. They will land in review, which is the correct outcome.

## Alternatives considered

- **Leave it manual.** Defensible for a 20-title library and indefensible
  for the 600-title libraries ADR 0005 explicitly targets as the adoption
  audience. The people most worth winning are the ones with the most to
  migrate.
- **Auto-adopt on best fuzzy score.** Rank results and take the top one.
  Much higher coverage, and it will confidently assign the wrong remake to
  a folder — the failure mode §2 is built to avoid. Fuzzy ranking is the
  right way to *order proposals for review*, not to decide.
- **Match on file contents rather than folder names.** Read embedded
  metadata, or hash and look up. More accurate in principle, dependent on
  tagging that scene releases do not carry, and far slower over a network
  mount. Folder and file names are what the *arr ecosystem has always
  matched on, and the parser is already tuned for them.
- **Import the Sonarr/Radarr database instead.** Already the ADR 0005
  stretch item, and it remains the better answer for users migrating from
  the *arrs specifically — it carries monitoring state this path cannot.
  It does nothing for a library that was never managed by an *arr, which
  is why it is a complement rather than a substitute.
