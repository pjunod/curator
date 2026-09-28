# ADR 0022 — Audio language is a profile criterion

- **Status:** Accepted — built 2026-09-28 (v0.31.0)
- **Date:** 2026-09-28
- **Relates to:** ADR [0014](0014-target-profiles.md) (the target model
  this adds an axis to), ADR [0013](0013-measured-quality.md) (the probe
  that measures what is on disk, and the don't-churn rule this extends),
  [settings](../settings.md), [usage](../usage.md)

## Context

A WEB-DL 1080p copy of *Just Friends* (2005) sat in the library with no
English audio track. The item's profile targets WEB-DL 1080p, so by ADR
0014 the target was **met**: the wanted index dropped the item, backlog
search never looked at it, and every other release the indexers had was
"not an upgrade" — same or lower rank. The file was useless for the
person who owned it and monarr was satisfied with it forever.

Nothing in the model could express the problem. A profile knew about
source and resolution and nothing else, and the release parser had no
notion of language at all, even though the probe (ADR 0013) had been
reading the language tag off every audio track since Phase 6 and storing
it in `media_info`.

## Decision

### 1. A profile may require audio languages

```
Profile {
    …
    Languages []string   // canonical codes; empty = no opinion
}
```

Any one of the listed languages satisfies. Empty means what every profile
meant before this ADR — no opinion — so nothing existing changes behaviour
until someone asks for it. The profile sentence says it: *"hunts the best
release up to WEB-DL 1080p, then stops; **English audio required**"*.

It is a **requirement, not a rank**. Language is not a fifth resolution
tier; a file in the wrong language is not "a bit worse" than one in the
right language, it is the wrong file. Three rules follow, stated exactly:

- **languageMet(disk)** — no requirement, OR the disk's languages are not
  *known* (see §3), OR at least one required language is among them.
- **satisfied(disk)** — `met(disk)` (ADR 0014 §2, unchanged) AND
  `languageMet(disk)`. This replaces `met` as the question the wanted
  index asks. An item at the target in the wrong language is still wanted.
- **upgrade(release, disk)** — `acceptable(release)` (unchanged) AND the
  release *offers* a required language (§4) AND: if `!languageMet(disk)`,
  **true regardless of rank**; otherwise the ADR 0014 rule
  (`Rank(release) > Rank(disk)` and `!met(disk)`).

The "regardless of rank" clause is the point. A 720p English release IS
an upgrade over a German-only 1080p file, because the 1080p file does
not do the job at all. The floor still applies (a release below it is not
acceptable) and so does the resolution cap; the language rule relaxes
only the rank comparison.

A release offering none of the required languages is refused outright
with a new rejection code, `language_not_wanted`, whether the item is
missing or on disk. A German dub cannot fix a German problem.

### 2. One vocabulary, three sources

Three things describe a soundtrack's language and none of them agree on
spelling: a Matroska track says `ger` or `de-DE`, an MP4 media header
says `deu`, a release name says `GERMAN`. `internal/domain/language`
reduces all of them to one canonical code (ISO 639-1 where it exists:
`en`, `de`, `ja`) before anything compares them. `mul` — the real ISO
639-2 code for "multiple languages" — is what a `MULTi` or `DUAL-AUDIO`
release tag becomes, and it satisfies any requirement, because that is
what the tag promises.

Container tags accept the whole alias table (every ISO code is safe in a
data field). Release names use a deliberately smaller list: the spellings
release groups actually write as tags (`GERMAN`, `ITA`, `VFF`,
`TRUEFRENCH`, `MULTi`, `DL`). Three-letter ISO codes that are ordinary
words — `per`, `may`, `ice`, `fin`, `est` — are not read from names,
because names contain episode titles.

### 3. What is on disk is known only when every track has spoken

The probe records a language per audio track. The disk's language set is
**known** only when the file has at least one audio track and *every*
track declares a language. One untagged track is one track that might be
the language somebody wants, and this is the ADR 0013 §5 don't-churn rule
on a new axis: absence is a fact only when it has been measured, and
until then the requirement counts as met. A file monarr could not probe,
or a file whose tracks are all `und`, is never hunted for its language.

Across several files (a season's episodes, an item's file set) the rule is
weakest-link, the same as `SeasonWantable.CurrentQuality`: known only when
every file is known, carrying only the languages every file carries.

### 4. What a release offers is read from its name, and silence means English

A candidate has not been downloaded; its name is all there is. The parser
reads language tags from the part of the name **after the title**, so
*The French Connection* stays a title. A name with no language tag is read
as **English** — the scene convention (tags mark exceptions, not the rule)
and the one Radarr uses. Subtitle tags (`VOSTFR`, `SUBBED`, `NLSUBS`) say
nothing about the audio and are ignored. The compound source tags are
stripped before tokenizing so `WEB-DL` never yields the German scene's
`DL` (dual-language) tag.

Import judges by the same claim the grab did — the release name — and the
probe that follows placement records the truth. A file that turns out not
to carry the language leaves the item wanted again, which is the
self-correcting direction: a wrong grab costs one more search, never a
permanently satisfied item.

### 5. Every surface says so

- The item page's quality row reads *"upgrading · no English audio on disk
  (German only)"* instead of a nonsensical *"upgrading to 1080p"* beside a
  1080p file. The measured-facts pill names the language(s) of every file
  (`1080p · H.264 · AC-3 · German · 8.0 Mbps`), with `undeclared` for an
  untagged track once any track is tagged.
- The wanted list's *upgrade from* reads *"WEB-DL 1080p (German)"*.
- Interactive search shows what each release advertises when it is not
  plain English, and a refused release carries the reason (*"German
  audio; profile "1080p" requires English"*).
- The profile editor offers the vocabulary (`GET /languages`) as a set of
  toggles; codes are never typed.

### 6. Storage

The requirement lives in the profile's definition JSON as `languages`,
omitted when empty, so no migration runs and every definition written
before this ADR reads back as "no opinion". The list view's stats query
extracts the per-track languages from `media_info` with `json_each`, so
the grid grades language without a second query or a derived column.

## Consequences

- The profile model has an axis ADR 0014 did not anticipate, and it is
  the first criterion that is not a rank. `Profile.Upgrade` now takes the
  release's languages and the disk's audio; `decision.Decide` takes a
  `Release{Quality, Languages}` rather than a bare quality.
- Nothing changes for a profile with no requirement — every predicate
  short-circuits to its ADR 0014 form. The seeded profiles have none.
- Files probed before this ADR already carry their track languages, so
  the rule takes effect on existing libraries as soon as a profile asks;
  no re-probe is needed. A file that was never probed, or one whose
  tracks carry no tags, is not hunted — and the item page says the
  language could not be determined rather than pretending.
- Subtitles are out of scope. The probe does not read subtitle tracks, and
  the requirement is about audio; a subtitle rule would be a separate
  decision with a separate measurement.
- A language name inside an *episode* title (*"The French Mistake"*) can
  still be read as a tag, which makes an English release look French and
  refuses it. The refusal is visible in interactive search with its
  reason, and a manual grab is never gated. Upstream parsers live with
  the same limit; narrowing the scan further would miss the far more
  common `Show.S01E02.German.DL.1080p` form.
- The compat shim (ADR 0003) is unchanged: Radarr-shaped profiles do not
  carry the requirement, and nothing consuming them makes grab decisions.
