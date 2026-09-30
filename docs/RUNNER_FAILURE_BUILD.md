# Download failure build — implementation handoff for Runner and Monarr

**Status:** ready for implementation · **Written:** 2026-09-30

Implements the [reviewed fix plan](RUNNER_FAILURE_FIX_PLAN.md), retaining all
nine dispositions in the [adversarial review](RUNNER_FAILURE_REVIEW.md).
The [diagnosis](RUNNER_FAILURE_DIAGNOSIS.md) distinguishes live observations
from checkout reproductions. Read those documents before editing, then execute
this document in order. This is a build authorization; production deployment
and mutation of the retained media backlog are separate work.

## 1. Deliverable and working boundaries

Build and verify the fixes across Runner and Monarr. Continue through the
phases without stopping after a plan, reproduction, or first patch. Deliver
working files, migrations, regression tests, updated behavior documentation,
and an evidence-backed build status. Preserve release and data-ownership
contracts. If a design assumption cannot hold, record the concrete conflict,
keep the affected operation held, and continue independent implementation.

| Workspace | Purpose and starting caveat |
|---|---|
| `/Users/pjunod/code/nzbd` | Runner implementation; local HEAD inspected as `f959cf922b47c581713df9401003c7c6f96cf5f7`, older than the diagnosed checkout |
| `/Users/pjunod/code/monarr` | Curator implementation and this handoff; contains the earlier failure/inventory patch plus unrelated concurrent work |
| `/private/tmp/runner-diagnosis-ab25d0c` | Read-only investigation snapshot of host checkout `ab25d0cbecfdb5adf299072813a2948383192d9f`; not a delivery checkout |
| `/private/tmp/runner-regression-probes` | Four diagnostic reproductions; port into Runner's locked test suite |

Read applicable `AGENTS.md`/`CLAUDE.md` in both repositories and their parents.
Use the parent `/Users/pjunod/code` project for this session so both repositories
are in scope, but limit edits to these two projects. Do not modify unrelated
repositories, discard local changes, reset the checkout, sweep untracked files
into commits, or replace another session's generated files.

Monarr's `CLAUDE.md` requires delivery in its working tree, full gates, repeated
affected-package tests, target toolchains, explicit commit paths, and version
and behavior-document updates. The earlier patch was verified as `0.34.4`;
the shared tree has since advanced. Read `VERSION` immediately before changing
it and choose the next appropriate version without reverting other work.

**Out of scope:** deploy/restart live services, submit new media downloads,
rewrite live databases, resume/pause existing live imports, bulk rename old
media, delete staging, and clear blocklists. Build dry-run/recovery mechanisms
and test them on synthetic data. A failure to obtain actual-mount evidence
leaves that release gate pending; it does not justify a speculative unsafe
fallback or stopping all other work.

## 2. B0 — establish a usable baseline and preserve regressions

Record HEAD, dirty tracked paths, untracked paths, toolchains, and source
versions in a new `docs/RUNNER_FAILURE_BUILD_STATUS.md` in Monarr. Keep that
file as the build ledger: phase, implementation state, commands/results,
remaining blockers, and exact revisions tested. Exclude secrets and raw URLs
with credentials.

```bash
git -C /Users/pjunod/code/nzbd status --short
git -C /Users/pjunod/code/nzbd log -1 --format='%H %s'
git -C /Users/pjunod/code/monarr status --short
git -C /Users/pjunod/code/monarr log -1 --format='%H %s'
```

Safely fetch Runner's current upstream and compare its ancestry and fixes with
the diagnosed checkout. Do not implement a second obsolete version of code
already fixed upstream. Advance the local Runner checkout only if existing
work can be preserved. Its modified README and untracked lifecycle documents
may collide with newer tracked files: preserve and compare them explicitly,
never force checkout or silently replace them. If a separate ordinary Runner
checkout is needed, record its absolute path and delivery method; do not use
the temporary diagnostic snapshot as the product source. Monarr stays in its
existing working tree as required by its repository rules.

Create small, deterministic repository fixtures for the four existing cases:

- Bracketed, unquoted NZB subject becomes a subject-shaped media filename.
- Real PAR set refers to a nested file whose actual bytes are flat.
- Missing first extractor and Linux disk-full message from fallback.
- First extractor fails generically and masks fallback disk-full diagnosis.

Also preserve failing cases for write/finalize exhaustion, two archive outputs
sharing a parent, repeated reviewed relocation, and consumer regrab after a
local resource failure. Use injected filesystem/extractor faults instead of
filling a real volume. Do not hardcode the diagnostic machine's `par2` path;
use repository tool discovery and make missing required tools fail in the
strict test lane.

**Acceptance:** baseline provenance is recorded; each claimed defect has a
meaningful old-behavior failure or is explicitly marked already fixed by a
verified upstream regression. Fixture dependencies use Runner's lockfile.

## 3. Interfaces to preserve and extend

These existing interfaces were checked on 2026-09-30. Re-verify at the chosen
implementation revision before editing. New fields below are a proposed build
contract; reconcile names with any equivalent upstream work while retaining
the semantics, and record the final wire schema in both integration docs.

### 3.1 Existing seams

| Area | Existing interface/location | Required consequence |
|---|---|---|
| Job lifecycle | `JobStatus` in Runner `crates/nzbd-types/src/lib.rs`: `Queued`, `Downloading`, `Paused`, `Fetching`, `PostQueued`, `Post`, `Completed`, `Failed`, `Deleted` | Holds must not become failed jobs merely to carry an error |
| Post-processing terminal marker | `PP_DONE_PARAM = "*PP:done"` in the same file | Never write this terminal marker for resumable resource holds |
| Queue API | `GET /api/v1/jobs`; Monarr fields `status`, `pp_done`, `ready`, `stages` | Preserve old clients; add facts rather than changing known strings to unknown enums |
| Resume action | `POST /api/v1/jobs/{id}/actions/resume` in Runner API | Preserve manual pause semantics; resume must honor resource admission and other holds |
| Events | `/api/v1/events`, including queue and PP events | Emit persisted facts; SSE sequence is not a durable job-state revision |
| Writer | `WriteCmd::Segment { seg_number, offset, data, crc, file_size, server }`; `Finalize { file_size, combined_crc }` in `nzbd-engine/src/writer.rs` | Add metadata/checkpoint control without allowing worker tasks to change authoritative names |
| Relocation | `Inventory::relocate(&self, job: u32, destination: &Path) -> Result<()>` | Fallback publication needs a returned actual artifact/path; update every caller |
| Artifacts | `artifacts(id,job,path,state,updated_at,data)` and `operations(id,artifact,state,data)` in `nzbd-state/src/artifacts/mod.rs` | Migrate existing journaled operations; do not create a disconnected second owner |
| Consumer lifecycle | `DownloadStatus`/`DownloadState` in [downloadclient.go](../internal/ports/downloadclient.go) | `Blameless` is not retry suppression; `StateRemoved` keeps its existing semantics |
| Consumer active rows | [acquisition query](../internal/infra/sqlite/queries/acquisition.sql) includes grabbed/downloading/downloaded/awaiting_import/importing | Held acquisitions must remain in the active set and survive restart |

### 3.2 Proposed additive control facts

Use one versioned control object in authoritative job snapshots and relevant
events. Encode a monotonic revision as a decimal string to avoid JavaScript
integer precision loss. For example, this is a proposed held-job response:

```json
{
  "status": "paused",
  "pp_done": false,
  "ready": false,
  "control": {
    "version": 1,
    "revision": "42",
    "lifecycle": "held",
    "cause": "capacity",
    "stage": "download_write",
    "retry_policy": "resume_same_job",
    "message": "Waiting for storage capacity"
  }
}
```

Freeze exact serialization with shared golden fixtures before building UI.
Lifecycle values are running/held/succeeded/failed; causes and stages follow
fix-plan §4.1. Existing terminal/history semantics remain authoritative for
legacy clients. Monarr already maps `paused` to `StateQueued`; verify Runner's
other shipped clients also retain it nonterminal before choosing this projection.
An open PP stage can still supply detail, but cannot override the control hold.

Persist each transition and its revision before event emission. Job identity
includes the configured client identity and stable transfer/job association;
do not compare revision numbers across different clients or reused job IDs.
Preserve `monarr-transfer`. Loss/restoration of the authoritative store must
not make an older revision overwrite newer custody decisions: require explicit
reconciliation of the instance/transfer identity if continuity is lost.

Absent control fields mean legacy behavior, not permission to clear an existing
hold. A newer explicit running snapshot clears only the corresponding resolved
resource hold. Unknown schema/cause stays reviewable and nonterminal when
terminality is unproven. Duplicate or stale revisions have no side effects.
History and completion events must not advertise failure for held work, nor
success merely because a retained copy was faithfully relocated.

### 3.3 Consumer persistence and atomic acquisition suppression

Prefer keeping held transfers in the existing active acquisition lifecycle
with separately persisted control/revision facts. This avoids relying on every
existing query recognizing a new SQL state. If introducing a held state instead,
audit and update every active/terminal query, sweep, UI count, and scheduler.

Persist control facts and the acquisition transition together. Allocate the
next migration number from the actual tree at build time; concurrent work
means a number chosen in this document would be unsafe. Update SQL sources
and regenerate repository bindings rather than editing generated Go by hand.

The final admission check must see the held transfer under the same locking or
transaction discipline used to prevent concurrent grabs. Cover individual
episodes and season-pack wantables, retries, scheduled searches, reconciliation,
and operator actions. A release refresh or late event cannot create a second
acquisition. Historical failed rows get no blanket reclassification.

## 4. B1 — stop failure and retry amplification

### 4.1 Add a durable resource-hold transition

Touch Runner job types, queue owner, snapshot/state persistence, API events,
and the Monarr adapter/acquisition boundary. Keep resource holds distinct from
user pauses, scheduler pauses, recovery claims, and identity-review holds.
Resolving capacity must never release another hold or a user pause.

Transition order is: recognize cause; fence scheduling for affected operations;
record job/stage/cause/checkpoint and increment revision; persist; expose the
nonterminal snapshot/event. On restart restore fences before scheduling jobs.
A rejected persistence operation cannot be followed by a success/failure event
claiming the transition committed.

Prevent starting further downloads, recovery PAR blocks, fallback extraction,
or relocation copies when their destination's resource hold applies. Existing
work acknowledges quiescence before custody can mutate. A global pause flag
alone is insufficient because jobs and holds must remain attributable.

### 4.2 Preserve extractor errors before fallback or repair

In `nzbd-post/src/tools.rs`, retain bounded outcomes from all attempts, including
which executable ran, exit code, native I/O cause, and redacted diagnostic text.
Recognize POSIX and supported Windows disk-full output plus quota failures.
Prefer structured native causes where available; localization/unknown stderr
must not be mislabeled confidently as a corrupt release.

Classify before fallback and before `manager.rs` invokes repair. A missing
extractor may fall back. Resource exhaustion holds immediately; it does not
invoke another extractor or download parity to solve a filesystem problem.
Do not use `first.or(second)` as a failure-diagnosis selector. Retain the actual
failing attempt and both diagnostics when they disagree.

### 4.3 Stop repeated relocation attempts before copying

Before a cross-volume copy, probe the actual destination's publication
capability with tiny owned entries. Cache by verified mount identity, with
invalidation after remount or contradictory syscall results. Unsupported
exclusive rename yields a structured hold until B4 provides a compatible path.

Create a durable uniqueness claim for source generation + destination purpose
before allocating scratch. The claim must cover running, held, and reviewed
operations; changing the source row revision does not release it. Existing
multiple attempts migrate into a reconciliation group without deleting data.
Reuse a verified eligible staging operation or require review; never allocate
an additional copy just because the previous attempt entered `review`.

**Acceptance:** failure-event/poll ordering tests produce zero extra NZBs,
zero incorrect blocks, and one held transfer. An unsupported-mount fixture
copies zero payload bytes. Repeated calls/restart retain one unresolved attempt
per generation, while legacy duplicates are reported and held.

## 5. B2 — make writes and resource recovery resumable

In `nzbd-engine/src/owner.rs`, `writer.rs`, and persistence, separate fetched,
written, durably checkpointed, and finalized segments. Keep expected file
size and validated ranges/checksums. A preallocated length, network completion,
or failed `Finalize` acknowledgement does not establish durable coverage.

On write, fsync, or publication failure, preserve the partial file and record
the exact failed stage. Do not call the permanent fail-whole-file path for
`ENOSPC`/`EDQUOT`. Drain/cancel queued writes in a defined way and acknowledge
writer quiescence before recovery. Distinguish redoing finalization from
refetching missing segments. Existing snapshots without reliable checkpoints
must be revalidated conservatively; do not upgrade their flags to proof.

Choose and document the checkpoint batching boundary. Data durability must
precede a durable metadata claim that ranges survive restart. Crash tests need
both orderings around every checkpoint. Final publication uses the same
no-overwrite/ownership principles as other file mutation; current name-based
`.part` compatibility needs explicit migration, not a blind rename of writers.

Implement capacity admission and health release from fix-plan §5.3. The build
must include per-filesystem accounting, concurrent reservations, quota context,
bounded health probes, retry backoff, and persisted retry budgets. If required
quota information is unavailable, remain held until explicit operator release;
do not infer quota headroom from `statvfs`.

**Acceptance:** injected ENOSPC/EDQUOT at segment write, sync, and final rename
resumes the same job after valid admission. Only unverified ranges are refetched.
Sparse holes never pass validation. Ample free space with exhausted quota,
restart during a hold, and repeated probe failure produce no retry storm.
Manual pause and unrelated custody holds remain in effect after capacity clears.

## 6. B3 — correct names, PAR layout, and extraction publication

### 6.1 Bind names to stable file IDs

In `nzbd-nzb/src/lib.rs`, support the observed bracketed subject form without
unbounded heuristic extraction. Forward yEnc metadata from engine `pool.rs`
to the owner. Keep partial storage keyed by stable FileId, not mutable names.
Persist the accepted safe basename/provenance and update queue classification
and writer publication together. Preserve extensions when allocating collision
suffixes. No separator, traversal, absolute path, or symlink target is allowed.

Test segments arriving out of order, conflicting names/sizes, restart before
and after confirmation, old subject-shaped partial paths, and a different
existing file at the desired name. Never let a worker rename a live writer.

### 6.2 Extend PAR identity before restoring directory structure

Extend `nzbd-par2` to expose validated packet/set IDs, declared size, full
digest, and prefix digest. Reject malformed or inconsistent packets. Retain
separate recovery sets in `nzbd-post/src/par2.rs`; do not merge unrelated sets.
Use a multi-candidate map for shared prefixes and normalized relative paths
for verification instead of comparing full catalog names with basenames.

Intact full-digest matches may enter a journaled path-restoration operation.
Damaged candidates go to a separate capacity-admitted repair workspace with
originals preserved. Use PAR block validation there to test candidate mappings;
require final size/full-digest verification before publishing repaired output.
Ambiguity or insufficient parity holds without destroying source evidence.

Anchor every traversal to the verified root. Bound depth, entry count, and
metadata parsing. Journal each mapping and reconcile both old/new locations
after crash. Reject unsafe paths and identity changes before mutation.

### 6.3 Keep one namespace through verification and extraction

Refactor recursive discovery consistently across PAR rename/verify/repair,
archive grouping, CRC bookkeeping, extraction, cleanup, and restart. Multipart
groups include parent directory and archive identity. Duplicate basenames in
different episodes are not collisions by themselves.

Replace `commit_staging`'s destination-directory deletion with per-archive
generations and manifest-based file publication. Equal existing bytes may be
reused with identity proof; different bytes require review. Retain archive
inputs until intended output is verified and durably published. Replace name-only
`.pp.*` stale cleanup with operation ownership and quiescence checks.

**Acceptance:** nested intact and repairable multi-episode fixtures reach
verified output. Include corruption after the first 16 KiB, enough/insufficient
parity, multiple sets, shared prefixes, duplicate basenames, malicious paths,
symlinks, and two archives sharing an output parent. Crash injection never
deletes a pre-existing sibling, original volume, or unowned staging directory.

## 7. B4 — implement compatible relocation publication

Keep the capability-proven exclusive-rename path where it works. Implement
the registry-generation fallback in fix-plan §5.4 behind disabled-by-default
enablement until consumers and mappings pass their gate. Do not replace
`RENAME_NOREPLACE` with ordinary rename after an existence check.

Proposed typed result, with names adaptable to existing upstream equivalents:

```rust
pub struct RelocationResult {
    pub operation_id: String,
    pub artifact_id: String,
    pub generation: String,
    pub published_path: std::path::PathBuf,
}
```

Returning this object means custody publication committed, not that media is
valid. Callers propagate the actual path and preserve terminal failure/hold
semantics. Recovery preview may expose a failed published generation; ordinary
automatic import still requires successful media eligibility.

Allocate a unique directory in a dedicated managed root; validate and retain
root/entry identities. Keep its physical path stable. Copy with exclusive file
creation, flush files/directories, verify source and destination manifests,
then transactionally commit registry publication. Only this committed registry
state makes the generation discoverable through permitted APIs. Freeze source
mutation with an ownership lease; a changed identity or hash fails closed.

Audit every reader: Runner discovery/recovery API, Curator automatic imports,
manual-import browser, storage inventory, retention, and cleanup. Exclude the
managed root from direct ordinary scans and expose it through the published
generation contract. Use the existing separate read-only recovery mapping
where applicable. A marker or dot-prefix is not enforcement. If a deployed
consumer cannot map actual paths or obey that boundary, keep fallback disabled
for it while retaining the source under a hold.

On startup, reconcile running and reviewed operations before new scheduling.
Revalidate root/generation identity and durable manifest before readiness or
source retirement. Source retirement is an independently journaled, idempotent
operation subject to all import/recovery holds. Keep source bytes while remote
durability is uncertain; a local DB commit or successful fsync syscall alone
does not prove the storage server's power-loss contract.

**Acceptance:** injected EINVAL/ENOSYS/EOPNOTSUPP/EXDEV, collisions, remounts,
source mutation, and crashes at each publication boundary retain ownership.
Consumers see either a committed generation or a hold, never staging as ready.
Retry reuses the same operation and path. A faithfully copied PAR_FAILURE
remains failed and never autoimports. Actual-mount verification is a separate
mandatory release gate, reported pending when unavailable.

## 8. B5 — complete Monarr behavior and recovery preparation

Preserve the existing changes in [completed.go](../internal/app/acquisition/completed.go),
[acquisition.go](../internal/app/acquisition/acquisition.go), and
[the Runner adapter](../internal/adapters/nzbd/nzbd.go). Verify rather than
reimplement them. Hidden container rows must leave child accounting intact.
Failure paths and `Failure:Files` detail must survive both polling and push.

Finish the control schema across poll, events, persistence, acquisition guards,
Activity UI, retry/resume actions, and storage inventory. Display bytes, current
stage, held cause, retained-file availability, and import result separately.
Do not let a 100% byte bar imply successful import. Known partial files remain
ineligible. Broad parent-path imports protect overlapping active work, while
cleanup authority remains per-file committed receipts and source identities.

Build dry-run recovery reconciliation using the existing placement and receipt
coordinator in [ADR 0021](adr/0021-file-lifecycle-recovery.md). Produce a per-file
plan with identity, evidence level, intended episode/copy, existing library
receipt/digest, duplicate relationships, required capacity, and blocked reason.
Initial observations while work is active are provisional. Mutations would
require leases and a fresh authoritative snapshot after quiescence.

Do not run a real Monster recovery in this build session. Synthetic tests must
cover the mixed HONE/BETTY episode selection, the partial episode 5, pending
receipt acknowledgements, parent-directory overlap, and a stale plan after
another import commits. No cleanup becomes eligible merely from apparent
size, EBML headers, a successful copy, or an expired HTTP request.

**Acceptance:** E2E fixtures show a held job through restart with no replacement;
resume targets the existing Runner job and respects unresolved holds. Inventory
counts payload children once. Dry-run recovery lists exact actions and reasons
without changing sources, libraries, queue records, or blocklists.

## 9. B6 — final verification, documentation, and delivery

Use pinned toolchains from the chosen source. Investigation used Runner Rust
1.97.1 with MSRV 1.95 and Monarr Go 1.25.7; verify pins again. The installed
rustup entrypoint is `/Users/pjunod/.cargo/bin/rustup`; avoid silently testing
only with the unrelated Homebrew compiler. Keep Cargo.lock unchanged unless
a deliberate dependency change is required and documented.

```bash
# Runner, from its actual implementation checkout with the pinned toolchain.
make check
make test-strict
make ui-test

# Monarr, from /Users/pjunod/code/monarr.
make lint
make test
make test-web
make test-e2e
```

During development run targeted crate/package tests first. Repeat affected
state-machine, persistence, and concurrency cases with varied ordering.
Use Go `-shuffle=on` and repeated affected-package runs; final full gates must
exercise the exact delivered code. Record command, toolchain, revision/dirty
scope, exit result, and skipped prerequisites in the build ledger. Do not carry
forward earlier gate counts as proof of new code. Identify unrelated concurrent
failures with evidence and fix only within authorized scope.

Maintain a failure matrix covering writer, extractor, PAR, relocation,
API/persistence, consumer acquisition, and recovery receipts. Every review
finding R1–R9 must map to concrete test names. Add old/new snapshot, wire-client,
and database migration fixtures. Cross-repository contract fixtures must be
identical in meaning, including unknown fields and stale revisions.

Update integration, settings/usage, lifecycle, and operational documentation
with actual implemented behavior. Stamp build provenance using the existing
build system; include a check that a release image does not report `+unknown`.
Do not mix historical evidence changes with newly implemented claims.

The final build ledger must distinguish:

| State | Meaning |
|---|---|
| Implemented | Code and migrations exist |
| Locally verified | Named tests passed on the delivered code |
| Mount-verified | Native publication/crash assumptions checked on the deployment storage |
| Deployable | Compatibility, migrations, ownership, and release gates all satisfied |
| Deployed | A separate deployment actually occurred; not part of this build request |

Provide explicit per-repository `git add` paths and commit commands. Do not
commit unrelated existing changes or use `git add -A`. The user did not ask for
automatic production deployment. Finish by reporting completed phases, exact
verification, any remaining release gates, and where to inspect the changes.

**Acceptance:** B0–B5 implementations and meaningful regressions are complete;
all available required gates pass; unavailable environment gates are named and
leave affected features disabled/held. The handoff includes migration/rollback
constraints and synthetic recovery evidence, with no live backlog mutation.

## 10. Build sequence and progress checkpoints

| Order | Checkpoint | Required output |
|---|---|---|
| B0 | Baseline and regressions | Source provenance, preserved work, executable reproductions |
| B1 | Containment | Durable holds, no automatic regrab, no repeated copy |
| B2 | Resource resume | Durable range validation and bounded same-job recovery |
| B3 | Payload correctness | Names, PAR identity/layout, safe extraction generations |
| B4 | Publication | Compatible generation path with reader/ownership enforcement |
| B5 | Consumer/recovery | UI and persistence complete; dry-run backlog plan |
| B6 | Verification/delivery | Gates, test-to-finding matrix, docs, precise change list |

Update the build ledger at every checkpoint. Continue to the next phase once
its dependencies are satisfied; a finished B1 is valuable but is not completion
of this request. If an actual-mount or production-only gate cannot be exercised,
finish all local code and synthetic tests, preserve the hold behavior, and
record the exact remaining validation without guessing its outcome.
