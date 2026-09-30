# Runner failures — Monster downloads and retained media

**Status:** diagnosed, not fixed or deployed · **Inspected:** 2026-09-30

Companion to [usage](usage.md#completed-folders--accounting-survives-an-empty-queue).
The [root-cause and fix plan](RUNNER_FAILURE_FIX_PLAN.md) turns this evidence
into proposed changes, milestones, and recovery gates; its
[adversarial review](RUNNER_FAILURE_REVIEW.md) records additional source hazards.
This records the Runner defects behind the Monster failures and accumulated
download storage. The earlier Monarr changes hide the storage container and
preserve newly reported failure paths; they do not repair these Runner defects.

The inspected host is `nuc3`. Runner's deployed source checkout is commit
`ab25d0cbecfdb5adf299072813a2948383192d9f`. Its tracked source was copied to
`/private/tmp/runner-diagnosis-ab25d0c` for inspection. The running binary reports
`0.2.0+unknown`, so that source revision is the host checkout, not independently
verified binary provenance. Observations below also use the live logs, files,
operation journal, and filesystem probes.

## 1. PAR renaming cannot restore catalog subdirectories

Monster's SCENE/BETTY PAR catalog names 447 files under episode directories,
such as `Monster.The.Ed.Gein.Story.S01E08.2160p.WEB.h265-BETTY/`.
All 447 corresponding flat filenames exist in the download directory, while
the catalog paths do not. Four sampled files match both the catalog size and
the MD5 of their first 16 KiB. This is not a full integrity check of the season.

`crates/nzbd-post/src/rename.rs:70` attempts `rename(from, to)` without creating
the catalog's parent directories. The live log records those renames failing
with `No such file or directory`, followed by a request for thousands of PAR
blocks. The extracted episode files remain, but the job ends as `PAR_FAILURE`.

An isolated reproduction creates a real PAR set containing a nested file,
flattens that file, and calls Runner's unchanged `par_rename`. The expected
restoration fails. Repair must handle validated relative catalog paths and
their parents without following symlinks or allowing traversal. Recursive
verification and extraction must agree with any restored directory layout.

## 2. Raw NZB subjects become unusable media filenames

`crates/nzbd-nzb/src/lib.rs:58` extracts a filename only when the subject has
double quotes. Otherwise it returns the entire subject. The queue sanitizes
that text and uses it as the output filename. The yEnc decoder has a filename,
but `crates/nzbd-engine/src/pool.rs:426` passes only bytes, offsets, size, and
CRC to the writer; the decoded filename never corrects the original hint.

The retained tree has 65 large candidates with `.mkv` embedded in a longer
Usenet subject, totaling 259.4 GiB. Sampled headers are Matroska/EBML. Neither
the header nor the apparent file size establishes that every segment exists.
Monarr's normal extension filter skips these names.

An isolated test using the observed bracketed subject form reproduces the
parser returning the subject rather than the media filename. Repair needs
safe filename selection, collision handling, and consistent persisted names;
blindly renaming all old files is not an integrity check.

## 3. Network mounts reject final publication of relocated files

`crates/nzbd-state/src/artifacts/fs.rs:410` calls Linux `renameat2` with
`RENAME_NOREPLACE`. Both relevant mounts return `EINVAL` for that operation,
even when the target does not exist. This was reproduced with unique empty
temporary directories on each mount; ordinary rename succeeds there.

The cross-mount rename returns `EXDEV`, so Runner begins its copy. After the
copy and verification, `artifacts/relocation.rs:146` calls the unsupported
exclusive rename again to publish the staging directory. That fails, retaining
both the original and the staging copy. The operation journal records the
Monster moves in `review` with `filesystem: Invalid argument (os error 22)`.

At inspection, three failed Monster staging copies occupied 250.5 GiB in
`/processing/failed`, and another automatic attempt had already copied
38.5 GiB. The combined 289.0 GiB is additional to the retained originals on
the download volume. The running attempt's size can change.

Small probes confirmed that directory and file fsync work on the tested paths.
The copy syscall returned `EXDEV`, and a normal read/write copy succeeded.
The directly reproduced publication failure is the exclusive rename, not
proof of a `copy_file_range` failure.

Repair needs a publication strategy that preserves no-overwrite guarantees
on these mounts, detects unsupported operations before copying a large job,
and reconciles existing journaled staging copies before starting another.
An unchecked ordinary rename is not an equivalent safe replacement.

## 4. Extraction disk exhaustion becomes a generic release failure

At 15:20:32 UTC, Runner logged just 22,020,096 bytes (21 MiB) free on the
download volume while unpacking Monster's first HONE copy. At 15:20:45 it
logged `unpack failed`; after staging cleanup it reported approximately
33.4 GiB free and resumed downloads. Monarr later received `UNPACK_FAILURE`
and grabbed a replacement. That replacement hit an explicit write/finalize
`ENOSPC` on episode 5 at 15:39 UTC. Its unfinished `.mkv.part` remains.

Three code defects weaken the diagnosis and recovery:

- `tools.rs:528` recognizes `There is not enough space`, but misses Linux's
  `No space left on device` message.
- `tools.rs:453` prefers the first extractor outcome, which can erase a
  disk-space error reported by the fallback extractor.
- `manager.rs:1499` does not use `disk_space_error` when handling the failure.
  It records a generic unpack failure and removes staging. The disk flag has
  no production consumer in the inspected source.

Two isolated tests reproduce the missing Linux diagnosis and the lost fallback
error. The exact HONE extractor stderr was not retained in the available log,
so attributing that individual unpack failure to ENOSPC is supported by the
simultaneous low-space event, rather than a preserved extractor error message.
The replacement's episode-5 ENOSPC is explicit.

`unrar` is absent in the container and the configured command is `unrar`.
The Dockerfile intentionally relies on 7-Zip as fallback. Its absence alone
does not explain a failed release; the fallback warning obscures which tool
actually ran.

Repair must distinguish environment failures from corrupt releases, retain
the useful diagnosis, and allow post-processing to resume after capacity
recovers instead of triggering a fresh season download.

## Verification and scope

The isolated harness at `/private/tmp/runner-regression-probes` imports the
unchanged deployed-source modules directly. Four tests assert the expected
behavior and all four fail, reproducing the defects above. It used Rust
1.97.1; this is focused diagnostic evidence, not a full Runner release gate.

```bash
/Users/pjunod/.cargo/bin/rustup run 1.97.1 cargo test \
  --manifest-path /private/tmp/runner-regression-probes/Cargo.toml \
  --test reproductions -- --nocapture
```

Filesystem publication was reproduced on the actual mounts with tiny,
uniquely named diagnostic directories, which were removed afterward.
No media, queue records, configuration, or running import was changed.
No Runner fix was deployed, and no retained payload was declared complete.
