# Wanted groups — expandable titles and searches with explicit scope

**Status:** historical design; implementation merged in PR #32 (`7ceaf9d`) ·
**Written / revised:** 2026-09-19 · **Build baseline:** `8ce3590` or a descendant

Preserved during repository cleanup on 2026-09-29. The requirements, source
contracts, and verification notes below record the original proposal, not
new implementation instructions. For delivered behavior and validation, see
[implementation status](wanted-groups-status.md) and [Usage](usage.md).

Companion to [Usage](usage.md) (the Wanted workflow) and
[Architecture](architecture.md) (acquisition and background work). Read §1–§4
before implementing the milestones in §6. This is a handoff for Sol.
Re-verify every source contract against the implementation branch first.

**Baseline resolved:** Fable inspected `8ce3590`, the merge of PR #28
(`codex/wanted-filter-pagination`), after the release-identity work in PR #27.
Its review establishes the PR #28 facts below; this revision independently
checked queue, grab bookkeeping, and retry contracts in the local `0da1c75`
checkout. The newer commit is not available locally. No runtime verification
was performed for this plan or Fable's review.

Sol must implement on `8ce3590` or a descendant containing both PRs, preserving
unrelated edits and untracked plans in the local working tree. Do not build
from or reset the stale local checkout. The newer Wanted page has client-side
pagination over the complete flat `/wanted` array, text search, title/reason/
kind sorting in either direction, and a labeled reason selector. Its exact
reason vocabulary is `missing | upgrade`, derived from `missing`; there are
no other current reasons. The API fields are unchanged. Retain these controls
with the grouped semantics below; no server pagination endpoint is needed.

Standing instruction: retain the established Wanted eligibility rules. If a
display category is informational rather than searchable, show that clearly;
do not make an item eligible just to enable its button.

## 1. Required behavior

The Wanted page groups each library title into one expandable row. A show
appears once even when many seasons, episodes, or quality copies need work.
You can search the group, expand it and search an individual entry, search
all entries for one reason, or use the retained **Search all now** button.

### 1.1 Group rows represent library items

Group by `mediaItemId`, never by title text or season. Two remakes with the
same title remain separate. Movies and books use the same model: one work
with its wanted copies/editions underneath. A single-target movie or book
can show its detail and Search directly without a redundant expander.

Each group shows its title/link, media kind, matching wanted-target count,
counts by reason, an expand/collapse button, and a search button. Default
groups to collapsed. For shows, expanded rows show season/episode, copy or
edition, reason, current quality when applicable, and **Search episode**.
Use **Search item** for non-episode children. Label primary targets explicitly
when multiple copies would otherwise be ambiguous; use “edition” for books.

Keep title/reason/kind sorting and ascending/descending direction. Title is
the default. For reason sorting, classify a group as Missing if any of its
reason-visible children are Missing, otherwise Upgrade; ascending puts
Missing first. For kind sorting use the structured media kind's lexical
order (`book`, `movie`, `series`). Reverse only the primary key for descending;
ties use title ascending then `mediaItemId` ascending. Sort children always
by numeric season and episode, then copy ID, so S02 precedes S10, regardless
of the group sort. Never use the raw ID string for numeric ordering.
Season headings inside an expanded show are fine; separate season-level
actions are outside this change.

### 1.2 All four search scopes must be visible and truthful

| Control | Exact scope |
|---|---|
| **Search all now** in the page header | Every eligible wanted target across all titles, reasons, and pages. Ignores display filters. |
| **Search all Missing (N)** beside the Missing reason filter | Every eligible Missing target across the entire wanted list, including collapsed groups and other pages. |
| **Search all [reason] (N)** for each other reason | The same operation restricted to that exact reason. |
| **Search show (N)** / **Search group (N)** on an unfiltered group | Every wanted target belonging to that `mediaItemId`, across all its copies and seasons. |
| **Search Missing in show (N)** when Missing is selected | Only matching Missing targets in that show. Apply the same labeling and restriction for any selected reason. |
| **Search episode** / **Search item** on a child | Only that exact `wantableId`, including its copy/edition discriminator. |

Keep the labeled reason selector (All, Missing, Upgrade) and add visible
**Search all Missing (N)** and **Search all Upgrade (N)** actions beside it.
A dropdown hidden in a row menu is insufficient. Counts are target counts,
not group counts, and
reason totals span all pages. Counts describe selection, not a promise of
successful downloads: in-flight or subsequently satisfied entries may skip.
Disable zero-target actions and explain non-searchable categories inline.

Reason actions always mean the whole reason category, irrespective of text
search. State beside these actions that they span all titles. Retain text
matching on title, detail, copy, kind, reason, quality, and raw `wantableId`;
matching an opaque ID string is fine, parsing it for metadata is not. A text
hit on any reason-visible child includes the whole group with **all** its
reason-visible children. It must not hide siblings or reduce the group action
count. Reason filtering alone narrows a group's children and its search scope.

```text
Wanted                                        [Search all now]
All (214)  Missing (180) [Search all Missing (180)]
Upgrade (34) [Search all Upgrade (34)]

▾ Example Show     12 wanted · 8 missing · 4 upgrade [Search show (12)]
    S01E01 · Primary · Missing                      [Search episode]
    S01E02 · Primary · Upgrade from 720p            [Search episode]
    S01E01 · 4K copy · Missing                      [Search episode]
▸ Another Show     18 wanted                       [Search show (18)]

1–25 of 42 titles · 214 wanted items          [Prev] [Next]
```

The numbers above are illustrative. In a Missing-filtered view the first
show contains eight visible children and its button says
**Search Missing in show (8)**; its four upgrades cannot enter that search.

### 1.3 Pagination applies to groups

Filter children by reason, group by item, retain groups having a text-search
hit, then sort and paginate groups. Text search never removes children from
a retained group.
Never paginate flat episodes and group only the page slice: that splits a
show across pages and makes group counts and searches incomplete.

Reuse PR #28's helpers in `web/src/wanted.ts` and the existing
[Pager primitives](../web/src/Pager.tsx). Preserve page sizes
`[25, 50, 100, 200, 500, 0]` (0 = All) and the user's size preference;
set `PageSizePicker.label` to “Titles per page.” Expansion must not alter the
outer page count. Reset to page one on filter changes, clamp after refresh,
and retain expansion for surviving IDs during polling and page navigation.
If a very large show's children need their own pagination, the group search
must still include all matching children, not that child page alone.

Show loading, request failure with retry, an empty library-wide wanted list,
and an empty filtered result as distinct states. Expand controls need
`aria-expanded`, a named controlled region, and keyboard support. Child
search clicks must not toggle the group. Preserve loop timing information.

## 2. Existing contracts and the bugs they can hide

These signatures and fields exist in the local inspected baseline and were
confirmed by Fable at `8ce3590`. Re-verify at build time.

```go
// internal/app/acquisition/automation.go
func (s *Service) WantedList(ctx context.Context) ([]WantedSummary, error)
func (s *Service) AutoSearchItem(ctx context.Context, itemID int64) (AutoSearchOutcome, error)
func (s *Service) BacklogSearch(ctx context.Context) error

// WantedSummary JSON fields:
// wantableId, mediaItemId, title, detail, missing, current, copy

// internal/app/acquisition/wanted.go
func (s *Service) Wanted(ctx context.Context) ([]domain.Wantable, error)
func (s *Service) notInFlight(ctx context.Context, wanted []domain.Wantable) []domain.Wantable

// internal/infra/jobs/jobs.go
func (q *Queue) Enqueue(ctx context.Context, j domain.Job) (int64, error)
func (q *Queue) EnqueueUnique(ctx context.Context, j domain.Job) (bool, error)
```

| Surface | Inspected behavior | Implementation consequence |
|---|---|---|
| [Wanted UI](../web/src/pages/Wanted.tsx) | Child Search calls `autoSearchItem(mediaItemId)` and tracks “kicked” by item ID forever. | It does not search one episode. Replace both scope and pending/result tracking. |
| [Automatic acquisition](../internal/app/acquisition/automation.go) | `AutoSearchItem` builds season-pack targets for a series and includes monitored copies. | Do not reuse it as an exact-child or reason-restricted implementation. |
| Same service | `BacklogSearch` has `backlogPerRun = 20`. | Triggering it once cannot fulfill an explicit all-target operation. |
| [Wanted index](../internal/app/acquisition/wanted.go) | Series contribute episode wantables; copies have separate identities. | Grouping is presentation; preserve individual target identity. |
| [Wantable IDs](../internal/domain/wantable.go) | Examples: `episode:42:2:5`, `episode:42:2:5:c3`, `movie:42`, `movie:42:c3`. | Keep IDs opaque in the client. Resolve IDs against server-owned wantables. |
| [Native API](../internal/api/acquisition_handlers.go) | `/wanted` returns an array; item autosearch is synchronous with a five-minute request context. | Preserve existing consumers; large explicit scopes need server-owned background execution. |
| [Queue](../internal/infra/jobs/jobs.go) | Leased jobs and deduplication already exist. | Reuse the queue instead of browser request loops or detached goroutines. |

Preserve the flat `GET /api/v1/wanted` contract for dashboard, global search,
mobile, metrics, and existing tests. Group the complete returned array in the
web client. Compute global reason counts before display filtering/pagination.

Add structured `reason`, `kind`, `copyId`, `season`, and `episode` fields. Keep
old fields for compatibility; use null/omitted episode coordinates for
movies/books. `kind` is `movie | series | book`, not the wantable-ID prefix.
Replace PR #28's `wantedKind()` prefix parsing and `wantedReason()` boolean
derivation with these fields. Do not parse localized `detail` to sort or
identify targets. One server classifier drives reasons and search selection.
Parameterize reason tests over exactly `missing` and `upgrade`; adding future
reasons requires extending that contract, not inventing categories here.

## 3. Search contract — select targets on the server

The following is a **proposed additive contract**, not an existing API. Reuse
an equivalent search-run facility if the implementation branch has one.

```ts
type WantedReason = 'missing' | 'upgrade'
type WantedSearchScope =
  | { scope: 'all' }
  | { scope: 'reason'; reason: WantedReason }
  | { scope: 'group'; mediaItemId: number; reason?: WantedReason }
  | { scope: 'target'; wantableId: string }

type WantedSearchRequest = WantedSearchScope & { targetDelayMs?: number }
// POST /api/v1/wanted/searches, body WantedSearchRequest -> 202 { runId: string }
// GET /api/v1/wanted/searches/{runId} -> WantedSearchRun
// POST /api/v1/wanted/searches/{runId}/cancel -> 202 WantedSearchRun
type WantedSearchRun = {
  runId: string
  scope: WantedSearchScope
  scopeLabel: string // e.g. "Missing in Example Show"
  status: 'queued' | 'running' | 'completed' | 'failed' | 'interrupted' | 'cancelled'
  createdAt: string // RFC 3339 UTC; persisted
  startedAt?: string
  finishedAt?: string
  cancelRequestedAt?: string
  targetDelayMs: number
  selected: number
  processed: number
  searched: number
  skipped: number
  failed: number
  grabbed: number
  error?: string
}
```

Validate discriminated request shapes strictly: unknown reason, invalid
scope, extra conflicting selector, malformed ID, or nonpositive group ID
returns 400. A nonexistent library item/target returns 404. A known target
that ceased to be wanted is a successful no-op with a `no_longer_wanted`
result. Empty all/reason/group selections complete with zero targets. No
enabled indexers must produce the existing explicit configuration error,
never a successful “nothing found.” Apply native API authentication.

At acceptance, resolve the complete scope using fresh authoritative wanted
state and persist its exact target IDs and selected reason. Do not accept a
page's IDs as an implementation of “all.” Membership is a snapshot: newly
wanted targets join the next run. Before executing each selected target,
refresh its eligibility/reason, monitoring, copy, and in-flight coverage.
Targets that leave the requested reason skip; never substitute another
episode or copy. Resolve against current domain objects, not unchecked ID
coordinates supplied by the client.

A full wanted rebuild is allowed at acceptance, not before every target.
Refresh a target from its own current item/copy using `target`/`targetCopy`,
or `episodeStates` and `wantedEpisodes` for that one series. Apply `wants()`
and `notInFlight` to the resolved target. Never invalidate and rebuild the
whole library in the per-target loop: grabs can invalidate that cache and
turn the loop into O(targets × library) reads.

### 3.1 Execute exact targets through acquisition

Add a scoped acquisition entry point that accepts the resolved wantables and
uses the existing `searchAndGrabBest`, profile decision engine, blocklist,
size checks, and copy-aware grab coordinates. Group/all/reason searches
are collections of exact targets, not calls to `AutoSearchItem` per group.

For this change, series searches use episode targets. Do not promote a
Missing subset into a whole-season search; the baseline's season-pack
strategy can include upgrades or other targets outside the selected scope.
Leave existing library-detail autosearch's season targeting intact.

**Mandatory candidate predicate:** reject `parsed.SeasonPack` or
`len(parsed.Episodes) > 1` on this new path for every target kind, even if all
covered episodes fall inside the selected scope. The matcher otherwise
accepts a pack for an episode, but `Grab` records only the requested target
ID. A second sibling search would not see the first pack as covering it and
could grab another pack. Before a future optimization allows these releases,
grab bookkeeping must record all covered IDs and in-flight checks must use
that coverage. That redesign is outside this change.

Add a candidate predicate parameter or a scoped wrapper around
`searchAndGrabBest`. Apply the predicate both to the indexer executor's
eligible-hit early-stop decision **and** to final candidate ranking/grabbing.
Applying it only to the executor's `eligible` closure leaves the final loop
able to grab a rejected pack. A pack-only first query must not suppress a
later query that could find an eligible single-episode release.

**Regrab limit:** explicit Wanted runs honor `regrabCapped`: three failed
grabs within twelve hours produce a `regrab_capped` skip, with existing
history reporting. Recheck before grab as well as before search. Failed
historical rows below the cap remain retryable; they are not “in flight.”
The existing `wantableOnRow` prefix logic mishandles season coverage across
copies. Fix that narrowly when wiring this guard: parse season/item/copy
coordinates and require the same copy (primary is 0). Test failed primary and
secondary packs against both primary and secondary episodes. This small
prerequisite prevents both undercounting and cross-copy suppression; it is
not permission to redesign release coverage or change the cap/window.

**Indexer cost:** PR #27's automatic query planner permits up to one ID plus
two title queries per indexer per target, stopping early on an eligible hit.
A missing 60-episode, three-season show can therefore issue 180 queries per
indexer here versus nine for the item page's season searches. This is an
intentional cost of exact scope; packs cannot satisfy this new path. Group
tooltips and Usage must say “Searches individual episodes; use the item's
Auto search for season packs.” The item action has broader scope and is not
a substitute for Missing-only search.

**Pacing:** `targetDelayMs` is an optional integer from 0 through 60,000,
default 1,000 ms, persisted per run. Expose it in a small advanced control;
keep the main search buttons one-click. It is the minimum gap between the end
of one target and the start of the next, not a rate-limit guarantee. Use job
`RunAfter` for the gap rather than sleeping in a queue worker. Retain indexer
timeouts and existing 429/error behavior; do not add unbounded retries.

**Overlap guard:** use a shared, cancellation-aware acquisition reservation
from before search through final in-flight validation and grab. All automatic
callers participate: explicit Wanted, backlog, RSS's direct auto-grab path,
item autosearch, and failure re-search. A bare per-wantable key cannot arbitrate
season versus episode targets. Use a conservative `(mediaItemId, copyId)` key
so season and episode work for the same copy serialize; different copies
remain independent. Acquire at entry points and do not reacquire recursively
in shared helpers. Waiting callers revalidate eligibility/in-flight state
after acquiring, and release on every exit. Cancellation while waiting must
not begin search. This prevents same-process automatic overlap; it is not a
claim of distributed or manual-grab exclusion. Multiple acquisition processes
would require a store-backed reservation before claiming that guarantee.

### 3.2 Run all selected work without a request timeout or silent cap

Use the existing leased queue, with persisted selection, cursor/results, and
run status in SQLite. Wire acquisition handlers in
[main](../cmd/monarr/main.go) using the pattern in
[library jobs](../internal/app/library/jobs.go). Add a narrow app-layer queue
interface; do not import queue infrastructure into acquisition.

**One active run:** enforce the invariant atomically in storage. A distinct
request while a run is queued/running (including cancellation draining) returns
409 `{ activeRunId, canCancel: true, message }`, with a message pointing to
Cancel on the active run. Identical normalized scope/pacing returns its run
ID; it must not resnapshot membership. Finished runs do not block new work.

**One target per job:** the default queue has two workers shared with scans,
adoption, probes, and identity refresh. Use a chunk of exactly one target to
release the worker after every search. A chunk reads the durable cursor,
executes one target, commits result/counts/next cursor and `nextReadyAt`, then
returns `nil`. Per-target failures are results, not job errors; only storage
or run-infrastructure failures retry the job (default maximum three attempts).
Lease loss/shutdown stops work and follows durable recovery, never masquerades
as a completed target. Preserve the existing bounded indexer fan-out. Use
normal queue priority (100), rather than repeatedly outranking background
jobs, and `RunAfter = nextReadyAt` for pacing.

Each chunk payload identifies `runId` and the expected cursor/target ordinal.
If that ordinal is already terminal, return `nil`; never use the retry of an
old chunk to consume the next target and bypass its pacing.

**Continuation owner:** an acquisition run coordinator, wired in main,
subscribes to `jobs.JobFinished`. On `done`, it reconciles the persisted run
and enqueues its next chunk if work remains. Dedupe key:
`wanted.search:<runId>`. Do **not** call `EnqueueUnique` for that same key
inside its leased handler: queued-or-leased dedupe rejects the continuation.
Enqueue only after the predecessor is terminal. Persist the job/run link
with enqueue, or provide transactional lookup by dedupe key so a crash cannot
orphan an accepted job. Cursor/result commits must be conditional on ownership
and run state; a stale handler cannot overwrite newer work or cancellation.

**Recovery owner:** the same coordinator reconciles on startup and every two
seconds because bus events can be dropped. For each queued/running run:

First honor a persisted cancellation: do not enqueue more work, and finalize
it once no chunk is leased. A remaining queued chunk must observe the terminal
run and exit without search. For a run without cancellation:

1. If a queued or leased job exists, leave it alone; lease recovery belongs
   to the queue. Never create a competing chunk for an expired lease.
2. If the current linked job is terminal failed, mark the run `interrupted`
   with its error and `finishedAt`, including when its event was lost. Do not
   reset attempts by silently creating another job.
3. If the job completed or the run has no job yet, enqueue one continuation
   if targets remain, honoring `nextReadyAt`; otherwise complete the run.

A `JobFinished` failure triggers step 2 immediately. Reconcile storage rather
than trusting an event's payload to identify current work: delayed events for
an older chunk must not interrupt a newer chunk. Startup repair covers both
“run committed, first job missing” and “cursor committed, next job missing.”
Transient reconciliation/storage errors are logged and retried on the next
tick; the UI must surface failure to read status instead of claiming success.

**Cancel:** show a Cancel button on the progress panel. The cancel endpoint
persists `cancelRequestedAt` and returns 202 with current run state. Check it
before every target and immediately before grab; no later target may start.
A queued run with no leased chunk becomes `cancelled` immediately. For active
work, show “Cancelling” until the current bounded operation drains, checkpoint
its actual result, then mark `cancelled` with `finishedAt`; until then the
active-run slot stays occupied. Already submitted downloads are not undone.
The coordinator finalizes cancellation and marks remaining targets skipped
as `cancelled`, including after restart. Conditional updates resolve races:
if completion committed first, return that completed state; otherwise cancel
wins. Repeated cancel on any terminal run returns 200 with unchanged state;
unknown run is 404. No extra confirmation dialog is required.

Keep the scheduled backlog cap and cadence unchanged. The explicit run uses
its own continuation/cursor; repeatedly triggering the capped backlog would
revisit the same first targets and can starve the rest.

Committed targets are not replayed on worker retry: reread the cursor and
results, and return without advancing a second target if this chunk already
checkpointed. Handle the crash window between external grab and checkpoint
using download records/idempotency and fresh in-flight checks; do not claim
exactly-once external effects without evidence.

Keep per-target seen/matched/accepted/grabbed/error fields and extend the
closed `AutoSearchTargetSkipped` enum in OpenAPI with `no_longer_wanted`,
`reason_changed`, `regrab_capped`, and `cancelled`, retaining `unmonitored`
and `downloading`. Deleted targets use `no_longer_wanted` with explanatory
text. Regenerate Go types and update web types/formatters together.
Expose reports through a paginated run-results endpoint so progress polling
does not return thousands of rows. Prune terminal runs and their results on
the existing queue reaper's hourly tick with the same `jobs.Options.Retention`
cutoff (default seven days), through a narrow cleanup hook/store operation.
Never purge active runs or introduce a second independent retention policy.

### 3.3 Feedback describes outcomes, not just button clicks

While a run is active, show “Searching 37 of 180 selected items,” its resolved
scope label, timestamps, Cancel control (or “Cancelling”),
and separate searched/skipped/failed/grabbed counts. `processed` is the number
of terminal targets and equals searched + skipped + failed; `grabbed` counts
successful releases and is a subset of searched. A failed attempt can still
carry candidate tallies. “Completed” means all targets were processed, not
that every search succeeded. Infrastructure-level termination is failed or
interrupted and must explain unfinished work. Cancellation is separate from
failure: once drained, cancelled remaining targets count as skipped, so the
same count invariant holds. A queued cancellation has no `startedAt`.

Show completion with access to per-target results, including partial errors,
no matches, rejected candidates, and already-downloading skips. Adapt
[autosearch formatting](../web/src/autosearch.ts) as useful, but never hide
errors just because another target grabbed successfully. Persist the active
run ID in page state/storage and reconnect on reload; handle expired/missing
runs explicitly. Clear pending state on every terminal outcome, and invalidate
wanted, queue, and relevant item queries. A grab need not remove an entry
before import, so do not equate a unchanged Wanted count with failed search.

## 4. Guardrails

1. **Keep eligibility policy unchanged.** Unknown on-disk quality is not
   missing, per [ADR 0013](adr/0013-measured-quality.md). Respect targets and
   upgrade switches from [ADR 0014](adr/0014-target-profiles.md).
2. **Keep copies and editions independent.** Group presentation does not merge
   their profiles or wanted/download identities. Preserve
   [ADR 0018](adr/0018-side-by-side-book-editions.md).
3. **Preserve the newer pagination and reasons.** This handoff is additive to
   the user's working page, not a request to restore the older baseline.
4. **Do not change RSS or scheduled backlog policy.** Their cadence and safety
   limits have separate operational purposes. The shared overlap reservation
   in §3.1 is required; it does not change their selection policy.
5. **No mobile redesign, multi-select UI, new reason taxonomy, season-level
   action, or general job dashboard.** Keep this implementation focused on
   the requested web page and the backend scope correctness it requires.
6. **Keep existing autosearch/API consumers working.** Add scoped behavior
   through the new contract; do not silently repurpose library-detail search.

## 5. Regression fixtures and observable acceptance

Use a show with Missing and Upgrade targets across at least two seasons,
including episode numbers 2 and 10, and a secondary copy. Add a second show
with the same title but a different ID, a movie, and a book with two editions.
Seed more than one outer page and more than 20 eligible targets. Use recorded
fake-indexer requests and fake grabs to assert scope, not only button labels.

| Scenario | Required assertion |
|---|---|
| Initial list | One row per media item; duplicate titles remain distinct; child ordering is numeric. |
| Sort controls | Title/reason/kind work in both directions with deterministic ties; child order stays numeric. |
| Text search | A hit on one reason-visible child shows all reason-visible siblings, with matching group/action counts. |
| Expansion/polling | Keyboard expansion works, survives refresh, and never changes outer page totals. |
| Pagination | A show's children are not split across outer pages; shrinking results clamp the page. |
| Group action | Searches the complete show selection, including hidden child pages and wanted copies. |
| Individual episode | Searches only that episode and copy; no sibling/season/other-copy grabs. |
| Missing-only action | Includes Missing on every page and excludes upgrades in mixed-reason shows. |
| Upgrade-only action | A two-case Missing/Upgrade table proves exact reason membership on every page. |
| Filtered group | Searches only the intersection of that group and selected reason. |
| Search all now | Ignores current reason/page; processes all selected targets beyond the first 20. |
| Pack/multi-episode release | Always rejected in exact-target runs, even if every sibling is selected; no pack grab. A pack-only early query does not suppress a later single-episode hit. |
| State changes after enqueue | Newly satisfied, unmonitored, deleted, reason-changed, and in-flight targets skip accurately. |
| Copy-aware in-flight state | A primary season pack suppresses primary episodes, not the additional copy. |
| Regrab cap | Three failed grabs within twelve hours skip as `regrab_capped`; fewer/older failures do not. Failed packs count only toward the same copy's episodes, for primary and secondary copies. |
| Target refresh cost | Per-target revalidation reads that item's state; it never calls a whole-library wanted rebuild. |
| Empty/error outcomes | Zero selection, no indexers, request failure, partial target errors, and no candidates have distinct feedback. |
| Double click/overlap | Identical submissions reuse a run; distinct active submissions report the conflict; no duplicate grabs. |
| Automatic overlap | Deterministic interleavings of Wanted, RSS, backlog, and item/failure search share reservations and recheck in-flight state; season and episode targets conflict within the same copy. |
| Chunk fairness/pacing | Each job handles at most one target; waiting background jobs can execute between chunks; continuation honors the persisted delay without occupying a worker. |
| Continuation dedupe | Next chunk is enqueued after predecessor retirement, not discarded against its still-leased dedupe key. |
| Target/job failures | A failed search advances the cursor with a failed result; only infrastructure failures consume job attempts. Exhaustion interrupts the run with the job error. |
| Restart and lost events | Recovery covers first-job and between-chunk gaps, dropped done/failed events, existing leases, and stale events; committed targets do not replay or skip pacing. |
| Cancel | Queued, active, reservation-waiting, and between-chunk cancellation prevents later searches; partial results/downloads survive. Repeat/reload/restart and completion races remain consistent. |
| Retention | Runs/results use the existing hourly cleanup and retention value; nondefault retention is honored and active runs survive. |
| Compatibility | Existing flat Wanted readers and library-item autosearch still pass their contracts. |

## 6. Build milestones

### 6.1 Reconcile the implementation branch and define selection

Start from `8ce3590` or a descendant with PRs #27 and #28. Preserve its text
search, two reasons, sorting, page sizes, and pager with §1 semantics. Add
structured fields, remove ID-prefix metadata parsing, and add one
authoritative scope selector.
Document any necessary name adjustments in this plan. Review
[OpenAPI](../internal/api/openapi.yaml),
[API handlers](../internal/api/acquisition_handlers.go),
[web API types](../web/src/api.ts), and the existing acquisition Wanted tests.

**Acceptance:** scope-selector tests prove all/group/reason/target membership
and copy identity, including mixed-reason shows and stale target handling.

```bash
go test ./internal/app/acquisition/... ./internal/api/... -count=1
```

### 6.2 Add exact-target execution and resumable bulk runs

Implement §3 with acquisition tests, queue wiring, storage migration/queries,
OpenAPI schemas/handlers, generated code, run progress, and per-target reports.
Include cancellation, the shared reservation, exact-candidate predicate,
copy-aware regrab coverage, and the run coordinator's continuation/recovery.
Test lifecycle and scope before connecting UI controls.

**Acceptance:** a run with more than 20 targets drains fully; Missing-only and
one-episode runs never broaden scope; cancellation, chunk fairness, and
failure/restart recovery pass the deterministic cases in §5.

```bash
make gen
go test ./internal/app/acquisition/... ./internal/api/... ./internal/infra/jobs/... ./internal/infra/sqlite/... -count=3 -shuffle=on
```

### 6.3 Build the grouped page and visible reason actions

Update the Wanted page, API client, styles, and result formatting. Extract
pure grouping/filtering helpers for unit tests. Reuse existing pager and
query conventions. Add Playwright coverage for the concrete interactions in
§5, asserting request bodies as well as visible labels.

**Acceptance:** the grouped list, all four scopes, reason counts, expansion,
pager, progress/results, and reload reconnection work on desktop and narrow
viewports. No search depends on which page or groups are open.

```bash
make test-web
npm --prefix web run typecheck
make test-e2e
```

### 6.4 Update shipped documentation and verify the final tree

Edit PR #28's Wanted pagination paragraph in [Usage](usage.md), adding
group/child/reason search, all-page semantics, cancellation, pacing, and the
query-cost/season-pack trade-off. Update this plan's status and bump
[VERSION](../VERSION) when behavior ships.
Preserve unrelated working-tree edits. Follow [repository rules](../CLAUDE.md)
and the target toolchain in [go.mod](../go.mod) and [Makefile](../Makefile).

**Acceptance:** §5 is covered by passing tests or recorded UI checks, generated
contracts are fresh, and the required delivery gates pass on the final code.

```bash
make lint
make test
make test-web
make test-e2e
make test-mobile  # verify unchanged mobile consumers of shared contracts
```

Run applicable CI race/coverage checks as required by the implementation
branch. Report exact unrun or failed commands; do not present an incomplete
gate as green. This document is a plan only: implementation and runtime
verification remain for Sol.

## 7. Fable review disposition

Fable's verdict was **APPROVE WITH CHANGES**, based on source inspection of
`8ce3590`, without executing tests. This revision incorporates M1–M4 and
S1–S7; the details below record two corrections to the suggested mechanics
and one bounded prerequisite rather than copying them blindly.

| Review | Resolution |
|---|---|
| Baseline and nits | Pin the build baseline, client pagination, two reasons, controls, structured kind replacement, timestamps/label, numeric sort, and PR #28 Usage paragraph. |
| M1: cancellation | Cancel API/control, durable request, drained terminal state, explicit races and count semantics (§3.2–§3.3). |
| M2: coverage leakage | Mandatory pack/multi-episode rejection in both early-stop eligibility and final selection (§3.1). No in-scope-coverage exception. |
| M3: queue occupancy | One-target chunks, normal priority, durable cursor, delayed continuation. Same-key enqueue happens **after** job completion because leased-job dedupe blocks self-enqueue (§3.2). |
| M4: interruption owner | Coordinator owns failed-job events and startup/periodic reconciliation. Durable job state covers lost events and enqueue gaps (§3.2). |
| S1–S3 | Defined group sorting/text matching, item-local refresh, indexer cost, per-run pacing, and item-page pack guidance (§1, §3). |
| S4 | Honor the cap and expose the skip. Include a narrow copy-aware `wantableOnRow` correction so the promised guard is not knowingly wrong for copies (§3.1). |
| S5 | Use the existing retention setting and hourly tick (§3.2). |
| S6 | Shared reservations include all automatic callers. Use item/copy scope because a bare wantable-ID set does not make season and episode searches conflict (§3.1). |
| S7 | Extend the OpenAPI skip enum, regenerate types, and update formatting (§3.2). |

Plan-only validation: relative links and whitespace checked. The local source
check supports the queue/dedupe/cap refinements; PR #27/#28 facts remain
attributed to Fable's supplied review until Sol verifies the build baseline.
