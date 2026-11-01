# ADR 0025 — A content digest is remembered for its file generation

**Status:** Accepted · **Date:** 2026-11-01

## Context

Curator proves bytes before it trusts them: a Runner manifest file is hashed
before import and again just before placement (ADR 0021), a placement hashes
its source and target, and redundancy and cleanup proofs hash the library
file they compare against. Each of those reads was independent and nothing
was remembered between attempts.

Measured on nuc3, 2026-11-01: proving one 2.9 GB duplicate redundant took
250–280 s against 31 s for a real 2.5 GB import, because the same bytes were
read three or four times over network mounts per attempt; a receipt retried
a thousand times had re-read its payload thousands of times; and a 31 GB
cleanup that re-hashed both copies held an import worker for over twenty
minutes.

## Decision

A SHA-256 computed by reading a file in full is remembered in process,
keyed by the file's device and generation (inode, size, mtime and ctime to
the nanosecond). A later digest of the same generation is answered without
reading. Every caller keeps its own identity checks (same file, unchanged
generation, no symlinks); the memo answers "what are these bytes", never
"may this file be trusted".

A digest is remembered only when the file's ctime is older than the moment
hashing began by at least a three-second margin — git's "racily clean" rule.
On a filesystem with coarse timestamps a same-size rewrite within one tick
could leave every generation field equal; a file that changed that recently
is re-read every time until it settles. ctime cannot be set by an ordinary
write or `utimes`, and a replaced file has a new inode.

The memo is bounded (8,192 generations, oldest evicted first) and not
persisted: a restart costs one read per file, as before.

Payload cleanup runs on its own worker, separate from the two import
workers, so cleanup verification can never occupy an import slot.

## Consequences

- A delivery's payload is read once per process lifetime while it is
  unchanged, however many verify, recheck, retry, redundancy and cleanup
  passes it goes through.
- Integrity guarantees are unchanged: any write produces a new generation and
  a fresh read; tampering between verification and placement is still caught.
- Platforms without inode/ctime identity (neither Linux nor macOS) keep the
  previous always-read behaviour.
