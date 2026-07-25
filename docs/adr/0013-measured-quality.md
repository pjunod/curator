# ADR 0013 — On-disk quality is measured, not inferred from names

- **Status:** Proposed
- **Date:** 2026-07-25
- **Relates to:** ADR [0005](0005-filesystem-adoption.md) (adoption, which is
  where nameless files enter the library), ADR
  [0008](0008-distributed-execution.md) (the job queue that runs the probe
  sweep), ADR [0014](0014-target-profiles.md) (the profile model that consumes
  what this ADR measures)

## Context

A file's recorded quality currently comes from one place: parsing its
filename ([scan.go](../../internal/app/library/scan.go), the
`SetFileQuality` call). When the name carries no tokens — which is the
*normal* state of an adopted library; Plex-era names look like
`A Good Day to Die Hard.mkv` — nothing is recorded, and the UI says so:
"quality not recorded — the filename does not say."

That reads like a cosmetic shrug. It is actually a correctness bug with a
four-step chain, each step reasonable in isolation:

1. `FileQualities` omits files whose quality string is empty
   ([sqlite/acquisition.go](../../internal/infra/sqlite/acquisition.go)).
2. `BestQualityForItem` therefore reports "no known quality" for the copy.
3. `buildWanted` treats a nil current quality as **missing**
   ([wanted.go](../../internal/app/acquisition/wanted.go), `wants()`).
4. `decision.Decide` accepts any allowed release for a missing wantable.

Net effect: a 17 GB file sits on disk, and RSS sync / backlog search will
grab a "replacement" for it. Worse, the import path then sees no current
quality, judges the import *not* an upgrade, and leaves the old file in
place — the library ends with two files and no record of why.

The blueprint saw this coming and the field was never built:
[architecture.md](../architecture.md) §4.2 specifies
`MediaFile: path, size, quality, mediaInfo, releaseGroup, dateAdded`.
This ADR is that debt coming due.

Beyond the bug: a filename is a *claim*, not a fact. Release names lie
(mislabeled resolutions are routine), adopted names say nothing, and a
system whose whole job is managing quality targets cannot run on claims it
never verifies when the bytes that settle the question are sitting on its
own disk.

## Decision

### 1. `media_files` carries measured media info

Every library file gets a probed record: container, video codec, width,
height, bit depth, HDR format(s), interlacing, audio codec list, duration,
and overall bitrate (derived from size/duration). Stored as a JSON column
plus provenance fields (§3) beside the existing canonical `quality` string.

### 2. The probe is native Go, corpus-tested, with no runtime dependency

Monarr ships as a single static binary in a distroless image
([Dockerfile](../../deploy/Dockerfile) — there is no shell to exec into and
nothing on PATH). Requiring or bundling ffprobe would either break that
posture or grow the image ~60–100 MB for every deployment to serve a
header-parsing task. So:

- MKV (EBML) and MP4/M4V headers are parsed in-process. Everything §1 needs
  lives in `Segment → Info/Tracks` and `moov` respectively; a probe is a
  few bounded header reads, never a full-file scan — cheap even over NFS.
- The parser gets the same treatment as the release-name parser: a pure
  package, a committed fixture corpus (truncated real-world headers), and a
  fuzz target. It must never panic on arbitrary bytes.
- If `ffprobe` is present on PATH or named by `MONARR_FFPROBE`, it is used
  opportunistically for containers the native probe does not deep-parse
  (AVI/TS/WMV). In the stock image it never is, and nothing degrades: those
  containers simply keep filename-hint quality.

### 3. Every quality record carries provenance and confidence

`quality` remains the canonical `(source, resolution)` string, but the row
now says where it came from: `probe` · `filename` · `release` · `manual` ·
`failed`, with a confidence for inferred sources (`high`/`medium`/`low`).
Resolution is always taken from measurement when a probe succeeded — the
bytes outrank any name, always. Source (WEB-DL vs remux vs Bluray encode)
is an *inference* from measurements (lossless audio, bitrate bands,
interlacing, muxer signature), cross-checked against the filename token,
which is demoted from source-of-truth to hint: accepted only when the
measurements do not contradict it.

Release names keep their current role at *search* time — a candidate that
has not been downloaded has nothing else to judge — and import is where the
claim meets the measurement and the measurement wins.

### 4. Probing runs at import, at scan, and as a backfill — through the queue

Imports probe inline (the file was just placed; the read is local and
cheap). Scans enqueue a `library.probe` job per file that has no probe
record or whose size changed — deduped per file, retried, visible, exactly
the workload the leased queue exists for (ADR 0008 step 1), and ready for
mount-capability routing when multi-host lands (step 3). The backfill for
an existing library is not a special mechanism: it is the first scan after
upgrade doing what scans now do.

### 5. Unknown is never missing, and a guess never evicts a file

Two rules the decision paths must obey from now on:

- **A file whose quality could not be determined still exists.** "On disk,
  quality unverified" is a distinct state from "missing" everywhere a
  decision is made — the wanted index, the decision engine, the UI. After
  this ADR the state is rare (a probe always yields at least resolution;
  only unreadable files fail), but rare is not never.
- **Don't churn.** When the measured resolution meets the profile's target
  and only the *source* is unverified, the target counts as met, badged
  "source unverified" in the UI. Auto-replacing a possibly-perfect file on
  a guess is the one unforgivable move for a tool sharing a disk with a
  user's collection. The escape hatch already exists and stays manual:
  interactive search grabs whatever the user picks, no decision gate.

## Consequences

- The re-download chain in Context dies, and with it the two-files-no-story
  failure. A regression test pins it by name.
- The deployment story is untouched: same binary, same image, no new
  dependency. The cost moved into our code — an EBML walker and an MP4 box
  walker we now own, fuzz, and maintain. That is the same trade the
  release-name parser made, and it has paid for itself in testability.
- Source inference will sometimes be wrong or shrug. The design absorbs
  this: confidence is stored and shown, low-confidence inference never
  triggers replacement (§5), and resolution — the axis that dominates
  ranking — is measured fact, not inference.
- Probing adds I/O to scans. Bounded header reads (single-digit MiB per
  file, once, cached by size) keep a multi-TB NAS library tractable; the
  queue paces it and survives interruption.
- The UI can finally say something true and useful: measured facts
  ("1080p · HEVC · HDR10 · TrueHD · 23 Mbps") instead of an apology.

## Alternatives considered

- **Bundle ffprobe.** Battle-tested against every container ever made, and
  the obvious move. Rejected: +60–100 MB on every image, abandons
  distroless/static — a posture chosen deliberately in Phase 0 — and buys
  generality Monarr does not need (media libraries are overwhelmingly
  MKV/MP4; the long tail keeps hint-quality and loses nothing it has
  today).
- **Require an external ffprobe, feature off without it.** Zero image cost,
  but the fix silently does not apply on default deployments — the exact
  population with the problem.
- **libmediainfo via cgo.** Ends the pure-Go static build; worst of both.
- **Do nothing / parse harder.** No amount of filename cleverness recovers
  information the name does not contain.
