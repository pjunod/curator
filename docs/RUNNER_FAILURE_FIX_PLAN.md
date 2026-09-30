# Download failures — root causes and proposed recovery fixes

**Status:** adversarially reviewed proposal, not implemented or deployed · **Written:** 2026-09-30

This is the implementation proposal for the Monster failures and retained
download backlog. Read [the incident evidence](RUNNER_FAILURE_DIAGNOSIS.md)
for the original observations and [the adversarial review](RUNNER_FAILURE_REVIEW.md)
for challenges and their dispositions. Work through the milestones in §8;
do not run a backlog mutation before its ownership and integrity checks pass.
The existing [recovery contract](adr/0021-file-lifecycle-recovery.md) remains
the authority for library placement and receipts.
The [build handoff](RUNNER_FAILURE_BUILD.md) specifies the implementation
sequence, code seams, compatibility contract, and verification deliverables.

## 1. Conclusion and scope

There are several interacting failures, rather than one broken download.
Runner can misidentify existing archive files, preserve unusable subject-based
filenames, permanently fail writes when storage fills, and repeatedly copy
retained payloads into a destination where its final rename is unsupported.
Monarr then treats coarse failure reports as reasons to acquire replacements.
Together these behaviors consume more space and make subsequent failures more
likely. A full progress bar proves neither a valid extraction nor a completed
library import.

The base `completed` directory is a separate Monarr inventory presentation bug.
Hiding it does not resolve Runner failures or recover the backlog.

**Non-goals:** this proposal does not declare retained media complete, delete
duplicates by filename, clear all failures or blocklist entries, disable PAR
verification, replace the library placement coordinator, or automatically
import every file under `completed`. Each would discard evidence or bypass
the ownership and per-file receipt rules needed for safe recovery.

### 1.1 What has actually changed

The uncommitted Monarr patch in this workspace hides the storage container
while retaining its child entries and byte accounting. It saves paths from
newly received failure reports and displays Runner's `Failure:Files` detail.
It was prepared and verified as `0.34.4`; unrelated concurrent work has since
advanced the shared tree's version. This investigation did not deploy it. See
[inventory code](../internal/app/acquisition/completed.go),
[failure handling](../internal/app/acquisition/acquisition.go), and
[Runner adapter](../internal/adapters/nzbd/nzbd.go).

That patch does **not** backfill old failures, make retained media valid,
repair Runner, or prevent every early failure/regrab race. All changes in
§4–§9 below are proposed work. No production code, configuration, queue
records, or running imports were changed during the investigation.

## 2. Evidence, provenance, and chronology

Inspection occurred on `nuc3` on 2026-09-30. Times below are UTC. The host's
Runner checkout was `ab25d0cbecfdb5adf299072813a2948383192d9f`; its tracked
source was archived to `/private/tmp/runner-diagnosis-ab25d0c`. The running
binary reports `0.2.0+unknown`, so this is **not verified binary provenance**.
Source reproductions establish defects in that checkout. Live files, logs,
operation records, and mount probes independently establish the observations.

| Download | Evidence | Interpretation |
|---|---|---|
| Runner 1679 / Monarr 11, SCENE/BETTY | Downloaded 13:10; PAR rename errors 13:21; repair requests of 7,995 then 7,595 blocks; `PAR_FAILURE` 14:30:57 | Catalog-path mismatch is directly observed; total payload integrity remains unverified |
| Runner 1682 / Monarr 14, bracketed HONE | 21 MiB free at 15:20:32; unpack failed 15:20:45; approximately 33.4 GiB free after staging cleanup at 15:20:52; `UNPACK_FAILURE` 15:32:55, replacement 15:32:56 | Disk exhaustion during this extraction is a strongly supported inference; extractor stderr was not retained |
| Runner 1683 / Monarr 15, replacement HONE | Explicit write/finalize `ENOSPC` at 15:39:22–23 on episode 5; generic `failed` received 15:50:55 with post-processing unfinished | Local capacity failure is confirmed; replacement downloading did not solve it |
| Monster failure relocations | Journal entries in `review`, error `Invalid argument (os error 22)`; fresh attempt after previous staging retained | Publication failure and repeated copying are observed |

For 1679, all 447 flat basenames corresponding to nested PAR catalog paths
exist, while the expected nested paths do not. Four sampled files match
catalog size and first-16-KiB MD5. Eight extracted BETTY episode files also
remain. None of these observations proves all archive slices or media valid.
For 1683, seven finalized episodes and an approximately 7.09 GB episode-5
`.mkv.part` were present. That partial file is not import-ready.

The user-started import selected seven HONE episodes and BETTY episode 5.
Its source path was the broad `/working/monarr/completed` root. A parent-path
association must not be interpreted as ownership of every child or permission
to clean the tree. Re-read its committed per-file receipts before recovery.

### 2.1 Storage measurements are snapshots, not a deletion estimate

The initial inventory reported 39 payload directories and approximately
625 GiB of logical data. It included 180.6 GiB of recognized `.mkv` files,
160.2 GiB of archives, 277.1 GiB of other files, and 6.8 GiB of partial files.
Rounding explains small differences. This is not a verified measurement of
the user's approximately 800 GB estimate or of physically allocated blocks.

Within that tree, 65 misnamed candidates totaled 259.4 GiB. Samples begin with
the Matroska/EBML header, but headers do not prove complete segment coverage,
playability, unique content, or successful import. They overlap the inventory
categories above and must not be added again.

Separately, Monster relocation staging under `/processing/failed` occupied
250.5 GiB across three reviewed attempts; a running retry had another 38.5 GiB
at inspection. The combined 289.0 GiB is additional logical data on the
processing volume. A running attempt changes size. These numbers cannot be
turned into expected freed space without fresh allocation and identity checks.

### 2.2 Relevant ownership and mount boundaries

| Container path | Host path | Consequence |
|---|---|---|
| `/working` | `/mnt/qnap/working` | Includes Curator's completed-download tree |
| `/working/monarr/completed` | `/mnt/qnap/working/monarr/completed` | Runner destination and Curator working-tree ownership overlap |
| Curator `/downloads` | `/mnt/qnap/working/monarr/completed` | Alias of the same tree; do not double count |
| Runner `/processing` | `/mnt/processing` | Runner-owned processing/recovery; separate relocation destination volume |
| Curator `/data` | `/srv/monarr` | Contains `monarr.db`; inspection used read-only access |

Runner has `main_dir=/processing/`, `dest_dir=/working/monarr/completed`, and
`failure_action=park`; failed placement targets `/processing/failed`.
Configured tools are `unrar`, `7z`, and `par2`. The image intentionally omits
`unrar` and uses 7-Zip fallback. Missing `unrar` alone is not the root cause.

## 3. Root causes and contributing defects

Runner source references in this section are relative to the pinned checkout
in §2. Re-verify names and line numbers against the implementation revision.

### 3.1 PAR identity and directory restoration disagree

**Mechanism:** `crates/nzbd-post/src/rename.rs:70` renames a matching file to
its catalog path without creating parent directories. The nested target
therefore fails with `ENOENT`; verification sees apparently missing files
and requests repair blocks despite matching flat candidates being present.

**Why a small fix is insufficient:** `par_rename` stores first-16-KiB hashes
in a one-value map. Multiple candidates with the same prefix can overwrite
one another. `par2.rs` compares a basename to the full catalog path in
`quick_verify`, and merges root PAR packets without separating recovery sets.
The leaf PAR parser does not expose all identity information needed for the
proposed full-digest/set-aware match. File enumeration, archive discovery,
and cleanup are shallow. Restoring subdirectories alone creates another
inconsistent pipeline.

**Additional source-backed hazard:** `manager.rs:2163` `commit_staging`
removes an existing destination directory recursively before publishing an
archive's output. Two archives emitting the same parent directory can erase
previous output or source volumes. This is a correctness defect found during
review, not an established cause of the observed Monster loss.

### 3.2 The filename hint becomes permanent without yEnc confirmation

**Mechanism:** `crates/nzbd-nzb/src/lib.rs:58` returns the entire NZB subject
when it finds no quoted filename. `queue.rs` sanitizes that subject and uses
it as the output name. `crates/nzbd-engine/src/pool.rs:426` receives a decoded
yEnc filename but does not forward it to the writer. A filename containing
`.mkv]-[...]-__ yEnc ...` consequently fails normal extension recognition.

**Impact:** valid-looking retained bytes become invisible to ordinary media
discovery. The file can still be incomplete; naming correction and content
verification are separate steps. Renaming active writers or merely changing
the parser leaves persisted file state and restart behavior unresolved.

### 3.3 Relocation publication fails after an expensive copy

**Mechanism:** `crates/nzbd-state/src/artifacts/fs.rs:410` uses Linux
`renameat2(RENAME_NOREPLACE)`. Tiny probes on both relevant mounts returned
`EINVAL` for same-mount directory publication, while ordinary rename worked.
The cross-mount initial rename returned `EXDEV`, as expected. Read/write copy
and file/directory fsync probes succeeded.

`artifacts/relocation.rs:146` invokes the unsupported exclusive rename after
copying and hashing the staging directory. Failure keeps both source and
staging. The journal moves to `review`; the source returns to `active` with
an advanced revision. A subsequent relocation constructs a new operation key
from that revision and creates another staging copy. Startup reconciliation
of `running` operations does not resolve all the retained `review` attempts.

This proves a publication compatibility problem. It does not prove a broken
read/write copy implementation or justify an unchecked ordinary rename.

### 3.4 Resource failures enter the corrupt-release retry path

**Extraction:** `tools.rs:528` recognizes a Windows disk-full phrase but
misses Linux `No space left on device`. The fallback result selection at
`tools.rs:453` can discard the second tool's disk-space diagnosis.
`manager.rs:1499` does not consume `disk_space_error`. Classification must
happen before fallback extraction and `repair_loop`, or those paths still
consume space and download repair blocks after a local resource failure.

**Writer:** the owner handles writer errors by permanently failing remaining
segments; finalization can record `finalized` and write failure even when
the cause is `ENOSPC`. Merely retrying extraction cannot recover episode 5.
Successful network fetches and preallocated file lengths are not evidence of
successfully persisted segments.

**Consumer:** Monarr's coarse failed event may win before terminal history
and retained-path information exist. Its failure handling starts replacement
search; `Blameless` skips blocklisting but still allows the new search. Once
a record is terminal, ordinary active-job reconciliation is insufficient to
repair it. Better error strings alone cannot stop duplicate acquisition.

## 4. Proposed cross-service contract

The following is a proposed contract, not an existing API guarantee. Additive
wire fields and migrations must be documented in both repositories before
enabling the behavior. Preserve existing IDs and the `monarr-transfer` job
parameter; path strings are not job identities.

### 4.1 Separate lifecycle, failure cause, and file custody

Persist a typed outcome with these independent facts:

| Proposed fact | Meaning |
|---|---|
| Lifecycle | Running, held, terminal success, or terminal failure |
| Cause | Capacity, quota, unsupported filesystem operation, I/O, identity conflict, corrupt payload, unsupported format, missing tool, or unknown |
| Stage | Download write, finalize, PAR identity/verify/repair, extract, relocate, or import handoff |
| Retry policy | Resume this job after an explicit condition, require review, or permit a replacement |
| Custody | Artifact/generation ID, manifest revision, retained path, and source holds; never a claim of completeness |
| Custody publication | Durable registry generation and manifest revision, or absent; makes a retained copy discoverable for recovery review |
| Media eligibility | Separately verified complete media and successful job/import eligibility; never inferred from relocation copy equality |

A failed PAR payload may be published into recovery custody while remaining
failed and review-only. Equal source/copy hashes prove transfer fidelity, not
valid media. Ordinary automatic import requires the independent successful-job
and media-eligibility checks. Recovery preview may display failed retained
files with their evidence limits. A legacy `final_dir` path must preserve the
job's failed/held semantics; publication never changes that job to success.

`held` is nonterminal. Capacity/quota failures suspend writes, extraction,
repair downloads, and further relocation copies for the affected volume.
Identity conflicts and unknown failures retain data for review. A positively
identified bad release can be terminal and eligible for replacement under the
existing policy. A transient classification never marks a partial file ready.

Durably commit the state before sending its event. Give snapshots and events
a persisted per-job monotonic revision that survives daemon restart. Monarr
ignores older revisions and reconciles polling and push to the same result.
If a legacy failed event is ambiguous, fetch authoritative state and retain a
pending reconciliation record; a timeout is not permission to regrab.

### 4.2 Compatibility and acquisition suppression

The adapter's existing `GET /api/v1/jobs` representation contains `status`,
`pp_done`, and `ready`; see [the actual fields](../internal/adapters/nzbd/nzbd.go).
Map a held job to an already supported **nonterminal** status for old clients,
and emit no failed completion event or failed history row for that hold.
Choose and test the exact legacy status against both clients before shipping;
an unknown new enum could itself decode as failure. New clients additionally
receive the structured reason and resumability information.

Monarr must count a held or unresolved transfer as in flight for every
automatic acquisition entrypoint: failure retries, scheduled wanted search,
season-pack expansion, and reconciliation. Persist the suppression across
restart. An explicit operator retry must explain whether it resumes the same
job or starts a replacement; the latter must resolve the old hold first.

Do not silently reinterpret every historical failed record as held. Backfill
only positively correlated Runner job/transfer IDs and artifact identities,
with an audit entry. Preserve genuine release failures and unrelated blocks.

## 5. Proposed Runner fixes

### 5.1 Restore PAR paths using verified identities

Extend the PAR parser to expose recovery-set ID, validated packet digest,
declared size, full-file digest, and first-16-KiB digest. Keep independent
sets separate. Prefix hashes narrow candidates; full size/digest and set
membership establish intact-file identity. Ambiguous candidates enter review
without overwriting or moving either file. Digest matches identify bytes; they
do not make paths supplied by an archive or catalog trustworthy.

Keep a separate path for damaged but repairable candidates. A full-digest
mismatch cannot exclude a file from repair: corruption after the first 16 KiB
is precisely a case PAR should recover. Preserve the original and create a
journaled, capacity-admitted repair workspace. Use validated set metadata and
PAR block verification to test candidate-to-catalog mappings there; do not
assign a sole original to a path based only on a shared prefix or basename.
Publish repaired output only when size and full catalog digest verify. When
mapping remains ambiguous or parity is insufficient, retain the inputs for
review. Partial files contribute only verified blocks, never implicit zeros
from their preallocated size.

Normalize catalog names into bounded relative components. Reject absolute
paths, traversal, separators that escape the chosen namespace, symlinks, and
unsupported entries. Open parents relative to an anchored directory handle,
create missing parents safely, and check root/entry identities at mutation.
Persist the intended mapping and completion per file so a crash can reconcile
old/new locations without repeating destructive operations.

Use one bounded recursive inventory and normalized relative-path convention
through rename, quick verification, repair, archive grouping, extraction,
CRC bookkeeping, and cleanup. Group multipart archives by their directory
and archive identity. Do not merge same basenames across episode directories.

Extract each archive into its own operation-owned generation and record its
output manifest. Merge at the file level only after collision checks. An
existing identical file may be reused with verified identity; differing bytes
require review. Never recursively remove a destination directory to make room.
Retain source volumes until the whole intended job output is verified and
durably published. Cleanup uses the captured manifest, not extension matching
over an entire tree. Reconcile staging by operation ownership, not `.pp.*`
name alone.

### 5.2 Persist a safe filename decision once

Teach the subject parser the observed bracketed form with bounded parsing,
but keep the result a hint. Forward yEnc filename metadata to the queue owner.
The owner accepts a safe basename only when segments agree on file identity,
size, and name. Conflicting metadata enters review; it does not switch an
already-open writer's destination. Retain provenance for the chosen name.

Use a stable internal file ID for partial storage and writer commands. Persist
the final display/publication name before publishing bytes. Reserve collisions
without overwriting another file, preserve the media extension in generated
suffixes, and update classification and restart serialization together.
PAR catalog metadata can correct an obfuscated name only after its stronger
identity checks pass. Neither NZB subjects nor yEnc names authorize paths.

Existing subject-named files use the same identity/verification pipeline in
§7. There is no bulk regex rename followed by automatic success.

### 5.3 Hold resource failures and resume the same job

Preserve every extractor attempt's executable, exit status, and bounded,
redacted diagnostic text. Classify known disk-full/quota output and native
I/O error codes before fallback or repair. Missing-tool fallback remains
valid; disk exhaustion does not trigger a second extractor. Unknown outcomes
remain reviewable instead of being labeled a bad release without evidence.

For writes and finalization, distinguish fetched, written, durably checkpointed,
and finalized data. On capacity/quota failure retain `.part`, expected size,
segment checksums/ranges, and stage. Resume only the unacknowledged or invalid
ranges; do not count a preallocated hole as downloaded. Restart reconciliation
revalidates coverage/checksums against durable checkpoints. A failed sync or
rename cannot acknowledge finalization. Repair the existing job rather than
submitting a replacement NZB.

Capacity admission accounts for concurrent download growth, extraction output,
copy staging, and retained rollback/source bytes on each actual filesystem.
Use reservations across Runner operations and recheck available space; other
processes can consume it, so reservations do not replace runtime error handling.
If extraction size is unknown, allow one bounded attempt with monitoring,
then hold on resource exhaustion rather than looping. Recovery placement
continues to reserve the full copy plus 1 GiB per destination volume as
specified by [ADR 0021](adr/0021-file-lifecycle-recovery.md).

Persist the hold against the actual filesystem identity, operation stage,
observed cause, and quota context where known. An unchanged or unknown
`statvfs` reading never clears write-observed exhaustion. For `ENOSPC`, automatic
release requires fresh capacity admission for the remaining budget, available
space above that budget plus the configured reserve, and a bounded write/flush
probe in the same destination context. The probe confirms a small write works;
it does not establish that an entire extraction will fit. For `EDQUOT`, require
authoritative quota headroom or an explicit operator release, plus admission
and the probe; ample filesystem free space alone is irrelevant.

Journal probe/retry timing. Permit at most one resumed attempt per qualifying
health transition, use bounded probe I/O and backoff, and immediately rehold
on recurrence. Restart preserves attempts and the hold instead of resetting
the retry budget. Probe failure retains the hold and removes only its own
temporary entry. Acceptance must cover partial probe cleanup and unavailable
quota information as well as the successful path.

### 5.4 Publish relocations without requiring unsupported rename

The existing interface is
`Inventory::relocate(&self, job: u32, destination: &Path) -> Result<()>`.
It assumes the payload ends at the requested path. Re-verify in
`crates/nzbd-state/src/artifacts/relocation.rs` before changing callers.

**Immediate containment:** capability-probe the actual destination before any
large copy, using unique owned temporary entries. Unsupported publication
holds the operation with its source intact. Reuse or reconcile existing
operations, including `review`, before allocating another staging directory.
A new artifact revision alone must not authorize a new attempt. This stops
amplification but does not itself make unsupported mounts functional.

| Publication option | Trade-off | Recommendation |
|---|---|---|
| Require proven exclusive rename support | Smallest protocol change; deployment/storage migration and enough capacity required | Supported fast path and temporary operational alternative |
| Replace exclusive rename with ordinary rename after `exists()` | Another writer can create a target between check and mutation | Reject |
| Publish an immutable generation through the artifact registry | Avoids directory rename, changes path/discovery contract and requires consumer migration | Proposed fallback, gated on the contract below |

For the fallback, allocate one unpredictable operation-owned generation with
atomic `mkdir` under a dedicated managed root. Verify its parent/entry identity,
use exclusive file creation and anchored no-follow traversal, then copy,
flush, and hash-verify its manifest. The directory stays at the same path.
An atomic local-state transaction switches its registry state from staging to
published and records the actual generation path. No ordinary rename replaces
an existing directory. The proposed relocate result returns the published
artifact/path rather than assuming it equals the requested title path.

**Visibility is part of correctness:** a dot-prefix or marker is insufficient.
All Runner/Curator discovery, recovery APIs, manual-import browsing, and cleanup
must exclude unmanaged access to this dedicated root and expose only published
registry generations. Mount it separately with the existing read-only recovery
mapping where appropriate. A legacy client may receive the stable actual path
only after commit if it can map that path correctly. If any consumer depends
on direct scanning or a fixed destination name, do not enable this fallback
for it; use the supported-storage option or remain held. This prerequisite
must be verified before deployment, not delegated to an operator assumption.

The registry remains on supported local durable storage. A committed record
does not prove a remote mount still has its data: after restart/remount,
revalidate root/generation identities and the manifest before serving readiness
or retiring source. Failed durability/identity checks retain both copies and
require review. Tiny fsync probes establish syscall behavior, not server-side
power-loss guarantees. Keep sources until the publication durability contract
has been proven for the deployed storage and the intended handoff is recorded.

Use a stable relocation operation ID and a uniqueness constraint for unresolved
operations per source generation and destination purpose. Journal copy progress
and hash proof. Resume a valid staging generation; changed source, destination,
or manifest enters review. Reconcile all reviewed/running predecessors before
retry. A forced new attempt requires explicit disposition of retained staging
and capacity admission. Source retirement is a separate idempotent operation
over the exact captured identities, honoring import/recovery holds.

## 6. Proposed Monarr fixes

Keep the existing container-row fix and failure-path preservation. Add the
held/revision contract from §4 to polling, events, persisted acquisition state,
and all retry eligibility checks. Both transports must produce identical state
and diagnostics. Refresh the authoritative reason/path when a coarse earlier
event was incomplete; do not let that refresh trigger a second search.

Show separate facts in Activity: download bytes, current stage, reason for a
hold or failure, retained-file availability, and import result. A 100% byte
bar cannot visually imply a completed import. Offer resume only when Runner
reports it as supported, and review retained files through the existing
preview/receipt workflow. Keep operational details such as syscall names in
expandable diagnostics rather than the primary action label.

Inventory should count payloads once across aliases and generations, separate
logical bytes from allocation estimates, and distinguish partials, archives,
published media, and unverified candidates. The storage container contributes
no selectable media row. A broad import source path protects overlapping
operations while active, but cleanup authorization comes only from committed
per-file records and exact source identities.

## 7. Backlog recovery is a separate, audited operation

This sequence is proposed; no steps that mutate the live backlog were executed.

1. Record the running build identities, export queue/artifact/Monarr metadata,
   and capture a fresh manifest with job IDs, transfer IDs, generation IDs,
   paths, sizes, allocation, and current holds. This preliminary inventory is
   provisional while work continues. Redact credentials from logs.
2. Contain automatic replacement and relocation retries for affected jobs and
   volumes with the new hold mechanism. If not available yet, use an explicit
   maintenance pause with a recorded resume condition. Do not interrupt the
   user's active import; wait for committed results or acknowledged quiescence.
   Then capture a consistent metadata/manifest snapshot under the relevant
   ownership leases, revalidate file identities and receipts, and use that
   snapshot as the authority for mutation. If leases cannot be obtained, wait.
3. Reconcile every Monster operation, including reviewed staging, before any
   new copy. Select an authoritative generation using identities and full
   manifests. A same title, equal size, or apparent duplicate is insufficient.
4. Revalidate the 447 SCENE/BETTY archive candidates with set-aware PAR identity,
   restore intact files transactionally and route damaged candidates through
   the isolated repair mapping in §5.1, then extract only verified output.
   Repair episode 5's incomplete HONE data in its existing job
   if segment metadata supports it. Keep uncertain files in review.
5. Preview recognized and misnamed media through the bounded native parser,
   verify available transport/PAR integrity and complete file coverage, and
   match exact series/season/episode/copy destinations. A parser recognizing
   a container is weaker than an archive/segment integrity proof. Record the
   evidence level and reject known partials. Resolve duplicates against the
   library's committed receipts and actual destination digests.
6. Import selected verified candidates through the existing placement
   coordinator and receipt outbox. Recover partial imports per file; a failed
   HTTP acknowledgement does not undo a committed library file or permit
   source deletion. Keep existing source holds until receipts settle.
7. Propose cleanup only after no active writer/import/recovery lease remains,
   an independently retained verified copy or committed import proves the
   bytes no longer needed, and exact source identities still match. Use the
   owner application's journaled deletion workflow. Show the exact paths and
   fresh allocation estimate for review; do not infer deletion approval from
   this document request.

Do not globally clear blocklists. Any incorrect block introduced by this
incident requires release-specific provenance and a recorded correction.
True corrupt releases and unrelated failures remain blocked.

## 8. Milestones and acceptance criteria

These are implementation acceptance tests, not claims that fixes have passed.
Keep fixtures synthetic and small. Native filesystem tests must run against
the deployed mount types as well as a local filesystem.

### 8.1 M0 — preserve evidence and reproduce

Pin binary build provenance and source; preserve the four existing diagnostic
reproductions as repository tests using Runner's lockfile. Add writer/finalize,
publication retry, and overlapping-extraction reproductions from review.

**Acceptance:** each regression fails for its intended assertion on the old
revision, with no real media mutation. Capture job/event ordering in a fixture.
The mount probe reports exclusive rename unsupported before a large copy.

### 8.2 M1 — stop capacity and retry amplification

Implement typed resource holds, writer recovery checkpoints, extractor outcome
preservation, early capacity classification, and relocation attempt uniqueness.
Implement Monarr suppression and compatible wire behavior together.

**Acceptance:** injected write, sync, finalize, extract, and relocation
`ENOSPC`/`EDQUOT` hold the same job. No extra NZB, repair request, fallback
extractor, or second staging generation starts while held. Restart preserves
the hold; the recovery conditions in §5.3 resume only missing work. Ample
`statvfs` plus `EDQUOT`, unknown quota, failed health probes, and recurring
exhaustion across restart never create a retry storm. Push-before-poll,
poll-before-push, duplicates, out-of-order events, and restart all produce one
acquisition and no erroneous blocklist entry. Test old/new client combinations.

### 8.3 M2 — repair names, PAR layout, and extraction publication

Implement §5.1–§5.2 as a coherent pipeline, including leaf parser changes and
operation-owned extraction cleanup. Keep it disabled for backlog mutations
until the tests below pass.

**Acceptance:** a nested synthetic multi-episode PAR release with flat source
files verifies and extracts without unnecessary recovery blocks. Independent
PAR sets, duplicate basenames/prefixes, invalid packets, malicious paths,
symlinks, conflicting outputs, and crash-at-each-step fixtures preserve all
pre-existing bytes. Two archives sharing a parent keep both outputs and source
volumes. Subject/yEnc disagreements and resumed writers never overwrite a file.
A nested file damaged beyond its first 16 KiB repairs with sufficient parity,
while an ambiguous candidate mapping or insufficient parity remains held.

### 8.4 M3 — support publication on the actual storage

Ship the capability-tested fast path and implement the generation fallback
only with the visibility and durability prerequisites in §5.4. M1's hold is
the supported behavior until those prerequisites pass.

**Acceptance:** inject exclusive-rename `EINVAL`, `ENOSYS`, `EOPNOTSUPP`,
cross-mount `EXDEV`, source mutation, destination collision, remount, and
crashes before/after copy, fsync, registry publication, and source retirement.
Every run has at most one unresolved staging generation; existing legacy
duplicates are reported without making more. No consumer can select staging
as ready. Restart either returns the same verified published path or a hold.
No source is retired on uncertain identity or durability. Prove these on the
actual mounts before enabling the fallback there.
A successfully relocated failed PAR payload is visible in recovery preview
with failed media eligibility and never triggers ordinary automatic import.

### 8.5 M4 — recover one season, then expand

Dry-run §7 and show a per-file action list. Pilot Monster only after the active
manual import is settled. Expand to the rest of the backlog in bounded batches
only after the pilot's receipts and allocation accounting reconcile.

**Acceptance:** each intended Monster episode has a verified, committed library
record with no duplicate replacement acquisition. Unverified bytes remain
explicitly held. Cleanup, if separately authorized, removes only the approved
identities and produces durable receipts. Report before/after physical space
separately from logical payload bytes.

### 8.6 M5 — release gates and rollback

Run Runner's prescribed toolchains and `make check` (format, lint, tests, MSRV)
in a clean checkout with its lockfile. Run Monarr's documented gates:

```bash
# In the Monarr checkout, using the repository's required toolchain.
make lint
make test
make test-web
make test-e2e
```

**Acceptance:** all gates pass, affected state-machine tests pass repeatedly,
and the supported client/version matrix is recorded. Gate success is required
in addition to native-mount and crash tests; unit tests cannot prove NFS
publication semantics. Build metadata must report the deployed revision.

Roll out additive consumer support and acquisition suppression first, then
Runner holds and publication changes behind capability checks. Keep new
publication disabled until consumers and path mappings are confirmed. Back up
the databases and record schema compatibility before migration. Rollback first
quiesces jobs and disables new writes; do not start old binaries against
unrecognized generation/hold states. Retain a forward recovery path and all
source holds if database downgrade is unsupported.

## 9. Evidence checks already performed and remaining uncertainty

Four isolated tests against unchanged source failed as expected: raw subject
naming, nested PAR restoration, Linux extraction disk-full recognition, and
fallback error preservation. The harness used Rust 1.97.1 and imported source
modules from the archived checkout, but its dependency resolution was not a
full reproduction of Runner's locked release. It is diagnostic evidence only.
The harness and archived source are temporary investigation artifacts; M0
must preserve the fixtures in the owning repository.

```bash
# Diagnostic reproduction; four failures are expected on the archived source.
/Users/pjunod/.cargo/bin/rustup run 1.97.1 cargo test \
  --manifest-path /private/tmp/runner-regression-probes/Cargo.toml \
  --test reproductions -- --nocapture
```

The earlier local Monarr patch passed `make lint`, `make test`,
`make test-web` (119 tests), and `make test-e2e` (122 tests), plus shuffled
repeat tests for acquisition and the Runner adapter using Go 1.25.7. Those
results apply to that limited patch, not this unimplemented plan or unrelated
concurrent changes in the shared workspace. This document pass checks links
and whitespace; it does not repeat release gates for those other changes.

Unresolved incident facts are the exact deployed Runner build, full integrity
of retained candidates, the missing HONE extractor stderr, fresh free-space
and operation state, and final results of the user's manual import. The
generation fallback also requires proof that every deployed consumer obeys
its discovery boundary and storage durability assumptions. These are explicit
release/recovery gates; the proposal does not claim them already satisfied.
