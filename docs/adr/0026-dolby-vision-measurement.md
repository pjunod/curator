# ADR 0026 — Dolby Vision and HDR attributes are measured, ranked by profile, and probed before grabbing

- **Status:** Accepted — design 2026-11-24; implementation tracked in effort
  E-F99485C2CA
- **Date:** 2026-11-24
- **Relates to:** ADR [0013](0013-measured-quality.md) (measured quality and
  the native probe this extends), ADR [0014](0014-target-profiles.md) (the
  profile model that gains an HDR order), ADR [0022](0022-audio-language.md)
  (the precedent for a profile axis that is not a resolution rank), ADR
  [0019](0019-release-identity-evidence.md) (release names are evidence, not
  facts)

## Context

Curator records whether a file carries Dolby Vision and nothing else about
it. The native walkers see the `dvcC`/`dvvC` box in MP4
([mp4.go](../../internal/domain/mediainfo/mp4.go)) and the matching
`BlockAddIDType` in Matroska
([ebml.go](../../internal/domain/mediainfo/ebml.go)) and append `"dv"` to
`VideoInfo.HDR`. The ffprobe mapping
([probe.go](../../internal/infra/probe/probe.go)) does not even do that: it
reads only the colour transfer, so an MKV probed through ffprobe has no DV
at all. The release parser has no HDR vocabulary, so a search cannot tell
`DV`, `DoVi`, `HDR10+` or `DV.HDR10` releases apart from SDR ones.

That is too little for what Dolby Vision actually varies on:

| Attribute | Where it lives | Why it matters |
|---|---|---|
| Profile (5, 7, 8, …) | config record byte 2 | P5 has no HDR10 base layer and plays wrong anywhere DV is not decoded; P7 is the disc format; P8 is the single-layer streaming/encode format |
| Level | config record | Resolution/frame-rate envelope |
| `rpu`/`el`/`bl` present flags | config record | Whether a second (enhancement) layer exists |
| BL signal compatibility id | config record | 1 = HDR10, 2 = SDR, 4 = HLG, 6 = HDR10 (Blu-ray); says what a non-DV player shows |
| EL type: none / MEL / FEL | first RPU (NLQ parameters) | A Full Enhancement Layer carries real residual picture data; a Minimal one carries none and looks identical to P8.1 |
| CM version (2.9 / 4.0) | first RPU (extension blocks) | CM v4.0 trims map better on modern displays |
| HDR10+ | ST 2094-40 SEI (HEVC) / ITU-T T.35 in MKV BlockAdditions | Dynamic metadata without DV |

The two most valuable facts, FEL versus MEL and whether a "DV" release is
P5, are not in any release name reliably. The P7 FEL/MEL split is not in the
config record at all. Only the bytes settle it, and before a grab Curator
has no bytes.

## Decision

### 1. `VideoInfo` gains a measured Dolby Vision record and HDR10+

```
VideoInfo {
    …
    HDR    []string        // unchanged; "hdr10plus" added to the vocabulary
    DoVi   *DoViInfo       // nil = no DV config record found
}
DoViInfo {
    Profile       int     // 5, 7, 8, …
    Level         int
    RPU, EL, BL   bool    // presence flags from the config record
    BLCompatID    int     // 0 none, 1 HDR10, 2 SDR, 4 HLG, 6 HDR10 (UHD-BD)
    ELType        string  // "" unknown, "none", "mel", "fel"
    CMVersion     string  // "" unknown, "2.9", "4.0"
    Source        string  // "config" | "rpu" — how far the probe got
}
```

`ELType` and `CMVersion` come from the first RPU. A probe that reached the
config record but not an RPU leaves them empty, which means *unknown*, not
*absent* — the ADR 0013 §5 rule on a new axis. `ELType = "none"` is set
from the config record alone when `EL` is false (single-layer P5/P8).

### 2. One decoder, used by every probe path

A pure `internal/domain/mediainfo/dovi` package decodes:

- the `dvcC`/`dvvC`/`dvwC` configuration record (MP4 sample entry box and
  Matroska `BlockAddIDExtraData` carry the same 24 bytes);
- the first RPU NAL unit (HEVC NAL type 62 in-band for P5/P8; the EL
  track's or interleaved NAL for P7 MKV), unescaping emulation prevention
  bytes, enough to read the NLQ method and whether residual mapping is
  non-trivial (FEL) or identity (MEL), and the CM version from the
  extension metadata blocks;
- the HDR10+ presence marker (ST 2094-40 user data, country code 0xB5,
  provider 0x003C, oriented code 0x0001).

The native EBML and MP4 walkers hand it the config record and, when the
byte budget reaches the first video frame, the first access unit. The
ffprobe path maps `side_data_list` entries (`DOVI configuration record`,
`Dolby Vision RPU Data`, `HDR Dynamic Metadata SMPTE2094-40`) onto the same
struct, so a file never gets a different answer depending on which prober
read it. The decoder gets the parser treatment: committed truncated
corpus, fuzz target, never panics. The probe byte budget rises only as far
as needed to reach the first frame (single-digit MiB, as ADR 0013 already
allows).

### 3. Release names carry HDR claims with a confidence

The parser gains an `HDR` claim on `Parsed`:

```
HDRClaim {
    Formats    []string // "dv", "hdr10", "hdr10plus", "hlg"
    DVProfile  int      // 0 unknown; 5, 7, 8
    ELType     string   // "", "mel", "fel"
    Confidence string   // "high" | "medium" | "low"
}
```

Tokens: `DV`, `DoVi`, `DOLBY.VISION`, `HDR`, `HDR10`, `HDR10+`, `HDR10P`,
`HDR10PLUS`, `HLG`, explicit `P5`/`P7`/`P8`/`FEL`/`MEL`, and the hybrid
`DV.HDR10`/`DV HDR10+`. Explicit profile tags give high confidence. An
inferred profile is medium or low: a UHD BluRay REMUX with DV is likely P7
(medium), a WEB-DL with DV and no HDR tag is likely P5 (medium), an encode
with `DV.HDR10` is likely P8.1 (medium), a bare `DV` with nothing else is
low. The claim never says FEL unless the name says it. These are the
dashed "DV P7?" badges in the UI; measured values are the solid ones.

### 4. Profiles order and reject HDR tiers, measured outranks claimed

A profile gains an optional HDR order, following the ADR 0022 pattern of a
criterion beside the resolution target rather than a new resolution tier:

```
Profile {
    …
    HDROrder  []string  // e.g. ["dv-p7-fel","dv-p7-mel","dv-p8","hdr10plus","hdr10"]
    HDRReject []string  // e.g. ["dv-p5","sdr-2160"]
}
```

Empty means no opinion, which is every existing profile. The tier of a file
or release is computed from its measured `DoViInfo`/`HDR`, else its name
claim. Within the ADR 0014 predicates the HDR tier breaks ties after
resolution and source and before custom-format score. A rejected tier is
refused with `hdr_rejected`. A measured tier always outranks a claimed tier
of the same rank, and an unknown tier (probe failed, name silent) never
ranks above a measured one.

The `met` rule follows ADR 0013 §5: an on-disk file whose HDR tier is
unknown counts as met on that axis. A file is an upgrade target on the HDR
axis only when its measured tier is below the profile's top tier and
upgrades are on.

### 5. The order can be derived from the playback device

Cinema (plurx) knows what each player decodes: whether it decodes a P7 EL,
whether it plays P5, and whether it renders HDR10+ or falls back to static
metadata. Curator reads that capability report and offers "derive order
from device": tiers a device renders identically collapse (on a device that
drops the EL, P7 FEL, P7 MEL and P8 are one tier), and tiers it cannot play
correctly are proposed as rejections. The derivation fills the profile's
fields; the profile, not the device, remains the policy.

### 6. Pre-flight probe: Runner fetches bytes, Curator interprets them

Before grabbing, Curator asks Runner for the head of the top N candidates:

```
POST /api/v1/probe   {"nzb": <nzb or id>, "bytes": 4194304}
→ {"status":"ok", "file", "file_size", "path":["yenc","rar-store"],
   "articles", "bytes", "body": "/api/v1/probe/<id>/head"}
→ {"status":"unavailable", "reason":"encrypted"|"compressed"|"missing_articles"|…}
```

Runner picks the main inner file (largest, by par2 or RAR headers when
names are obfuscated), fetches and yEnc-decodes only the articles covering
the first N bytes, and unwraps store-mode RAR. It never parses video. A
compressed or encrypted archive, or missing articles, returns a reason.

Curator passes the returned bytes to the same `mediainfo.Probe` it runs on
imported files, so a pre-flight measurement and a post-import measurement
cannot disagree. Search results show both the name claim and the
measurement; ranking uses the measurement when present. The measured
`ELType` is stored per title (`FEL on disc`), so later remux searches for
that title expect FEL and flag a MEL remux as a different master.

Probing is ordinary profile configuration (how many top candidates, bytes
per probe, what to do when a probe cannot read), not a feature gate. With
no Runner reachable the search behaves exactly as today, ranking by name
claims.

### 7. Cinema shows the measurement and the outcome

Cinema reads the measured `DoViInfo` per version through Curator's existing
file metadata, shows it on each version, says how each registered device
will play it (full FEL, DV tone-mapped, EL dropped, HDR10 base layer), and
the player overlay says "EL dropped" when that happens rather than hiding
it.

### 8. Built: measurement (v0.39.0)

What the first slice settled while building §1–§2:

- **The Matroska DV signal was being read in the wrong element.**
  `BlockAdditionMapping` is a child of `TrackEntry`; the walker looked for
  it inside `Video`, and the hand-built test fixture made the same mistake,
  so real mkvmerge output was recognised only when its CodecID happened to
  say Dolby Vision. It is now read where the spec puts it, with its
  `BlockAddIDExtraData` (the configuration record).
- **The RPU parser is a port of libdovi's** (`dolby_vision` crate, MIT), the
  de facto reference, and it checks the RPU's CRC32 before trusting a field.
  Its tests run against RPUs libdovi wrote, with libdovi's own parse as the
  expectation; MEL and CM v2.9, which those fixtures lack, are covered by an
  RPU written from the syntax table.
- **The first frame is read without reading the frame.** Both walkers step
  over NAL units by their length prefixes and read only the RPU (type 62) and
  prefix SEI (type 39) bodies, so a multi-MiB UHD keyframe costs a few KiB of
  budget. MP4 finds the first sample from `stsz`/`stco`/`co64`, or, in a
  fragmented file, from the first `moof`'s `tfhd`/`trun`.
- **HDR10+ implies HDR10.** ST 2094-40 is defined over a PQ signal, so the SEI
  proves the HDR10 base even when an MP4 carries no `colr` box.
- **The ffprobe path is one call**, adding `-show_frames -read_intervals
  %+#1` to read the first frame's side data, and maps FFmpeg's decoded RPU
  fields onto `dovi.Header` and `dovi.NLQ`, so profile and FEL/MEL go through
  the same functions. FFmpeg does not print the CM version or level 6, so
  those stay unknown on that path. A test holds the two paths equal on every
  field FFmpeg reports, using committed ffprobe output for the same fixtures.
- **Existing measurements are refreshed by revision, not by migration.**
  `Info` records the `ProberRevision` that measured it; `Info.Outdated()`
  says whether the current prober would learn more (revision 2: HEVC
  files), and the scan's `NeedsProbe` queues those like any unprobed file
  (ADR 0013 §4). Manual qualities are never re-measured.

## Consequences

- One more binary format we own (the DV config record and enough RPU to
  classify the EL). It is small, documented, and fuzzed like the rest.
- Search cost grows by a few MiB per probed candidate. The default probes
  five; Runner's probe log shows the share of bandwidth.
- Obfuscated or encrypted releases stay ranked by claim, below measured
  ones; that is the honest order, not a penalty.
- Existing profiles are unchanged until someone sets an HDR order.

## Alternatives considered

- **Name-only DV ranking.** Cheapest, and wrong exactly where it matters:
  names do not say FEL versus MEL, and "DV" WEB-DLs are often P5.
- **Runner interprets the bytes.** Would duplicate the prober in a second
  codebase and let pre-flight and post-import answers drift. Runner returns
  bytes, Curator decides.
- **Download first, measure, then delete.** Measures perfectly and wastes
  tens of GB per wrong guess.
- **ffprobe required.** Rejected for the same reasons as ADR 0013 §2; it
  stays an optional second path mapped onto the same struct.
