# Quality truth — implementation plan

**Status:** BUILT 2026-07-25 (v0.6.0) — kept as the record of how, and of
the two rules §9 did not anticipate (ADR 0014 §7) · **Executes:** ADR
[0013](adr/0013-measured-quality.md) + ADR [0014](adr/0014-target-profiles.md)
· **Written:** 2026-07-25 · **Ledger:** STATUS.md "Phase 6 — Quality truth"

Read the two ADRs first — they carry the *why* and the rules; this file
carries the *how* and the order. Work milestone by milestone (§8), checking
off the STATUS.md Phase 6 items as each lands. Every code citation below
was verified against the tree on 2026-07-25 — **re-verify each one at build
time**; cite-by-name, not by line, is deliberate. Standing instruction: if
a step seems to require changing something §9 forbids (the `Rank` encoding,
the compat wire shapes, the stored `Source` strings), stop and flag it
instead of improvising.

---

## 1. The two failures this ships against

**Failure A — the screenshot.** An adopted movie (`A Good Day to Die
Hard.mkv`, 17 GB) shows "quality not recorded — the filename does not say"
and profile "Any — upgrades until WEB-DL 1080p, then stops". The system has
the bytes and refuses to look at them; the profile line requires an apology
comment in the UI to explain.

**Failure B — the silent one.** That same item is in the wanted index as
*missing* right now. The chain, each link verified:

1. `scanItem` ([app/library/scan.go](../internal/app/library/scan.go))
   records quality only when the filename parse yields something:
   `if q := parser.Parse(...).Quality; q.Resolution != 0 || q.Source !=
   quality.SourceUnknown { SetFileQuality(...) }` — otherwise no record.
2. `FileQualities` ([infra/sqlite/acquisition.go](../internal/infra/sqlite/acquisition.go))
   skips empty quality strings; `BestQualityForItem` then returns
   `ok=false` — "no known quality" and "no files" are indistinguishable.
3. `buildWanted` ([app/acquisition/wanted.go](../internal/app/acquisition/wanted.go)):
   `wants := func(q *quality.Quality) bool { if q == nil { return true } … }`
   — nil means *missing*, so the item is hunted.
4. `decision.Decide` ([domain/decision/decision.go](../internal/domain/decision/decision.go)):
   `CurrentQuality()` not ok → `Decision{Accepted: true}` — any allowed
   release is grabbed by RSS sync, backlog search, or Auto search.
5. On import, `importMovieFile`
   ([app/acquisition/import.go](../internal/app/acquisition/import.go))
   sees `have=false`, judges it not an upgrade, and **does not** call
   `removeExistingFiles` — the 17 GB original stays. Two files, no story.

**Failure C — the profile asymmetry.** `searchAndGrabBest`
([app/acquisition/automation.go](../internal/app/acquisition/automation.go))
takes the best accepted release by `quality.Rank`, unbounded above; the
cutoff only stops later upgrades. "Any" grabs a 2160p remux for a missing
item it would have declared finished at WEB-DL 1080p.

---

## 2. Contract — what exists today (re-verify at build time)

**Quality model** — [domain/quality/quality.go](../internal/domain/quality/quality.go):

```go
type Quality struct { Source Source; Resolution int }   // String() "webdl-1080"
func Rank(q Quality) int  // resolutionRank*16 + sourceRank — DO NOT CHANGE
type Profile struct { ID int64; Name string; Allowed []Quality
                      Cutoff Quality; UpgradesAllowed bool }
func (p Profile) IsAllowed(q Quality) bool   // unknown source matches on res
func (p Profile) MeetsCutoff(q Quality) bool // Rank(q) >= Rank(p.Cutoff)
func DefaultProfiles() []Profile             // ids 1-5, seeded in migration 0003
```

**Decision engine** — [domain/decision/decision.go](../internal/domain/decision/decision.go):
`Decide(q quality.Quality, w domain.Wantable, profile quality.Profile)
Decision`, pure; rejection codes `unmonitored · quality_unknown ·
quality_not_allowed · not_an_upgrade · already_at_cutoff ·
upgrades_disabled`, surfaced only in Monarr's own UI.

**Wantables** — [domain/wantable.go](../internal/domain/wantable.go):
interface `ID() / MediaItemID() / ProfileID() / Monitored() /
CurrentQuality() (quality.Quality, bool)`; implementations Movie, Episode,
Season (worst-episode current), Book. Constructed by `target`/`targetCopy`
and `buildWanted` in app/acquisition.

**Storage** — [infra/sqlite/acquisition.go](../internal/infra/sqlite/acquisition.go):
`SetFileQuality(ctx, fileID, q)`, `BestQualityForItem(ctx, itemID, copyID)
(quality.Quality, bool, error)`, `FileQualities(ctx, itemID)
map[int64]quality.Quality` (omits empty). `quality_profiles.definition` is
JSON `{"allowed":[...],"cutoff":{...}}` read by `profileFromRow`.
Migrations run through `0017_manual_entries.sql`; **next free number is
0018**. All tables STRICT; goose up-only; queries via sqlc
(`infra/sqlite/queries/*.sql` → `make gen`), wrapper methods on
`*sqlite.DB` (generated code stays package-private).

**Job queue** — [domain/job.go](../internal/domain/job.go) +
[infra/jobs/jobs.go](../internal/infra/jobs/jobs.go): `Job{Kind, Payload
(JSON string), Priority (lower first), DedupeKey, RequiredCapability,
AffinityNode, MaxAttempts}`; `Queue.Register(kind, handler)` before Start;
`EnqueueUnique` coalesces on DedupeKey. Library job kinds + registration:
[app/library/jobs.go](../internal/app/library/jobs.go) (`library.scan`,
`library.adopt`).

**Import** — [app/acquisition/import.go](../internal/app/acquisition/import.go):
per-file `parser.Parse(base)` with fallback `q = dl.Quality` (the grabbed
release's parsed quality); `place()` hardlink-or-copy; `UpsertFile` +
`SetFileQuality`.

**API** — `ListProfiles` in
[api/acquisition_handlers.go](../internal/api/acquisition_handlers.go)
serves `apigen.QualityProfile{Id, Name, Cutoff (display string),
UpgradesAllowed, Qualities}`. Spec-first: edit
[api/openapi.yaml](../internal/api/openapi.yaml), then `make gen`
(oapi-codegen v2.8.0 pinned). **Gotcha (repo history):** release-search
marshals apigen DTOs directly — any new field on internal structs that
reach the API must ALSO be added to the openapi schema or it silently
drops.

**Compat** — [compat/](../internal/compat/): `/sonarr` + `/radarr`
personalities serve `/qualityprofile` lists and `qualityProfileId` on
items; the in-process fake-consumer suite (`compat_test.go`,
`contract_test.go`) replays Jellyseerr/Prowlarr/Bazarr flows and asserts
wire shapes. Those tests define the compatibility bar for §7.6.

**UI** — [web/src/pages/MediaDetail.tsx](../web/src/pages/MediaDetail.tsx):
`QualityFacts` renders the on-disk pill / "quality not recorded" fallback;
the Profile row renders "— upgrades until {target}, then stops" with the
apology comment. `EditPanel` + `CopiesPanel` + Library mass editor select
profiles by id. [web/src/pages/SettingsAcquisition.tsx](../web/src/pages/SettingsAcquisition.tsx)
currently has no profile section.

---

## 3. Shape of the change

```
                       ┌────────────────────────────┐
 release name ────────▶│ parser.Parse (unchanged)   │──▶ candidate Quality
 (search time: claims) └────────────────────────────┘        │
                                                             ▼
                       ┌────────────────────────────┐   decision engine
 file on disk ────────▶│ mediainfo.Probe (NEW)      │──▶ (target rules,
 (import/scan: facts)  │ + source inference          │    ADR 0014 §2)
                       │ + filename cross-check      │        │
                       └────────────────────────────┘        ▼
                          resolution: measured, wins     grab / met /
                          source: inferred + confidence  don't-churn
                          provenance recorded
```

The asymmetry is the design: candidates are judged by their names (nothing
else exists pre-download), disk is judged by measurement, and import is
where a claim meets the bytes — the bytes win.

---

## 4. The probe (`internal/domain/mediainfo`, new package)

Pure package, stdlib-only imports (arch_test enforces domain arrows), same
discipline as `domain/parser`: corpus fixtures + fuzz, must never panic.

### 4.1 API and record

```go
// Probe reads container/stream headers from r (never the whole file).
// size is the file size on disk (bitrate derivation). It returns partial
// info with Err set rather than failing wholesale where it can.
func Probe(r io.ReaderAt, size int64) (Info, error)

type Info struct {
    Container   string   // "mkv" | "mp4" | "unsupported:<ext>"
    Video       *VideoInfo
    Audio       []AudioInfo
    DurationMS  int64
    BitrateKbps int64    // size*8 / duration — overall, not per-track
    WritingApp  string   // MKV MuxingApp/WritingApp, "" elsewhere
}
type VideoInfo struct {
    Codec      string   // "h264" | "hevc" | "av1" | "mpeg2" | "vc1" | …
    Width, Height int
    BitDepth   int      // 8 default; from hvcC/avcC when present
    HDR        []string // "hdr10" | "hlg" | "dv" (any subset)
    Interlaced bool
}
type AudioInfo struct { Codec string; Channels int } // "truehd","dtshd?→dts",
                                                     // "eac3","ac3","aac",
                                                     // "flac","pcm","opus","mp3"
```

**MKV:** EBML header → Segment; follow SeekHead to `Info` (Duration ×
TimestampScale, MuxingApp/WritingApp) and `Tracks` (per TrackEntry:
TrackType, CodecID, Video{PixelWidth, PixelHeight, FlagInterlaced,
Colour{TransferCharacteristics: 16→hdr10, 18→hlg}, BlockAdditionMapping
dvcC/dvvC→dv}, Audio{Channels}). Bit depth from CodecPrivate where the
codec record carries it (hvcC: `bitDepthLumaMinus8`). Cap total bytes read
at 8 MiB; skip Tags/Attachments/Clusters.

**MP4/M4V:** top-level box scan (moov is often at EOF — that is why the
API takes ReaderAt, not Reader); `mvhd` duration/timescale; per `trak`:
`hdlr` vide/soun, `stsd` entry (avc1/hvc1/hev1/av01 width+height; avcC/hvcC
bit depth; `colr` nclx transfer 16/18; dvcC/dvvC → dv; audio mp4a→aac,
ec-3→eac3, ac-3→ac3, dtsc→dts, …). Read moov fully, cap 32 MiB.

**Other containers** (avi/ts/wmv): `Container = "unsupported:…"`, no deep
parse. If `MONARR_FFPROBE` names a binary or `exec.LookPath("ffprobe")`
hits (it never does in the stock distroless image), shell out with
`-print_format json -show_streams -show_format` and map the same fields.
Absent that, the file keeps filename-hint quality — same as today, and
provenance says so.

**Fixtures:** `testdata/mediainfo/` — committed header-window truncations
of real files (first N KiB of an MKV/MP4 parses identically to the whole
file for everything above). Cover: 1080p x264 8-bit, 2160p HEVC HDR10,
HEVC DV profile 8, HLG, TrueHD+Atmos audio, DTS, EAC3-only web file,
interlaced mpeg2, scope (2.39:1) 1920×800, moov-at-end MP4, and a
garbage-bytes fuzz seed. A `corpus_test.go` mirrors the parser corpus
harness: table of fixture → expected Info.

### 4.2 Resolution tier — measured, absolute

Scope crops make height alone a trap (1920×800 is 1080p content). Tier by
width-or-height, first match:

| tier | rule |
|---|---|
| 2160 | width ≥ 3000 **or** height ≥ 1600 |
| 1080 | width ≥ 1700 **or** height ≥ 900 |
| 720  | width ≥ 1150 **or** height ≥ 650 |
| 480  | anything smaller with height > 0 |

When a probe succeeded, this tier IS the file's resolution — filename
tokens never override it (they only ever break ties the probe cannot see,
which for resolution is none).

### 4.3 Source inference — signals in, verdict + confidence out

Named constants, one table, corpus-tested (`inference_test.go`, table of
measurements → verdict). Bands are overall-bitrate Mbps, per tier —
starting values, expected to be tuned against the corpus:

| tier | remuxFloor | bdEncode band | web band |
|---|---|---|---|
| 2160 | 40 | 15–40 | 4–30 |
| 1080 | 20 | 6–20  | 2–12 |
| 720  | 10 | 3–10  | 1–6  |
| 480  | 8  | 2–8   | 0.5–4 |

Audio families: lossless = truehd/flac/pcm/mlp · disc-family = dts ·
web-family = eac3/aac/opus · neutral = ac3/mp3.

Rules, first match wins:

1. `Interlaced` → **hdtv**, high.
2. Lossless audio present **and** bitrate ≥ remuxFloor → **remux**, high.
3. Lossless audio present, bitrate below remuxFloor → **bluray**, medium
   (an encode that kept the disc audio).
4. `WritingApp` contains "MakeMKV" → **remux**, high.
5. Bitrate ≥ remuxFloor → **remux**, medium.
6. Audio all web-family/neutral and bitrate in web band → **webdl**,
   medium (low if only neutral audio).
7. Bitrate in bdEncode band → **bluray**, low.
8. Otherwise → **unknown**.

**Filename cross-check** (the demotion of names to hints): parse the
basename as today. Token resolution: discarded, always — measured wins.
Token source `T` is *contradicted* when: `T=remux` with no lossless audio
and bitrate < remuxFloor; or `T∈{webdl,webrip}` with lossless audio
present. Pick order for the recorded source:

1. measured verdict at **high** confidence;
2. else uncontradicted token `T` (provenance `filename`);
3. else measured verdict at medium/low;
4. else **unknown**.

Recorded quality = `(picked source, measured tier)`; provenance = `probe` |
`filename` (token won) | `release` (import-time claim won, §6.2) |
`manual` | `failed` (unreadable file); confidence stored for inferred
sources.

---

## 5. Target profiles (`domain/quality` rework)

Per ADR 0014. `Profile` becomes:

```go
type Profile struct {
    ID              int64
    Name            string
    Target          Quality   // the point of the profile
    Floor           *Quality  // nil = no floor
    UpgradesAllowed bool
}
// The three predicates — the entire decision vocabulary from now on:
func (p Profile) Met(current Quality, sourceVerified bool) bool
func (p Profile) Acceptable(release Quality) bool
func (p Profile) Upgrade(release, current Quality, sourceVerified bool) bool
```

Exact rules in ADR 0014 §2 — implement them as written; the ADR text is
the spec and the table-driven test enumerates the lattice (missing / below
/ at-res-below-source / at / above target × floor / no floor × verified /
unverified source × books at resolution 0).

**Decision engine:** `Decide` keeps its signature but speaks the new
vocabulary; codes become `unmonitored · quality_unknown ·
below_floor · above_target · not_an_upgrade · target_met ·
upgrades_disabled · on_disk_unverified`. The last is new: a wantable whose
files exist but whose quality could not be measured (probe `failed`) is
**rejected for automation** — on disk beats guessing (ADR 0013 §5) — while
interactive search still lists candidates with that reason attached and
manual grab stays ungated (verified: `Grab` has no `Decide` gate).

**Wantable gains `OnDisk() bool`** (files present regardless of measured
quality) on all five implementations; `CurrentQuality` keeps meaning "best
*known*". Episodes get it from the `HasFile` flag already hydrated on
`domain.Episode`; movies/books from the disk-state query below; a season is
on-disk when every episode is.

**Storage:** replace `BestQualityForItem`'s ambiguous `ok` with

```go
type CopyDiskState struct { HasFiles bool; Best *quality.Quality }
func (d *DB) DiskStateForItem(ctx, itemID, copyID int64) (CopyDiskState, error)
```

and thread it through `target`/`targetCopy`/`buildWanted`/`episodeQualities`.
`wants()` in `buildWanted` becomes: no files → wanted (missing); files with
no measurable quality → **not** wanted; otherwise `UpgradesAllowed &&
!profile.Met(...)`. This line is the churn fix — pin it with
`TestUnknownQualityOnDiskIsNotHunted` (movie + episode variants).

**Migration 0019** rewrites `quality_profiles.definition` in place to
`{"target":{"source":…,"resolution":…},"floor":{…}|null}` per the ADR 0014
§4 table, renaming ids 1 and 3 (`Any`→`1080p`, `Ultra-HD`→`4K`).
`upgrades_allowed` column unchanged. `profileFromRow` reads only the new
shape (up-only migrations; no dual-format reader).

---

## 6. Pipeline wiring

### 6.1 Scan → probe jobs

New job kind in [app/library/jobs.go](../internal/app/library/jobs.go):

```go
JobProbe = "library.probe"   // payload {"fileId":N}, dedupe "probe:<fileId>",
                             // priority 80 (below scan 50 / adopt 60)
```

`scanItem` enqueues one per file where `media_info` is empty **or** the
stored size changed (upsert already tracks size). Handler =
`Service.ProbeFile(ctx, fileID)`: open, `mediainfo.Probe`, run inference +
cross-check (§4.3), write `media_info` + provenance + `quality`, invalidate
the wanted index via the existing bus event. Unreadable → provenance
`failed`, quality untouched, job succeeds (a corrupt file is a result, not
a retryable error). No queue configured → probe inline during scan, same
degradation `EnqueueScan` already documents. **The backfill is not a
mechanism** — it is the first post-upgrade scan doing the above over every
file.

### 6.2 Import probes inline

In `importDownload`, after `place()`: probe the placed file. Recorded
quality = §4.3 pick, with the release-name claim (`p.Quality` /
`dl.Quality`) treated as one more token-hint — provenance `release` when
it wins. On resolution mismatch (claimed 2160, measured 1080): log warn +
`AddHistory(ctx, "quality_mismatch", …)` and record the *measured* truth.
Import proceeds — the file is what it is; if it now sits below target, the
wanted index re-hunts honestly. (Auto-blocklist on mismatch: deliberately
not now — §9.)

The per-file upgrade check in `importMovieFile`/`importEpisodeFile`
switches to the same `DiskStateForItem`/`Met` vocabulary, comparing against
measured current quality.

### 6.3 API + UI

- Profile CRUD: `POST/PUT/DELETE /profiles` (delete → 409 while any
  item/copy/import-list references it), openapi + `make gen`.
  `QualityProfile` DTO becomes `{id, name, target, floor?,
  upgradesAllowed, sentence}` — `sentence` is the server-rendered "hunts
  the best release up to WEB-DL 1080p, then stops", so every client says
  exactly the same true thing. Editor UI in Settings → Acquisition
  (target = resolution + source pickers; floor optional; live sentence
  preview).
- `MediaDetail` `QualityFacts`: on-disk pill from measurement ("1080p ·
  HEVC · HDR10 · TrueHD · 23 Mbps"), provenance badge (`measured` ·
  `from filename` · `from release` · `unverified`), and the don't-churn
  state reads "at target (source unverified)". The apology comment on the
  Profile row is deleted along with the reason for it. Files table gains
  quality + provenance columns. "quality not recorded — the filename does
  not say" survives only for provenance `failed`, reworded: "unreadable —
  monarr could not parse this file".
- Wanted page: unchanged shape, now truthful.

### 6.4 Compat

`/qualityprofile` responses synthesize allowed-items + cutoff from the
target (ADR 0014 §6): items = every known quality with `res ≤ target res`
(≥ floor when set), cutoff = the target, mapped onto the *arr quality-id
vocabulary the shim already emits. Ids/names pass through. The
fake-consumer suite is the acceptance bar and must not need edits to pass
— if it does, the shim changed shape and that is a §9 stop-and-flag.

---

## 7. Schema summary

**Migration 0018** (`0018_media_info.sql`):

```sql
ALTER TABLE media_files ADD COLUMN media_info         TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN quality_provenance TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN quality_confidence TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN probed_at          INTEGER NOT NULL DEFAULT 0;
```

(`quality` stays the canonical string; STRICT tables — mind the types.)
**Migration 0019** (`0019_target_profiles.sql`): the §5 rewrite. sqlc
queries updated for both; `make gen`; wrapper methods on `*sqlite.DB`.

---

## 8. Milestones — each ends with a runnable check

**M1 — the prober.** `domain/mediainfo` with MKV + MP4 walkers, fixtures,
fuzz seed.
*Accept:* `go test ./internal/domain/mediainfo/...` green with the §4.1
fixture list covered; `go test -fuzz=FuzzProbe -fuzztime=30s` clean;
`internal/arch_test.go` still green (stdlib-only imports).

**M2 — storage + probe wiring.** Migration 0018, `DiskStateForItem`,
`library.probe` jobs + inline import probe, scan enqueue.
*Accept:* e2e: adopt a fixture-backed tokenless file → item page shows
measured quality with `measured` badge after the scan's jobs drain;
`make test` green.

**M3 — inference + cross-check.** §4.3 rules, provenance/confidence
recorded, release-claim reconciliation on import + `quality_mismatch`
history event.
*Accept:* `inference_test.go` table green (every rule + every
contradiction case exercised); import e2e shows provenance `release` when
the name wins and `probe` when measurement outranks it.

**M4 — target profiles + the churn fix.** §5 whole: domain rework,
migration 0019, decision engine, wanted index, `OnDisk()`.
*Accept:* `TestUnknownQualityOnDiskIsNotHunted` (the named regression for
Failure B) green; profile predicate lattice test green; full-loop e2e
(delete file → rescan → wanted → RSS re-grab → import) still green — it
proves genuinely-missing items still hunt.

**M5 — API, UI, compat.** §6.3 CRUD + editor + QualityFacts/Files
rendering; §6.4 shim synthesis.
*Accept:* fake-consumer suite green untouched; Playwright e2e: create a
profile in the editor, assign via mass editor, sentence renders on the
item page; `make lint` zero findings.

**M6 — truth in the docs.** usage.md (what the badges mean),
settings.md (`MONARR_FFPROBE`), architecture.md §4.2 MediaFile note,
ADRs 0013/0014 → Accepted, STATUS.md Phase 6 checked off, corpus counts
updated.
*Accept:* docs changed in the same commits as behavior; STATUS snapshot
line current.

Order matters: M1→M2→M3 before M4 (don't-churn needs provenance), M4
before M5. M6 rides along each milestone and closes at the end.

---

## 9. Non-goals — guardrails, each with its reason

- **No ffprobe/ffmpeg requirement, bundling, or image change.** The
  distroless static-binary posture is a Phase 0 decision this plan works
  *within* (ADR 0013 §2). `MONARR_FFPROBE` is opportunistic enrichment
  only.
- **No full-file reads, ever.** Header windows with hard byte caps (§4.1)
  — a probe sweep over a multi-TB NFS library must stay cheap.
- **No bitrate-as-upgrade-axis.** Bitrate feeds source inference and the
  UI pill, never `Upgrade()` — "better bitrate" is a treadmill with no top.
- **No auto-blocklist / auto-re-search on quality mismatch.** Log +
  history now; an automated punishment loop needs its own design once
  mismatch data exists. Leave the hook, not the behavior.
- **No changes to `Rank` encoding or stored `Source` strings.** DB rows,
  compat mapping, and sort order all lean on them. Extend, never renumber.
- **No release-name parser behavior changes.** The corpus pins it;
  candidates are still judged by name (§3). Adding *fields* is fine —
  remember the apigen/openapi dual-marshal gotcha (§2 API).
- **No new "Any" profile, no per-item quality overrides.** Interactive
  search is the manual override and already bypasses the decision gate.
- **No trust lists / TRaSH score imports / per-group reputation.** Custom
  formats remain the extension point for taste.

---

## 10. Repo gotchas the implementer must know (hard-won)

- apigen DTO ↔ openapi drift: new fields must land in
  `api/openapi.yaml` too, or release-search JSON silently drops them.
- Test servers may lack `Deps.Settings` — nil-guard via the existing
  `readSettingOr` pattern before reading `MONARR_FFPROBE`-style settings.
- e2e must NOT enable auth; Playwright uses `domcontentloaded` (SSE keeps
  the page loading forever otherwise); vite build wipes `web/dist/.gitkeep`
  — restore it or the embed breaks.
- sqlite generated code is package-private by design: add wrapper methods
  on `*sqlite.DB`, never export gen types.
- STRICT tables reject loose types; goose is up-only — a wrong migration
  ships a corrective follow-up, not an edit.
- Corpus expectations go stale when the parser/prober learns — update the
  expectation files in the same commit, and say so in the message.
- Toolchain pins: go 1.25.7 · sqlc v1.31.1 · oapi-codegen v2.8.0 ·
  golangci-lint v2.12.2. `make test` / `make lint` / `make test-e2e` are
  the gate (`make gen` after schema/spec edits — CI has a gen-drift
  check); slog escapes newlines — operator-facing multi-line output goes
  to stderr raw.
