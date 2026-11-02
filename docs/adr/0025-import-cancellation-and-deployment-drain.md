# ADR 0025: Durable import cancellation and deployment drain

- **Status:** concrete design reviewed by Astra; implementation in progress
- **Date:** 2026-11-01
- **Baseline:** `ecee8fe` (merged lifecycle PR #78)
- **Related:** [ADR 0021](0021-file-lifecycle-recovery.md),
  [implementation contract](../NFS_SHUTDOWN_IMPLEMENTATION.md)

## Context

NFS can block a canceled writer in writeback or close. Cancellation must record
intent promptly without acknowledging quiescence until that writer returns.
Deployments must pause admission, finish admitted work while status stays live,
then verify actual old-container exit before replacement. The user selected
in-process storage with measured grace, accepting restart blockage during NAS
outages. A finite outage deadline is not a requirement or a supported guarantee.

## Decision

### 1. Cancellation uses an import epoch and per-file progress

Migration 0063 adds three tables, independently of rewritten handoff JSON:

- `import_attempt_controls`: primary key `(download_id,generation_key)`, retained
  nonnegative `attempt_epoch`, outcome `''|pending|cancelled|completed`, nullable
  request/finalization milliseconds, and a `plan_sealed` boolean. Outcome checks
  require request/finalization times for cancelled, a request without finalization
  for pending, and a finalization time for completed. No request is required for
  normal completion.
- `import_attempt_files`: key `(download_id,generation_key,attempt_epoch,source)`;
  outcome `pending|committed|skipped`, optional committed placement ID. The exact
  media/potential-sidecar selection is sealed before first publication. Sidecars
  participate through their existing delivery ledger and immutable authorization;
  their ledger, authorization and per-file result commit together. Automatic retries must
  match it; an explicit retry creates a new epoch. Per-file rows distinguish a
  completed multi-file selection from one canceled between files, including
  after process restart. Existing selection/refusal and delivery-sidecar rules
  remain authoritative; a skip is a recorded policy result, not publication.
- `import_publication_authorizations`: key `(download_id,generation_key,
  attempt_epoch,publication_id)`, optional media placement ID, source/target and
  immutable identity payload, authorization milliseconds, outcome
  `pending|committed|reconciled`. Publication IDs distinguish media and sidecars. New prepared placement/stage identity includes the epoch. Prepared JSON epoch
  is immutable under prepare/update/commit CAS; prior committed files are adopted
  only through current-generation source/delivery receipt proof. Authorization and cancellation share SQLite W serialization.

Download foreign keys cascade only after a BEFORE DELETE trigger rejects pending
or effective cancelled controls and unresolved authorizations. Thus ordinary
terminal pruning retains its existing custody predicates; Activity cannot erase
effective suppression. Bulk prune/delete/dismiss statements explicitly exclude protected rows, avoiding
a trigger abort of unrelated terminal rows. Completed/never-cancelled controls can disappear with an
otherwise-deletable download. No independently reusable row/epoch is deleted by
retry. Migration Down refuses effective cancellation or unresolved authorization
before dropping additive state, following migration 0037's guard pattern.

The concrete DB API is `EnsureImportAttempt`, `BeginImportRetry`,
`RequestImportCancellation`, `FinalizeImportCancellation`, `SealImportAttempt`,
`ResolveImportFile`, `AuthorizeImportPublication`, and `CompleteImportAttempt`.
Every mutation rechecks the current download acquisition key and epoch on W.
`CommitPlacement` commits its matching authorization/file result with metadata.
Final cancellation reads fresh handoff JSON, appends `import_cancelled`, returns
the download to `downloaded`, and finalizes the control in the same transaction.
It refuses unresolved authorizations. Completion reconciles fully resolved
selections with committed files before acknowledging cancellation.

All ordinary/delivery enqueue and inline paths attach/recheck the attempt key.
Tickets cannot finalize a newer epoch. The canonical per-download mutex gets a
shared accessor and TryLock path; no second lock map is added. Cancel captures
key/epoch, signals only the matching ticket, persists guarded intent, then returns
202 if ownership is still held or 204 after transactional finalization under the
mutex. Completed or stale attempts return 409 with a specific outcome. An optional
client epoch guard fences a delayed request; Activity always sends its displayed
epoch. Calls without the guard address the current attempt at server observation.

Publication authorization occurs after staging/validation, before rename, with
no SQLite transaction or cancellation mutex spanning NFS. Once authorized, that
placement finishes/reconciles on an uncanceled context while retaining worker
ownership. A later cancellation prevents subsequent authorizations. Uncertain
publication errors retain pending authorization for reconciliation/review; they
do not permit cancellation finalization or epoch advance. Reconciliation can
commit already-published matching bytes; it does not republish an old intent.
Committed files remain recorded after partial cancellation. Cleanup authority
for uncommitted files is not granted by cancellation.

Uncertain authorization exposes `review_required`. An explicit authenticated
`retain_uncertain` action requires the displayed epoch and acknowledgement that
external writers were fenced. It TryLocks both canonical download and import
publication mutexes, then rechecks epoch and pending authority on W. One
transaction irrevocably revokes those authorizations and finalizes cancellation.
It retains all prepared/source/stage custody, makes no filesystem calls and does
not assert that publication never occurred. Revoked authorization cannot later
commit or reconcile. Retained custody blocks publication to its target across
new epochs until explicit Library review/adoption/disposition resolves that
restriction; source cleanup remains separately fenced. New epochs get fresh
stage names, not a retry loop on the retained stage. External fencing is an
operator attestation, never measured proof of cross-host recovery.

Only deterministic policy refusals resolve a file as skipped. Cancellation,
stale epoch, authorization and DB failures remain pending and propagate as typed
control errors. Current target generation and source/delivery identity must be
proved before an earlier committed receipt satisfies a new epoch.

Recovery imports with no download retain their existing durable recovery
cancellation protocol. They use the same placement and maintenance boundaries;
the ordinary-download control foreign key does not replace recovery claims.

### 2. One reversible admission coordinator owns maintenance

The coordinator has accepting, draining, drained and terminal phases. Admission
and active-registration use one mutex. A token is held through real completion,
including descriptor close and local finalization. Context carries the current
live token: an admitted parent may register a child during drain, but an ended
token cannot create work after drain reached zero. Detached children acquire
their own tokens before their parent returns. Durable queue tickets do not
inherit a short-lived caller token; they acquire execution admission separately.

`TryEnter(ctx,kind)` rejects new root work while paused. `Enter(ctx,kind)` waits
for resume for worker/timer loops without counting an idle waiter as active.
Release wakes status/waiters; it does not reopen admission. Terminal Close never
resumes. Abort validates a process-unique generation and a random owner token;
an old owner cannot reopen a later drain or terminal shutdown.

Maintenance controls follow existing native API authentication. POST accepts a
caller-generated 256-bit owner token in the body, making a lost response safely
repeatable; GET exposes phase/generation/work descriptions and elapsed time but
never the token; DELETE requires the matching token and generation. Repeated POST
by the same owner is idempotent, another owner gets 409. All controls are exempt
from ordinary admission and remain prompt while storage work blocks.

Native/compat requests are classified by actual operations. Unknown operations
are admitted as mutations; only an explicit status/read-only inventory is exempt.
SSE/status stay available. GETs that persist previews/search state are mutations.
Rejected actions return visible 503 maintenance errors. Background searches own
detached tokens. Timer tasks wait before execution; accepted in-memory manual
triggers retain a child token until execution. Job claims acquire before lease
mutation and retain through finish/heartbeat join. Reaper/prune iterations are
also covered. Import/cleanup workers wait after dequeue before I/O, preserving
unstarted durable work. Acquisition retry/recovery/replacement/preview iterations
and subscriber event mutations acquire at their real entry points. Operational
health/notification telemetry is not media-publication admission and remains
joined by terminal shutdown; status does not claim that every DB write stops.

### 3. Deployment binds drain and exit to one container

The deployment command captures a container ID and a fresh owner token, requests
drain, polls status, and only after drained sends TERM. Operator timeouts report
pending/failure and retain the old process; no force removal or replacement is
allowed. It waits for `docker wait` and verifies stopped state/PID for that exact
ID before Compose replacement. Resume/abort is explicit and owner/generation
guarded. It never automatically resumes a timed-out drain, because admitted NAS
work may still be blocked. Stop/grace inputs are explicit measured deployment
parameters; no invented 90-second production default is added.

## Alternatives and consequences

Storing cancellation in handoff JSON loses intent to stale worker writes.
Cancel-and-Wait HTTP handlers block behind NFS. Counting imports without closing
admission races new work. Terminal lifecycle cancellation cannot implement
reversible maintenance. The selected design adds durable state and narrow shared
admission in exchange for explicit pending outcomes and retained custody.

## Verification

Tests cover epoch ABA, stale tickets, queued and running cancellation, SQLite
failure, restart suppression, publication/cancellation ordering and multi-file
partial completion; migration guards and pruning; admission/child/drain races,
resume and stale abort, API/compat/background/job/worker coverage; and exact-ID
deployment sequencing. NFS qualification records matched four-policy trials,
large-file final-sync/kill tails and deployment-shape limits separately. Automatic
cross-host recovery is not enabled on basic advisory-lock evidence.
