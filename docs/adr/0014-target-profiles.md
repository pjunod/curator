# ADR 0014 — A quality profile is a target, not a list with a cutoff

- **Status:** Accepted — built 2026-07-25 (v0.6.0)
- **Date:** 2026-07-25
- **Relates to:** ADR [0013](0013-measured-quality.md) (measured on-disk
  quality, which these rules consume), ADR
  [0003](0003-compat-personalities.md) (the shim must keep serving
  Radarr-shaped profiles), ADR [0006](0006-books-third-media-kind.md)
  (book formats ride the same model on the source axis)

## Context

The current model is Radarr's, inherited whole: a profile is an ordered
*allowed list* plus a *cutoff* (`quality.Profile` in
[quality.go](../../internal/domain/quality/quality.go)). The seeded default
is named **Any**: fourteen allowed entries and a hard-coded cutoff of
WEB-DL 1080p ([migration 0003](../../internal/infra/sqlite/migrations/0003_acquisition.sql)).

What the user sees on an item page is "Any — upgrades until WEB-DL 1080p,
then stops", and the honest reaction is the one that prompted this ADR:
*that doesn't make any sense*. The UI already knew — the profile row in
[MediaDetail.tsx](../../web/src/pages/MediaDetail.tsx) carries a comment
apologizing for the surprise. A model the UI has to apologize for is the
wrong model.

The incoherence is structural, not cosmetic, and it cuts both directions:

- **The cutoff does not bound what gets grabbed.** For a missing item,
  automation grabs the *highest-ranked* accepted release
  (`searchAndGrabBest` in
  [automation.go](../../internal/app/acquisition/automation.go) sorts by
  `quality.Rank`). Under "Any" that is an 80 GB 2160p remux — for a profile
  whose cutoff says WEB-DL 1080p would have been plenty.
- **Yet it stops upgrades below what it would happily have grabbed.** Start
  at 720p and the hunt ends at WEB-DL 1080p; start missing and you may get
  a 2160p remux. Same profile, two different answers to "what do I want".
- Two knobs (allowed set, cutoff) encode one intent ("what do I want on
  disk"), so every profile is a puzzle. Nobody solves it: there is no
  profile editor — the five seeded rows are the only profiles that have
  ever existed.

## Decision

### 1. A profile is a floor, a target, and an upgrades switch

```
Profile {
    Name            string
    Target          Quality    // the point of the profile
    Floor           *Quality   // optional: below this, don't even grab
    UpgradesAllowed bool
}
```

The semantics fit in three sentences, and the sentences ARE the spec:

> Hunt the best release **at or below the target's resolution**. While
> what's on disk is below the target and upgrades are on, keep looking;
> once the target is met, stop. Never grab below the floor.

One knob expresses the intent; the name can finally tell the truth. The
seeded profiles become **1080p** (target WEB-DL 1080p — the default),
**HD-1080p** (same target, floor at 1080p: never accept less), **4K**
(target WEB-DL 2160p), **Ebook** (target EPUB), **Audiobook** (target M4B).
**"Any" is retired** — a hunting profile with no opinion is not a thing;
what "Any" actually did (stop at WEB-DL 1080p) is what the 1080p profile
now says it does.

### 2. The three predicates, exactly

With `res(q)` the resolution tier and `srcRank(q)` the source rank
(both existing; `Rank` and the `Source` strings do not change):

- **met(current)** — `res(current) > res(target)`, or `res(current) ==
  res(target)` and (`srcRank(current) ≥ srcRank(target)` **or the source is
  unverified** — the don't-churn rule, ADR 0013 §5).
- **acceptable(release)** — `res(release) ≤ res(target)`, and if a floor is
  set, `Rank(release) ≥ Rank(floor)`. Release quality still comes from the
  release name; a candidate has nothing else to judge by.
- **upgrade(release, current)** — `acceptable(release)` and
  `Rank(release) > Rank(current)` and `!met(current)`.

Grabbing picks the best acceptable release by `Rank`, exactly as today —
the change is that *acceptable* is now resolution-capped by the target, so
the missing-item path can no longer overshoot the profile's own stated
goal.

### 3. Resolution is capped; source is not

At the target resolution, a source *above* the target's source is welcome:
a 1080p remux satisfies (and may be grabbed under) a WEB-DL 1080p target.
Refusing a strictly better file at the resolution the user asked for helps
no one; the thing people actually fear — surprise 4K downloads — is a
resolution problem, and the resolution cap handles it. Size preferences
within a resolution are what custom-format scores already do
(`format.Score` breaks ranking ties today; unchanged).

### 4. Existing rows migrate in place, ids stable

`media_items`, `media_copies`, and import lists reference profiles by id,
so ids 1–5 are rewritten, never re-created:

| id | was | becomes |
|---|---|---|
| 1 | Any (cutoff webdl-1080) | **1080p** — target webdl-1080, no floor |
| 2 | HD-1080p (1080-only allowed) | **HD-1080p** — target webdl-1080, floor hdtv-1080 |
| 3 | Ultra-HD (2160-only allowed) | **4K** — target webdl-2160, floor webdl-2160 |
| 4 | Ebook (cutoff epub) | **Ebook** — target epub, no floor |
| 5 | Audiobook (cutoff m4b) | **Audiobook** — target m4b, no floor |

Behavior change to name honestly: items on old-"Any" with nothing on disk
could previously grab 2160p; after migration they cap at 1080p. That is
the fix, not a regression — the profile finally does what its cutoff
always claimed.

### 5. Profiles become editable

A model users cannot see or change is how "Any" happened. Profile CRUD
(create/edit/delete, delete refused while referenced) ships in the same
phase, API and UI, with the profile row rendered as the sentence it now is:
"1080p — hunts the best release up to WEB-DL 1080p, then stops."

### 6. The compat shim keeps serving Radarr-shaped profiles

`/qualityprofile` consumers (Jellyseerr et al.) get a synthesized
allowed-items + cutoff view generated from the target — same wire shape the
fake-consumer suite already asserts, same ids and names. Translation-only,
per ADR 0003; the target model is not exposed through the shim.

### 7. Two exclusions the allowed-lists gave for free

Discovered while building this, and stated here because a target model
does not imply them:

- **Format families never compete.** Ebook, audiobook, and film/TV are
  three vocabularies sharing one `Source` field. Without an explicit rule
  an M4B is "better than" an EPUB by rank and satisfies an Ebook profile.
  A release is only acceptable if its family matches the target's.
- **A screen capture is never a stand-in.** CAM and telesync are not
  lower-quality copies of the film, they are a recording of a screening,
  and no profile wants one while waiting for the real thing. A floor
  cannot express this: `Rank` is resolution-dominant, so a 1080p CAM
  outranks a 480p Bluray. It is a rule, not a threshold — a release whose
  source is a screen capture is rejected unless the profile targets one.

## Consequences

- Every decision-engine call site is re-specified against §2, and the
  rejection codes change with them (`above_target`, `below_floor`,
  `target_met` replace the cutoff vocabulary). The codes only surface in
  Monarr's own UI; the compat shim never carried them.
- The decision engine gains its missing distinction: "on disk with
  unverified quality" is neither missing nor upgradable-by-guess
  (ADR 0013 §5). This kills the duplicate-grab chain.
- Books are unchanged in behavior: resolution is 0 on both sides, so the
  resolution cap is vacuous and `met` reduces to source-rank ≥ target —
  which is precisely the old cutoff semantics for formats.
- Season logic is untouched: a season's current quality is still its
  weakest episode; `met`/`upgrade` apply to that value as before.
- One expressiveness loss, accepted: the old model could exclude an
  *interior* quality (allow 720p and 1080p but ban 1080p WEBRip). No seeded
  profile did this, no editor existed to build one, and custom formats are
  the right tool for that shape of opinion.

## Alternatives considered

- **Keep allowed+cutoff, add the editor and honest copy.** Smallest
  change, and the UI comment proves where it ends: explaining a confusing
  model instead of fixing it. The grab-above-cutoff asymmetry survives
  untouched.
- **Weighted score ladder (TRaSH-style).** Strictly more expressive,
  strictly harder to reason about, and it moves the user's intent into
  numeric weights nobody can read back. Custom formats already provide the
  scoring escape valve at the margin where it earns its complexity.
- **Per-item targets, no named profiles.** Loses the one thing profiles do
  well — set policy once for hundreds of items (and per copy; the copies
  feature leans on profile references). The fix is making the named thing
  legible, not removing the name.
