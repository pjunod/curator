# ADR 0021: Durable Runner recovery handoff

- **Status:** implemented, verification pending
- **Date:** 2026-09-25
- **Related:** [ADR 0013](0013-measured-quality.md), [settings](../settings.md),
  [usage](../usage.md)

## Context

A downloader History row can disappear while retained media remains. Recovery
must keep those bytes attributable, let the operator choose their library target,
and prevent cleanup from treating a pending HTTP response as completed deletion.
The approved scope includes both this fix and robust ordinary/recovery placement.

## Decision

Runner owns its source and recovery staging lifecycle. Curator owns its
configured working trees, including `/working/monarr/completed`, even when
Runner downloads into them. As clarified by the operator on 2026-09-29, Curator
may clean successfully imported payloads within that ownership boundary.
Runner-owned processing and recovery storage remain outside local fallback. Only queue HTTP 404 permits a
History fallback; pending, hold, authentication and transport failures retry.

Recovery reads only an explicitly configured published directory mounted read-only,
separate from library and completed-download mappings. Curator checks each selected
SHA-256 digest, uses its bounded native parser and previews exact title/copy/episode
choices and destination filenames. Recognition is not proof of completeness.
Books are outside the initial recovery contract.

All ordinary and recovery imports use one placement coordinator. Before filesystem
publication, persist the source/target, digest, previous target digest, temporary
name, rollback name and metadata intention. New bytes are flushed before rename.
An existing same-name target retains a rollback link until metadata commits.
Recovery forces independent copies even when a hardlink would be possible.

The SQLite transaction commits file metadata, copy/episode associations, measured
quality/provenance, placement completion and the recovery file result together.
FULL synchronization applies to durable receipts. Reconciliation may finish a
published placement; it never republishes an old pending intent over a newer file.
Verified temporary and rollback files are cleaned after the commit.

A database revision detects library, profile, root, copy and episode changes during
a recovery copy. The metadata transaction compares the accepted revision and
advances it with its own writes. The initial revision is global, deliberately
conservative: an unrelated library edit can require a fresh recovery preview.
The coordinator serializes imports while network downloading stays concurrent.

A persisted import and receipt outbox survive browser closure and daemon restart.
Final delivery acknowledgement and local completion commit in one transaction.
Runner receives only results from committed per-file records. Partial results and
consumer silence retain source holds. Informational claim heartbeats never grant
permission for cleanup. Cancellation signals the worker and acknowledges quiescence
only after active filesystem work returns; completed library files remain recorded.

## Consequences

The original, staging and library copy can coexist. Capacity checks reserve full
copy size plus 1 GiB on each relevant destination volume. A same-volume replacement
backup shares its old inode until commit. Operators must account for old content
and other volume users; a preview is not a space reservation.

Ownership and placement journals add durable state and write latency. They replace
ambiguous success with visible pending/review outcomes. Keep and conservative
failure handling can extend disk use until an operator resolves a problem.

Settings → Dev provides an enable control and advisory readiness information.
Enablement is always available. Actual operations still reject stale revisions,
unsafe paths, changed files or missing ownership. Automatic status refresh is an
operator preference; explicit recovery and committed receipt delivery remain live.

## Verification

The implementation adds rollback/receipt atomicity, restart reconciliation,
changed-target/digest, native recovery and cancellation regression cases.
Final adversarial review and the repository gates must pass before merge.

## 2026-09-30: durable Runner holds and retained observations

An active acquisition stores Runner control/revision facts independently of
transfer progress. Held episode/season overlap is checked under the final
acquisition admission lock. Resume addresses the existing client handle and
waits for a newer same-instance fact before clearing suppression. Migration
0037 rollback refuses any stored control facts to avoid silently losing custody.

Retained-file dry-run plans are provisional during overlapping active work.
They bind source identity and SHA-256 and observe the library revision and
pending placement receipts. Revalidation still grants no mutation lease.
Placement, receipt acknowledgement and identity checks remain the only path
to separately authorized import and cleanup.

Control revisions order custody transitions independently of progress. An
identical running revision accepts fresh transfer and stage facts; stale or
conflicting facts retain the fence. Terminal history and completion events
carry the last authoritative `*Control:v1` param to survive missed resume events
and queue retirement without interpreting legacy SUCCESS as hold resolution.
