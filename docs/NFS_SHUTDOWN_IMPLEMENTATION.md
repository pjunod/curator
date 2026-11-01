# NFS shutdown implementation — lifecycle fixes and concrete next contracts

**Status:** lifecycle code awaiting final review/gate; cancellation and maintenance contracts awaiting focused design review · **Written:** 2026-11-01

Companion to [implementation status](NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md) and
[deployment](deployment.md). This contract separates the implemented lifecycle
batch from the remaining cancellation, maintenance and storage work. Progress,
review findings, PR, validation and merge state live on the status page.

## 1. Implemented lifecycle batch preserves dependency ownership

The first code batch changes [application startup](../cmd/monarr/main.go),
[API lifecycle](../internal/api/lifecycle.go),
[SSE](../internal/api/sse.go), and
[background search](../internal/api/library_handlers.go).

- `run` owns an internal cancelable context. Every worker-wait defer cancels
  it first; listener failure and partial startup exit no longer require an
  external SIGTERM to release workers. The final goodbye follows owned cleanup.
- `Server.BeginShutdown()` atomically closes request/background admission,
  cancels event streams and background searches. Ordinary admitted handlers
  retain their HTTP grace. `CancelRequests()` is the second stage after that
  grace expires; `Wait()` joins actual handlers and background completion before
  queue/encoder/bus/database teardown.
- Event streams set a five-second write deadline before initial/event/heartbeat
  writes, check write/flush errors, and end on cancellation or failure. Logging
  middleware preserves ResponseController deadline/flush capabilities. Deadlines
  clear after a successful frame/flush so idle HTTP/2 streams survive; normal
  exit arms a deadline for the final response flush without extending a failed
  write deadline. Unsupported adapters fail explicitly.
- Registered notification, subscription, coordination and event loops are
  joined before their dependency cleanup. The subscriber supervisor joins its
  children; a wanted-search cancellation watcher joins before its owner returns.
- Background search retains its nondurable five-minute operation semantics;
  admission and WaitGroup registration share a mutex, work uses application
  cancellation, and shutdown joins its actual cleanup.

Regression coverage uses real HTTP connections, an occupied listener without
an external signal, a responsive stream, a non-reading client under actual
socket backpressure, unsupported controller capabilities, and blocked handler/
background cleanup. No unit test can establish interruption of hard NFS.

## 2. Cancellation needs a generation-scoped durable control row

This section is a proposed concrete design, not implemented code. Read the
current [import workers](../internal/app/acquisition/importers.go),
[manual retry](../internal/app/acquisition/handoff.go), and
[SQLite handoff writes](../internal/infra/sqlite/acquisition.go) before edits.
The current `advance` rewrites a stale in-memory handoff JSON; cancellation
suppression must not exist only in that field.

Use one additive table with an explicit import-attempt epoch independent of
Runner's acquisition `generation_key`. Manual import retry currently retains
that acquisition key; changing it would break delivery/receipt identity.
The epoch prevents a late cancellation from targeting a newly retried attempt.

```sql
CREATE TABLE import_attempt_controls (
    download_id INTEGER NOT NULL REFERENCES downloads(id) ON DELETE RESTRICT,
    generation_key TEXT NOT NULL,
    attempt_epoch INTEGER NOT NULL DEFAULT 0 CHECK (attempt_epoch >= 0),
    requested_at INTEGER,
    finalized_at INTEGER,
    outcome TEXT NOT NULL DEFAULT ''
        CHECK (outcome IN ('', 'pending', 'cancelled', 'completed')),
    PRIMARY KEY (download_id, generation_key)
);
```

Store times in UTC Unix milliseconds. The control row exists before admission,
including before any cancellation, so an explicit retry increments a retained
monotonic epoch rather than deleting and recreating its only fence. Requested
and finalized fields must be consistent with outcome; encode that invariant
in the migration and transactional methods. Terminal-row retention/pruning must
preserve effective suppression and not break existing activity removal.

**Admission and retry.** Every automatic, manual, delivery and recovery import
reads/initializes the control row while holding the canonical per-download
mutex, attaches its acquisition key/epoch to its ticket, and rejects pending/
cancelled suppression. Automatic retry keeps the epoch. Explicit ImportNow
requires previous writer quiescence, increments epoch and clears suppression
in a guarded local transaction before enqueue. Old canceled tickets retain
old epoch/context and cannot modify or finalize the new attempt. Queued work
remains durable when the channel disappears.

**Cancellation.** Capture key/epoch, signal only its matching ticket, and
TryLock the same canonical mutex (no second per-ID lock map). If held, a guarded
write on SQLite W checks current download acquisition key, attempt epoch and
terminal outcome before changing outcome to pending. Return 202 with
`status: cancel_requested` only after persistence. If lock acquired, re-read,
prove quiescence and finalize transactionally before 204. Completion that won
returns an already-completed outcome, not a stale accepted cancellation. Repeat
requests reconcile pending, cancelled or completed state idempotently.

Worker finalization uses a bounded, tracked local context independent of canceled
copy work. In one SQLite transaction it reads fresh handoff/state, records
`stepImportCancelled`, and marks the same epoch cancelled. It does not call
void `advance` and assume its write succeeded. Keep suppression until explicit
retry; admission cannot restart a downloaded row merely because finalization
finished. If publication already committed, reconcile completed instead of
pretending it was rolled back. Table predicates fence old worker completion.
A DB failure is an error and leaves prior durable intent for restart; no live
worker ownership is released merely because a timeout fired.

**Publication ordering uses a per-placement authorization record.** A multi-file
attempt cannot be represented by one global authorization boolean. Proposed
second additive table:

```sql
CREATE TABLE import_publication_authorizations (
    download_id INTEGER NOT NULL REFERENCES downloads(id) ON DELETE RESTRICT,
    generation_key TEXT NOT NULL,
    attempt_epoch INTEGER NOT NULL,
    placement_id TEXT NOT NULL REFERENCES import_placements(id) ON DELETE RESTRICT,
    authorized_at INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('pending', 'committed', 'reconciled')),
    PRIMARY KEY (download_id, generation_key, attempt_epoch, placement_id)
);
```

Before each filesystem publication, a short SQLite W transaction verifies the
current acquisition key/epoch, no effective cancellation, source/target authority,
and prepared placement; then inserts its pending authorization. Cancellation
insertion and authorization share SQLite's serialized writer boundary. Do not
hold an SQLite transaction or prompt-cancellation lock across rename, directory
sync or NFS calls. Placement ownership still spans that operation.

Cancellation that wins first prevents new authorization. Cancellation arriving
after authorization may persist pending intent, but the authorized file must
complete/reconcile before final cancellation. The metadata/placement commit
marks that authorization committed in the same transaction. Crash recovery
resolves any pending authorization before cancellation finalization or explicit
retry. Finalization cannot infer "not published" merely because download state
is still importing.

If all selected files committed, completion wins. If cancellation prevented
later files, retain committed file receipts, mark the remaining attempt cancelled,
and keep suppression; do not undo published files or authorize source cleanup
for uncommitted files. Retry uses a new epoch and the existing per-file ledger
to avoid duplicating already-committed work. It cannot advance an epoch while
an older authorization is unresolved. Preserve source cleanup/receipt authority
and test cancellation before authorization, after authorization, after rename,
after directory sync, before DB commit, and between files. Exact SQLite methods
and recovery/pruning compatibility require focused review before migration.

Add OpenAPI 202 plus the existing finalized 204 and an explicit completed/stale
response. Activity shows Cancelling while pending and offers explicit retry
only when execution is actually stopped. Test all admission paths, stale ticket,
completion-before-intent, intent-before-completion, explicit-retry ABA, queued
cancellation, shutdown, DB failure, and restored-state behavior.

Local DB restart preserves intent; asynchronous backup restoration can lose
it. An old binary ignores the new table: downgrade must resolve controls or
explicitly accept loss of cancellation semantics. Define migration Down refusal
for effective records, source compatibility and pruning before implementation.

## 3. Maintenance drain is an admission contract, not a status poll

The selected deployment workflow keeps read-only/status API access available
while storage mutations finish, then stops and waits for actual exit. It needs
a narrow shared admission coordinator, not a new global startup state machine.
This maintenance barrier is reversible and separate from terminal
`Server.BeginShutdown`: maintenance pauses new work but does not cancel admitted
searches, close read-only/status admission, or cancel reusable contexts. Abort
reopens only the same maintenance generation; after final process shutdown it
cannot reopen service. A lifecycle closing flag or canceled background context
cannot be reused to implement resume.
Proposed control surface, to review before source changes:

```text
POST /api/v1/system/deployment-drain       → owner token + generation + status
GET  /api/v1/system/deployment-drain       → phase, active work, elapsed, errors
DELETE /api/v1/system/deployment-drain     → explicit owner/generation-guarded abort
```

Apply existing administrative auth; absence of API-key configuration must
follow current application auth policy rather than invent an unauthenticated
backdoor. The exact owner-token transport and OpenAPI/UI/CLI contracts belong
to this feature and require visible status/error feedback.

The admission coordinator atomically changes accepting/draining state and
registers admitted mutation work. The following inventory must be covered on
the chosen implementation revision; configured worker count is not actual work:

| Participant | Entry and completion boundary |
|---|---|
| HTTP mutations and compat shims | Acquire before side effects, retain through synchronous mutation and registered children; do not infer read-only solely from HTTP verb |
| Detached background searches | Existing lifecycle admission closes; join existing searches; no late grab while drained |
| Scheduler producers | Pause new task work; let registered in-flight producers finish; preserve periodic schedule semantics on abort |
| Durable job workers | Pause claims before mutating lease/attempt state; finish active handlers and final state writes; queued jobs stay durable |
| Import and separate cleanup workers | Pause execution admission after dequeue but before filesystem work; leave unstarted tickets recoverable; retain active ownership through close/finalization |
| Recovery/manual/automatic/replacement/cleanup-retry loops | Cover each real importWG producer's iteration/mutation path; idle loops do not count as active mutations |
| Inline/manual import and placement | Same admission and per-download/target fences as asynchronous work; no untracked filesystem writers |

A parent cannot launch an untracked child after drain observes zero. Pending
queued work is not counted as a running mutation and must survive stop/abort
without losing a claim or leaking an in-memory ticket. No WaitGroup.Add may
race a completed drain. A drain command must not wait on its own registration.
Read-only NAS metadata may still stall and must be tracked for eventual HTTP
shutdown, even though it does not authorize publication.

The deploy tool captures the old container ID, requests drain, polls status
with a bounded operator timeout and visible diagnostics, then SIGTERMs it with
selected per-shape grace. It waits for actual exit (`docker wait` plus state
verification) before Compose replacement. An expired stop/drain timeout or
zombie error does not permit rm -f or a new writer. Explicit abort may reopen
admission only for the same maintenance owner/generation. Preserve the old
process if its storage cannot finish.

Acceptance includes admission/enqueue/dequeue/drain races, abort/repeat/stale
owner calls, active and queued jobs/imports, a blocked close, background grabs,
HTTP/compat mutations, restored state, and actual-exit deployment sequencing.
Include a successful new search after maintenance abort and a stale abort after
terminal shutdown; neither may confuse reversible pause with permanent close.
Compose and Kubernetes need distinct memory/writeback/grace measurements; no
90-second default is selected by this implementation contract.

## 4. Automatic storage recovery and writeback remain qualified separately

Do not implement cross-node automatic stage deletion from a filename, inode,
or basic advisory lock result. An OFD design needs stable namespace coordination
across unlink/recreate, post-lock descriptor/path validation, guard lifetime
through publication, old-version exclusion, and isolated lease/delegation/
partition qualification. E6's nominally nonblocking call took 35.142 seconds;
no prompt API handler may perform that operation directly. Retained-review
resolution must avoid a permanent deterministic-path wedge without deleting
an uncertain object. No stage-recovery code is included in the lifecycle batch.

Writeback selection requires matched whole-file, periodic, concurrent-commit
and Linux-range-writeback experiments, with large-file final-sync and killed-
worker tails. Measurements and design reviews are not evidence that source
changes passed the gate, nor that NFS became interruptible.
