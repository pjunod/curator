# Download lifecycle — implementation and review status

**Status:** Both PRs open; source corrections reviewed; Curator coverage gate remains below its floor; not deployed
**Updated:** 2026-10-23

Companion to [the approved operating proposal](DOWNLOAD_LIFECYCLE_PROPOSAL.md)
and [the replacement API draft](BAD_DOWNLOAD_API.md). This page records actual
implementation progress, review findings, validation, and delivery. Design prose
is not evidence that a behavior has been implemented or tested.

## 1. Scope and working copies

The user authorized Runner and Curator implementation in the proposal's order.
Lower-cost GPT-6 Luna and Sol agents build; the primary agent reviews design, code, and
integration. Existing processing, failed, and completed backlog files are outside
automatic rollout cleanup until a read-only report and concrete batches have
been reviewed and authorized. No production service or media mutation has run.

Independent clones preserve the owner's checkouts and unrelated work:

| Repository | Baseline | Working copy | Branch |
|---|---|---|---|
| Curator | `7e8e2f2` | `/private/tmp/noirr-lifecycle-20261022/curator` | `codex/download-lifecycle` |
| Runner | `42995f0` | `/private/tmp/noirr-lifecycle-20261022/runner` | `codex/download-lifecycle` |

Curator main already includes the zero-request-cap unlimited fix (#69), newer
than the proposal's original audit. Preserve it; complete the remaining explicit
per-indexer control and visibility work rather than duplicating that fix.

## 2. Milestones

| Milestone | State | Acceptance |
|---|---|---|
| Durable ownership, delivery, storage verification | Built and source reviewed; restart and custody regressions pass | Crash recovery is explicit; old backlog cannot gain automatic deletion authority |
| Runner payload-only publication, salvage, retention, retries | Built and source reviewed; strict regression suite and normal Linux CI pass | Only intended payload is published; source cleanup follows durable delivery |
| Curator adoption, additional copies, import/cleanup, Resolve | Built and source reviewed; worker and admission regressions pass | Durable purpose and per-file outcomes survive retries and Activity dismissal |
| Scoped replacement, reports, indexer controls, timelines | Built and source reviewed; scoped replacement and UI regressions pass | Agreed scope and UI behaviors are implemented and observable |
| Final Astra review and validation | Adversarial source/fix review complete; coverage and final Curator CI remain unresolved | Astra reviews the final PRs; findings addressed; repository gates and lifecycle acceptance verified |
| Fable review handoff | Prepared for independent review; both PRs remain unmerged and coverage blockers are explicit | Both PR links, agreed behavior, validation evidence, review findings and remaining risks supplied for independent review |
| PRs, merge, rollout | Runner PR #254 and Curator PR #70 open; keep both unmerged for Fable review | User receives handoff before merge; backlog remains separately controlled |
| Existing backlog report and batch actions | Read-only per-payload report captured; five exact duplicate media pairs verified for proposed batch V1 | No imports, retries or deletion executed; batch approval remains separate |

## 3. Foundation integration decisions

1. **Use durable pull delivery.** Runner already exposes history polling and
   SSE, but history visibility cannot be custody authority. Add an immutable
   delivery record and manifest discoverable after restart and history removal.
   Curator persists acceptance before acknowledging the exact generation.
2. **Runner cleanup is independent of acknowledgement.** Source retirement
   starts only after publication and the pending delivery record are durable.
   Publication, acceptance, and source cleanup have separate persisted results.
3. **Storage identity describes the mounted source.** Transient device/mount IDs
   may help select and validate a current operation, but are not persistent
   proof that expected storage changed. Failed observations preserve prior
   trusted state. Unsupported verification must not silently grant deletion.
4. **Legacy review follows immutable identity.** Existing jobs remain protected
   as their paths and states change. A mutable row snapshot or timestamp cutoff
   must not let an old acquisition escape the review cohort.
5. **Protocol selection belongs to admission.** A download records whether its
   client negotiated Delivery v1 before submission. Upgrading a client later
   does not retroactively require a delivery for an older job.
6. **Acknowledgement does not grant Runner publication deletion rights.**
   Curator can import after local durable acceptance and validation while a
   transient remote acknowledgement failure retries independently. An explicit
   conflicting acknowledgement blocks the operation for review.

**Storage enrollment limit:** old snapshots contain transient directory
identity but no trusted mount source. The first successful read-only inventory
observation enrolls the stable source/type/root tuple. It cannot distinguish an
intentionally local folder from an absent network mount exposing an underlying
local directory at that first observation. Existing backlog generations remain
protected by the independent review cohort, and new deliveries require exact
payload manifests. After enrollment, an unavailable or different source never
replaces the trusted tuple; restoring the same source resumes automatically.

## 4. Review findings and resolution

The table records issues and correction requests from successive source reviews.
Its entries preserve those review actions; the candidate notes and validation
table below record the later implementation and test results. A requested
regression is not proof that its final gate passed.

| Finding | Review action |
|---|---|
| Storage check returned success when no configured root matched | Corrected; final tests pending |
| Linux mount lookup could cover an absent path and choose a hidden stacked mount | Corrected using visible mount ID; stable identity excludes that transient ID |
| Storage tests only proved parser behavior, not persisted automatic recovery | Service regressions authored; not executed |
| Legacy fence matched mutable paths/title/handle and could be bypassed by normal completion | Immutable generation and independent legacy cohort implemented; final tests pending |
| Unknown cleanup reference could bypass legacy fence | Missing/stale generation now blocks automatic cleanup |
| Runner visible history cannot preserve pending delivery after user dismissal | Dedicated durable Delivery record implemented |
| Source retirement could run before durable publication receipt | Deferred relocation retirement now gated at admission and execution |
| Runner could mutate a publication after transferring ownership | Transfer and durable delivery share one transaction; transfer fences mutation |
| Crash after consumer import but before Runner terminal history required deleted payload | Recovery from durable delivery implemented; admission correlation under further review |
| Runner wire types and authentication did not match Curator adapter | Numeric fields corrected; existing API authentication alignment underway |
| Mutable source-cleanup progress was treated as immutable publication | Correction and regression coverage requested |
| Retry work could starve behind old unresolved receipts or survive as stuck running | Due-work selection and restart recovery corrections underway |
| Acknowledgement outage blocked otherwise viable local import | Separate durable acknowledgement and import progress requested |
| Initial stable storage enrollment lacks a previously stored mount source | Explicit enrollment limitation documented above; no filesystem-type guessing |
| Current publication path was incorrectly treated as proof that no processing source remained | Corrected to resolve relocation first and require immutable origin proof |
| A processing successor lost its ownership cohort after detaching its predecessor | Cohort captured before detachment; regression authored |
| Existing pending destructive Runner operations could resume during migration | One-time migration moves old destructive operations to review |
| Distinct manual requests for one URL could merge and overwrite their transfer identity | Request-scoped Runner admission correction in progress |
| Activity deletion could erase manual request idempotency | Independent request tombstone implemented during manual-copy slice |
| Incoming bytes could change between manifest validation and placement | Placement now requires the accepted manifest digest |
| Manual admission could mark submission complete before persisting its client handle | Atomic handle/phase commit requested; exact-token reconciliation must remain possible |
| Legacy cleanup could bypass the Delivery ledger and remove an entire payload | Delivery-managed guards added at legacy deletion boundaries; regression review pending |
| Partial-pack cleanup could require already removed files on its next import attempt | Retry now consults per-file outcomes; crash and partial-success coverage pending |
| Sidecar selection initially prevented media import, then retained sidecars indefinitely | Media selection corrected; associated sidecar placement and cleanup requested |
| Completed cleanup discarded its verified library file identity before unlink | Final library identity/ownership recheck requested |
| Cleanup held global locks while hashing large media files | Hashing moved outside global locks; short guarded mutation retained |
| A payload-selector test was labeled salvage without exercising failed post-processing | Actual failed-pack verification and publication path still required |
| Admission replay returned an old handle without exposing its terminal result | Durable authenticated request-admission lookup added; Curator integration underway |
| A ready partial-salvage delivery could restart as unconditional success | Exact failure outcome must be persisted with publication and restored |
| A successful direct-media PAR repair lacked publication evidence | Generation-bound repair verification is being added |
| Sidecar retries lost association with already placed media | Associations now recover from the per-file ledger; regression authored |
| Manual Resolve retries could re-select files already imported and cleaned | Remaining-selection correction requested |
| Equivalent encodings were kept indefinitely because redundancy required identical bytes | Scoped quality evidence added; copy-profile and stale-health checks under review |
| Polling many acknowledgements or cleanup records during an outage could block queue refresh | Bounded control-plane retry scheduling implemented; final validation pending |
| A multi-episode replacement stopped after its first successful import | Worker must continue uncovered scopes using this operation’s committed placements |
| Mark bad required unchanged original placement bytes | Explicit read-only observation preparation is being added so changed or damaged files remain actionable |
| Bad-file quarantine could mutate a stale path before validating authority | Generation-bound observation, guarded mutation and durable crash recovery requested |
| Replacement polling could repeatedly search or starve later operations | Durable due times, backoff and fair work selection requested |
| Runner remount recovery accepted a device match despite a conflicting filesystem identity | Stable mounted-source tuple implemented; visible mount selection and legacy enrollment under review |
| A salvage publication error could fall through into destructive failure disposition | Error path now returns before disposal; actual I/O/capacity retry regressions remain required |
| PAR-repaired output was trusted from its path rather than durable post-validation evidence | Persisted identity and SHA-256 proof added with mutation regression; final tests pending |
| Queue deletion or coarse completion could falsely settle Runner source custody | Admission tombstone now requires explicit artifact/Delivery-backed terminal evidence |
| Native receipt lookup depended on the job still appearing in queue/history | Independent bounded receipt polling requested |
| Existing blocked-import retry could bypass the new Delivery or legacy review boundary | Boundary guard and recoverable retry scheduling corrections requested |
| Bad-file quarantine treated its own hard-link/unlink ctime changes as external mutation | Controlled metadata transitions and crash regressions requested |
| Missing-file replacement could delete its database record without a final absence check | Final anchored checks of original and quarantine names requested |
| Explicit Keep incorrectly blocked automatic replacement | Corrected in native custody gate; final regression execution pending |
| File-local replacement progress disappeared when bad media was removed | Persistent item-level operations and review-flag interface requested |
| Replacement UI defaulted the season-pack blacklist choice and generated a new request ID on retry | Explicit choice and stable action identity corrections requested |
| Retry UI coupled Runner source cleanup to Curator acknowledgement | Independent retry controls requested, matching the approved custody boundary |
| New failed downloads could still exhaust the old three-attempt disposal budget | Durable indefinite cleanup retry and warning corrections requested |
| New policy was tied to the Curator wire capability, leaving direct Runner work on old policy | Independent admission cohort implemented; selected-publication retirement under review |
| A standalone selected publication could queue whole-source deletion | Per-file retirement must apply independently of Curator notification |
| Generic upgrade cleanup could delete a file still serving unselected good episodes | Replacement-specific post-commit cleanup and integrated shared-file regression requested |
| URL retry progress wrote a stale whole-job snapshot and fetches could outlive deletion | Owner-command field updates and per-job cancellation/deduplication requested |
| Delivery Resolve opened the old recovery-mount selector instead of the selected payload | One path-selected resolution surface requested |
| Explicit completed-file deletion required original manifest bytes even for a freshly reviewed corrupt file | Current-observation authority must be separate from automatic cleanup proof |
| A salvaged pack retained unconsumed archives merely because their extension suggested extraction | Astra confirmed the permanent retention veto; disposition must use actual recovery evidence |
| A later manual selection could reuse an earlier legacy approval | Approval must bind the current exact selection, source observations and target identity |
| Standalone publication used whole-directory rename despite a selected-file manifest | Astra requires selected staging independently of whether Curator receives a Delivery |
| Runner compared pre-mutation ctime after its own rename or hard-link | Owned transitions need journaled post-mutation identities while retaining external-change checks |
| Copy failures lost their I/O classification and an interrupted relocation could not resume | Preserve typed errors and recover the same exact staging operation after EIO/ENOSPC and restart |
| Normal post-processing failure could delete source despite per-download Keep | Read current Keep at the guarded destructive boundary and force retention when selected |
| Runner admission HTTP JSON omitted durable terminal/custody fields | Serialize the complete contract and test the actual HTTP handler |
| Migration fenced queued/running legacy deletes but omitted retry operations | Fence every resumable pre-upgrade destructive operation before reconciliation |
| URL deduplication could mutate an existing admission's delivery intent | Preserve the original admission protocol and cohort on deduplication |
| Runner Delivery UI used a mismatched accepted-query field and lacked pagination | Match the wire query and expose later pages, including accepted receipts |
| Runner source deletion still used the older multi-hour retry schedule | Apply the approved 30/60/120/300-second schedule to new lifecycle operations |
| Curator pack extras were checked as redundant library media and could never clean up | Bind selected-scope disposal to committed replacement coverage; keep redundancy proof separate |
| Curator's repeated redundancy proof lacked the source path and file generation | Populate and revalidate the exact observed source before cleanup |
| Replacement file size was observed but not persisted with the operation | Persist the reviewed size and exercise create, restart and worker cleanup together |
| Automatic admission could lose its handle when Runner's Add response was lost | Bind the exact validated admission handle before holding for Delivery discovery |
| Curator's own hard-link placement changed ctime and invalidated source cleanup | Preserve authorized transitions or use independent copies without weakening tamper checks |
| A crash before the terminal import stamp could leave a resolved but deleted publication unrecoverable | Reconcile committed per-file outcomes without requiring already cleaned source bytes |
| Repeating a stale file observation reused its request identity forever | Allow a new explicit observation while retaining idempotency for uncertain retries |
| Resolve queued a selected unresolved file but an imported receipt returned without doing work | Durably reopen only the selected unresolved import obligation and preserve prior placements |
| Shared rejected files and unrelated healthy alternate sources shared one cleanup disposition | Detach selected episode links only from the rejected source; preserve unrelated mappings |
| Quality replacement cleanup trusted database coverage without checking current replacement bytes | Require current committed replacement evidence and no open review flag before retiring usable media |
| Cleanup retry could fail after unlink but before recording removal | Recover the exact pending removal from durable authority while preserving mount and generation checks |
| Automatic submission code populated a retry payload that SQLite insertion discarded | Persist the exact submission payload, token and phase at the real admission boundary |
| First-review absence could mistake a detached NAS for its underlying local directory | Require a previously trusted library-storage identity before treating absence as replacement authority |
| Keep changes after publication targeted the publication instead of retained Runner source | Resolve and update the exact retained source generation; keep publication provenance immutable |
| Keep mutation could recursively acquire the same inventory database mutex | Release the query guard before loading the retained source artifact |
| Disabling Keep could delete genuinely recoverable source files | Separate positive recovery evidence from terminal disposal and explicit Keep dispositions |
| Automatic submission treated the database's zero timestamp as a parked row | Match persisted timestamp semantics and verify the actual Grab path |
| Selected re-import reopened the receipt but the queued worker returned on the imported download state | Make the worker honor the durable selected-file obligation; exercise the actual queue and restart |
| Resolve used a receipt captured before waiting for an active import | Reload and validate the receipt after acquiring the download lock |
| Cancelled uncertain manual submissions remained eligible for replay | Apply cancellation checks to manual resume, due-work selection and retry writes |
| Definite native admission refusal was treated as an uncertain response forever | Stop immutable-token conflicts and validation refusal; preserve lost-acknowledgement and temporary-failure retries |
| Lifecycle HTTP retries discarded Retry-After and automatic Add had no initial retry deadline | Persist the greater of local backoff and a valid server deadline; do not bypass it with Retry now |
| Publication replay reopened a source already removed by its own successful rename | Recover from the durable target proof when source hashing is already complete |
| Current admission receipts retained the original Keep choice after a source toggle | Refresh mutable Keep from the exact retained source generation; preserve original Delivery intent |
| Resumed publication error handling could overwrite a concurrent deletion or newer generation | Revalidate authority before every recovery write, including retry/error and pre-staging paths |
| Scheduler deadline checks did not protect every actual HTTP dispatch | Recheck under the operation lock immediately before Add, acknowledgement and cleanup retry |
| Generic extractor failures could classify storage faults as unusable data | Separate positive archive corruption from retryable I/O, permission and tool failures |
| Failed-health restart could reassess a committed publication before its Delivery was saved | Resume the original selected-publication journal before diagnostics or disposition; preserve source identity and terminal intent |
| PAR workspace storage/authority errors were treated as local tool failures | Preserve typed Artifact/I/O errors through the repair workspace and retry the affected operation |

### Candidate review and remaining work

Astra completed adversarial source review and focused fix reviews across both
repositories. The primary reviewed the final dispatch-deadline, failed-pack
publication replay, and atomic Keep-allocation corrections. The implementation
is now in behavioral validation; source-review acceptance does not establish
that its regression tests pass.

- Runner seeds a preallocation Keep opt-in in the first durable artifact row.
  Existing artifact settings remain authoritative over later queue snapshots.
- Curator excludes manual submissions before applying the automatic retry batch
  limit, so older manual rows cannot starve automatic retry work.
- The first backend gate exposed missing Goose statement boundaries around SQL
  triggers in migrations 0041 and 0043. The boundaries are corrected without
  changing their custody guards; migration execution is being rechecked.
- Lint exposed an automatic admission worker that was not registered at service
  startup. Startup wiring and its worker-lifecycle regression are corrected.
  Validation also caught an empty persisted automatic submission payload; the
  actual URL/category/priority now survive a lost Add response and database reopen.
- Backend validation exposed a queue deadlock: enqueueing an import acquired the
  storage lock held by an active placement worker. Queue admission now leaves
  that lock to the actual worker. Activity dismissal also preserves unresolved
  Runner acknowledgements after payload cleanup and restart.
- Runner validation caught missing direct-file inventory refresh, rejection of
  its own client-attribution metadata, and lost provenance across filename
  restoration. Corrections retain finalized writer CRC verification and the
  controlled identity/digest chain across multiple owned renames.
- Browser gates are exercising the new Resolve, lifecycle trace and replacement
  actions at desktop and phone sizes. Test fixtures are being aligned with the
  actual disclosures and explicit blacklist choice; product geometry remains
  part of the gate.
- qBittorrent acknowledges Add without returning a handle. Its adapter now sends
  an exact transfer tag and reconciles only that tag; duplicate native releases
  that do not echo the new token remain visibly unresolved. The e2e fake must
  preserve those tags too. Tokens use 128 random bits, and migration 0060 refuses
  a downgrade that would discard exact-correlation requirements.
- Cluster post-processing now seals the current attempt's selected publication,
  checks its manifest, and rejects a missing successful output. Failed sealing
  removes its private partial copy. Legacy jobs retain their existing handoff
  without acquiring automatic deletion rights.
- Interrupted Runner relocation held its database mutex while calling another
  database operation, deadlocking restart recovery. The guard is now released
  before recovery continues. Hard-link publication and source removal also need
  separately verified metadata transitions; nested directories and private
  copy publication are part of the regression checks.
- Legacy Resolve deletion mixed absolute inventory paths with relative selected
  paths. Both now use the same namespace, with each selected identity bound to
  the reviewed snapshot and live observation. A completed file still referenced
  directly by the library cannot be deleted through leftover-file resolution.
- A client can reject admission in its application response while returning
  HTTP 200. qBittorrent and SAB now expose a typed definite refusal; manual
  requests stop instead of waiting forever for a nonexistent transfer token.
  Transport uncertainty remains a separate reconciliation case.
- Runner admission refresh now preserves a pinned source identity across queue
  omission and partial updates. It never combines Delivery and Artifact fields
  into a synthetic generation or replaces live Keep with an old queue value.
- Browser validation exposed a separate season-plan admission path that had
  not adopted exact transfer correlation or typed application refusals. Its
  title-based fallback could bind an earlier manual payload. Planned dispatch
  now persists exact correlation and source-release provenance, uses opaque
  random tokens, shares the download lock with retries/cancellation, and
  preserves provider retry deadlines. Focused restart, refusal, ambiguous-tag
  and concurrent-dispatch regressions passed; full gates are running.
- Runner's qBittorrent compatibility adapter now persists validated Add tags
  through pending admission and snapshot restart and echoes them in status.
  Duplicate native hashes preserve the original job's tag rather than
  assigning it to another request. Malformed UTF-8 and reserved-key injection checks passed. The embedded
  UI gate passed 871 DOM assertions and 41 boot-time element lookups.
- Browser validation then exposed an import/admission lock inversion: a pack
  held destination authority while waiting for storage authority, while a new
  admission acquired those locks in the reverse order. The correction uses
  one order and stages large files outside storage authority. A broad-lock
  workaround passed focused tests but was rejected because it would block
  unrelated control actions for the duration of a media copy. Its passing
  test result is not final validation.
- The lifecycle disclosure fetched once and did not refresh after asynchronous
  retry completion. It now polls while visible; browser regressions prove
  hidden disclosures stop polling and background refresh preserves focus and scroll.
- The corrected lock order and final publication identity checks pass the full
  Go suite and all 174 browser tests. Manual admission also rechecks its exact
  selected files after waiting for locks, preventing a deleted selection from
  creating a forgotten job.
- Primary review found that startup placement reconciliation bypassed the
  staged-object checks used by normal publication replay. Both paths now share
  that check. Regressions reject a same-content different inode, preserve old
  digest-only journal compatibility, and prevent partially identified journals
  from falling back to that compatibility path.
- Astra reviewed Runner PR #254 at `f53cdcbfc9d0fa4a8f193cbc9a22b66c26738554`
  and requested three corrections: retain verified files when they are actual
  inputs to a shared PAR repair set; use the greater of scheduled backoff and
  Retry-After; durably reassess retained sources when Keep is disabled. Follow-up
  review found that Keep-off also had to resume an existing deletion journal
  after retryable I/O or interruption, including ordinary failures where Keep
  was initially disabled and no separate failure assessment was saved. Revision
  `f043a02` resumes only the exact authorized journal, preserving attempt progress
  and review holds; fresh deletion still requires an unusable-source assessment.
  State regressions pass (181 passed, two ignored); Astra reviewed
  Runner head `b7c259df5c912f3f41d35f1d29374bf4ab051a45` and approved
  source with no remaining actionable findings. Approval carries through
  `d84cd83db7e2567e8b660ad00af940f6c402f8a7`, whose only change waits for
  actual three-voter quorum readiness before cluster failure injection. Primary review also
  required unknown-integrity siblings to remain protected after partial
  publication, not only after total failure.
- Astra's Curator PR review at `dd7f09d6ffb3947c21c87b86151d9a85a5a499cf`
  requested four corrections: reject ambiguous transfer tags in both manual
  restart paths; record actual pack-excess provenance; wake its cleanup after
  replacement commits; and import matching sidecars with manual additional
  copies. Worker/restart regressions now exercise these real paths. Primary
  review added uniquely associated excess-sidecar cleanup and persisted retry
  deadlines when current replacement proof is temporarily unavailable. Unknown
  sidecars and ambiguous media associations remain protected. Astra reviewed
  `31c41c1ea0b8d7a2273cff8652e05529e2210d09`, confirmed all four findings
  resolved and found no additional actionable source defect. Astra subsequently
  approved `9cfd7184f23eefb5313099da953058169f0c0cc8`, including the atomic
  file/directory cleanup obligations and saved blacklist-choice response.
  Coverage and CI remain separate requirements.
- Runner Linux dependency audit also reports vulnerable locked dependencies.
  Compatible patches to quinn-proto, rustls, rustls-webpki and the WebPKI
  root packages pass local compilation, strict Clippy and MSRV checks. The
  revised Linux RustSec audit passes on `e3135e9`; no exceptions were added.
- Additional fault injection found that file-removal recording and the next
  directory-cleanup obligation could commit separately. They now share one
  transaction; directory removal also persists its own pending obligation
  before unlink. Failures recording completion can therefore resume after a
  database restart without touching library bytes.
- The saved source-blacklist choice was honored internally but omitted from
  replacement-operation responses. Detail, history and exact replay now return
  `blacklistSourceRelease`; the existing UI shows that saved choice.
- Full Linux race tests pass at Curator `ce7e550`; its coverage result is 84.8%,
  below the unchanged 86% floor. Runner normal Linux PR checks all pass at
  `708f9af`; its separate manual coverage gate remains below 90%.
- Validation found that a positive RSS deadline already in the past waited
  another 15 minutes. The corrected scheduler promptly attempts it once, then
  uses the idle sweep if no work changes that deadline. Future deadlines,
  per-indexer quotas and provider retry limits remain enforced.
- A database failure during manual-import admission could leave an orphan row
  after the caller received no ID. Target, selection, review approval and row
  state now commit together. Trigger-backed tests verify rollback and retry for
  both new and existing records. Astra reviewed both fixes; they are committed
  in `720a148`. Full ordinary tests pass, lint reports zero issues, and coverage
  is 84.4%. Further failure-path tests are being added to meet 86%.
- Retry and observation fixes in `ce7e550` pass full ordinary Go tests,
  lint, and focused race tests across acquisition, SQLite and API. Coverage
  is 84.7% (22,565/26,629 non-generated statements); the 86% gate still fails.
  A stale observation can now terminate after its library row changes or
  disappears, without retaining digest or filesystem authority. Replayed
  handleless acknowledgements preserve retry history and deadlines.
- Explicit selected-file deletion now journals the complete reviewed batch
  before unlink and atomically commits each removed outcome with its history
  event. A local bounded worker discovers pending batches after restart without
  Runner or the original browser request. Ordinary cleanup preserves pending
  selections and their publication directory until audit completion. The saved
  absolute path spelling preserves alias-based review identities; retargeting
  an alias retains both source and replacement bytes and records a retry against
  the original batch. Retry now includes those saved batches.
- Migration 0061 retains immutable file/parent, receipt, publication and storage
  identities plus retry state. Its downgrade refuses to discard any batch
  history; use a pre-upgrade database backup for rollback. SQLite tests cover
  both empty-journal downgrade and populated-journal refusal.
- Astra reviewed the complete deletion-journal correction with no remaining
  actionable findings. Full ordinary tests, focused acquisition/SQLite/API race
  tests, pinned lint and two identical pinned generations pass. Coverage is
  84.9%; the unchanged 86% gate still blocks final validation. Added regressions
  also cover stale recovery targets, changed quality replacements, sidecar
  ownership, protected language requirements and real HTTP import retry.
- Legacy selected-file deletion now compares active reservations and unfinished
  placement paths in the same canonical storage namespace as the reviewed
  payload. A symlink alias can no longer hide live ownership. Regressions prove
  both fences preserve selected and sibling files, while an unrelated active
  download does not prevent an authorized selected deletion. Astra approved
  the source correction; full Go tests, focused race checks and pinned lint pass.
  Additional integrated tests cover running manual-import selection, recovery
  receipt rollback and exact retry, stale recovery-revision transaction rollback,
  and season discovery recovering after indexer capability changes. Coverage
  remains 84.9%; the required 86% gate is unchanged.
- A further cleanup audit found that the legacy partial-payload absence path
  could settle custody without verifying the mounted storage. Candidate sweeps
  also lacked a complete generation check after waiting for a row lock. The
  correction preserves the selected generation across the sweep and final
  database writes, verifies storage before accepting absence, and retains
  pending custody during an outage or mount mismatch. Astra approved the source
  correction; the full Go suite, focused race tests and pinned lint pass.
  Regressions use an actual reused row ID with the original timestamp and a new
  generation, changed Delivery ownership, unavailable storage, a different mount,
  and automatic recovery after the expected source returns. Coverage is 84.9%
  (22,988/27,073 statements), below the unchanged 86% floor. This does not
  authorize backlog cleanup.
- Further import validation reproduced an all-duplicate payload that had durable
  redundant outcomes but remained in import retry. The completion path now
  accepts that outcome only when every selected file has a persisted redundancy
  proof and all ledger writes succeeded. Manual additional copies cannot take
  this path. Exact-byte native-book redundancy also respects format upgrades.
  Focused tests cover video and book duplicates, changed bytes, an obstructed
  book upgrade, and an injected ledger-write failure followed by retry. Unmatched
  sidecars remain protected; this does not claim every publication becomes empty.
- Two obsolete SQLite deletion mutators were removed. Explicit selected-file
  removal now has one journaled contract; tests exercise its batch and audit
  records rather than a second unused mutation path.
- Recovery fault injection reproduced a cancellation request lost to a local
  database write failure: the worker could finish placement and then retry a
  receipt Runner would never accept. Curator now reconciles that rejection
  against the exact remote artifact, generation, manifest, publication and
  import ID. Cancellation and its audit annotation commit together; the receipt
  remains undelivered history but leaves the dispatch queue. After remote
  acknowledgement, restart finishes the local record without reopening Runner
  cancellation. Prepared placements are rolled back; committed library files
  are preserved. Primary and Astra source reviews pass. Focused tests include
  the real 30-second heartbeat, two separate database failures, mismatched
  claims, and database reopen; the full Go suite, focused recovery/import/API race tests and pinned lint pass.
  Coverage remains 84.9% (23,126/27,227 non-generated statements), below 86%.
- Runner `677d4fa` encountered newly published Tokio advisory
  `RUSTSEC-2026-0386`. Revision `83cd4cd` updates Tokio to 1.53.3 and the
  yanked chacha20 dependency to 0.10.2. Workspace tests, strict Clippy,
  formatting and the Rust 1.95 check pass. Linux supply-chain and main fast-lane
  workflows both pass at `83cd4cd`; Astra approved that exact source revision.
- Subsequent tests exercise additional-copy replacement without touching the
  primary copy; changed current library proof; recreated nested parents during
  explicit deletion replay; a failed directory-cleanup obligation before removal;
  post-publication metadata and cleanup-stamp failures; and replacement admission
  committed before its operation-step journal. Fresh-service replay preserves the
  exact download token and handle without a second Add. A real candidate is held
  while changed quarantine bytes block cleanup, and admitted once valid reviewed
  bytes retire. The full Go suite, focused race checks and lint pass at source
  checkpoint `62eebc2`; Astra approved that exact checkpoint. Coverage is 85.1%
  (23,175/27,227 statements), still below the unchanged 86% floor.
- Astra approved source checkpoint `7e40206`, including actual HTTP copy-scope
  refusal, completed-root discovery after history dismissal, and manual-import
  admission and queued-worker regressions. The full Go suite passes with
  85.1% coverage (23,175/27,227 statements); the 86% gate still fails. Focused
  acquisition/API race tests pass. The test-only tagged-switch correction in
  `5243139` passes the focused test and pinned lint reports zero issues.
  [The Fable handoff](FABLE_REVIEW_HANDOFF.md) is ready for independent review;
  its validation limits are explicit, and both PRs remain unmerged.

## 5. Validation and delivery

Static compilation, scoped vet, formatting and generated-source checks ran
throughout construction. After adversarial source review and fix review, the
final gates began on 2026-10-22. Results so far:

| Gate | Result |
|---|---|
| Curator web unit tests | Passed: 14 files, 124 tests |
| Curator mobile typecheck and unit tests | Passed: 17 files, 109 tests |
| Curator mobile doctor and Android/iOS export | Passed: 20 checks; both platform bundles exported |
| Curator Go tests | Full ordinary suite passed after lock-order and manual-admission corrections; acquisition completed in 55.4 seconds. Full non-race coverpkg suite also passed its tests |
| Curator lint | Linux identified formatting and four lint issues; corrected locally. Pinned golangci-lint and gofmt sweep pass; final revision CI remains required |
| Curator browser acceptance | Passed: 174 tests, zero skipped, including lifecycle polling and mobile cases, against the rebuilt binary and final Vite bundle |
| Runner strict workspace tests | Passed with required external tools, including actual failed-health salvage, storage-outage Retry Now and HTTP Delivery regressions. Formatting, strict Clippy, Rust 1.95 MSRV and UI harness passed |
| Runner line coverage | Linux `b7c259d` instrumented workspace tests pass, but manual coverage enforcement fails at 86.23% (56,297/65,285 lines), below the unchanged 90% floor. Earlier identical macOS commands measured candidate 86.2814% and baseline `42995f0` 86.5527%; both fail 90%. No Linux baseline measurement is claimed |
| Curator Go race instrumentation | Full acquisition package passed locally in 547.452 seconds with `-race -covermode=atomic -coverpkg=./... -count=1`. Each test still owns an isolated database; an immutable migrated-schema template removes repeated schema construction. This resolves the earlier local timeout. Full Linux race-instrumented tests also pass at `744ae51` (acquisition 790.231 seconds); that job fails only at the 85.0% coverage result |
| Curator coverage | Full ordinary coverpkg suite passes all packages; latest measured candidate is 85.1% after the exact deletion-journal, recovery and replacement worker regressions, below the unchanged 86% floor. Unchanged baseline `7e8e2f2` passed the same command at 86.3%; this deficit is introduced and blocks the coverage gate. Coverage remains unresolved; the floor and exclusions have not changed |
| Curator generated consistency | Two consecutive local pinned sqlc and OpenAPI generations produced identical hashes with migration 0061; diff whitespace check passed |
| Final PR CI | Curator `744ae51`: browser, mobile, lint, Docker, both compatibility personalities and all Go race tests pass; coverage fails at 85.0%. Runner `83cd4cd`: all normal Linux PR checks and supply-chain audit pass. Astra source approval is bound to Curator `7e40206` and Runner `83cd4cd`. The newer Curator duplicate-completion, recovery-cancellation and custody regressions pass full local Go tests, focused race tests and lint; coverage is 85.1% (23,175/27,227 statements), below 86%. Curator final revision CI and coverage remain unresolved |

Required Curator gates: `make lint`, `make test`, `make test-web`, `make test-e2e`,
generated-code consistency and required CI. Runner gates follow its pinned Rust
toolchain, affected UI checks, and repository release/CI contracts. Filesystem
and restart tests use disposable fixtures, never the user's media.

**Review sequence updated by the user, 2026-10-22:** the primary agent reviews
construction, then GPT-6 Astra performs the final adversarial PR review. Address
its findings and record validation results. Provide a separate handoff for the
user to give Fable, covering both repositories and their shared lifecycle rather
than only individual diffs. Keep both PRs unmerged for that additional review.

Read-only backlog report: `/private/tmp/noirr-lifecycle-20261022/backlog-report/BACKLOG_REVIEW_REPORT.md`.
The 2026-10-22 capture found 81 completed entries (814.10 GiB logical) and
87 failed entries (396.50 GiB logical, 224.34 GiB allocated). Sparse files explain
the difference in the failed tree. The report excludes the unrelated Cinema
workspace and separates Runner queue/state. Five selected completed media files
have matching full SHA-256 hashes with current library copies, with unchanged
file identities and exact item/copy/path records after hashing. Batch V1 proposes
only those source media files; no mutation is authorized by the report itself.
Detailed private library paths and observations stay with the local report rather
than being published in the code PR.

Runner Linux cross-check: pinned Rust 1.97.1 resolves the installed RISC-V Linux
standard library, but the bundled SQLite build requires an unavailable
`riscv64-linux-gnu-gcc`. This is an environment limitation, not a passing Linux
check; final Linux CI remains required. Native pinned static builds pass.

PRs: [Runner #254](https://github.com/pjunod/runner/pull/254) and
[Curator #70](https://github.com/pjunod/curator/pull/70).
Merge: not performed. Deployment: not performed. Backlog
deletion/import/retry actions: not performed. Temporary unused managed worktree
was queued for archival; implementation uses independent clones.
