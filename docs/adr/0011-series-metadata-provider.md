# ADR 0011 — Series metadata is a chain, not a provider: TVDB when keyed, TVmaze free, TMDB always

- **Status:** Accepted — TVmaze first (no key), TheTVDB when a key exists.
  TVmaze and TMDB lookup/enrichment are implemented; the paid TheTVDB adapter
  remains future work.
- **Date:** 2026-07-25 (revised the same day from the Proposed single-provider
  version, after TVmaze turned out to hand out TheTVDB ids for free)
- **Relates to:** ADR [0005](0005-filesystem-adoption.md) (adoption is the
  migration path — this is a limit on how far matching can take it), ADR
  [0010](0010-scan-adopts.md) (the matcher, which is not the thing at fault
  here), ADR [0003](0003-compat-personalities.md) (the Sonarr personality,
  which already speaks TVDB ids), ADR
  [0012](0012-manual-entries.md) (the escape hatch for what no provider has
  right)

## Context

The blueprint chose one metadata provider for both video kinds (§4.2, "one
adapter, one key, one rate-limit policy"). TMDB serves movies and TV, and
that held for everything until it didn't.

### The case that surfaced it

A folder named `Cunk on Earth` would not match, and no matcher work fixed
it. Alternate-title lookups (added 2026-07-25) made it worse: the folder
matched, but to the wrong thing, and so did every other folder in the
franchise. The databases model the programmes differently, and only some of
them agree with what is on disk:

| Source | Record | Shape |
|---|---|---|
| TMDB | [`tv/79063`](https://www.themoviedb.org/tv/79063-cunk-on) "Cunk on…" (2018– ) | One series; *Cunk on Earth* is **season 2, titled "Earth"** |
| TheTVDB | `series/414217` "Cunk on Earth" (2022) | Standalone series, 1 season, 5 episodes |
| TVmaze | [`shows/63900`](https://www.tvmaze.com/shows/63900/cunk-on-earth) "Cunk on Earth" (2022-09-20) | Standalone, `S01E01`–`E05` + one special |
| Plex (the user's) | "Cunk on Earth", 2022, Season 1, 5 episodes | Matches TheTVDB and TVmaze |

There is no TMDB record to match, so no scoring rule can find one. TMDB's
alternate titles for `tv/79063` list every programme in the strand, which is
why alternate-title matching pointed all of them at one entry and only the
first could have it.

**This is provider coverage wearing a matching problem's clothes.**

### Why it is not a one-off

1. **Users' folders were laid out by Sonarr, and Sonarr is TVDB-first.** The
   audience ADR 0005 targets — people with 600-title libraries to migrate —
   have libraries shaped by TVDB's grouping. Every series where TVDB splits
   and TMDB lumps (documentary strands, anthologies, UK panel formats,
   miniseries under an umbrella) fails exactly as Cunk did.

2. **Scene release names carry TVDB episode numbering.** This is the larger
   half and has nothing to do with adoption. Monarr matches release names to
   episodes in acquisition; where TMDB numbers differently — specials
   placement, split seasons, absolute numbering for anime — a correctly
   identified series still grabs the wrong episode. Adoption made the
   mismatch visible; acquisition is where it costs something.

The code is already half-committed to TVDB ids without holding the data:
`domain.ExternalIDs` carries `TVDB`, the schema has `tvdb_id`, the TMDB
adapter reads `external_ids.tvdb_id`
([client.go](../../internal/adapters/tmdb/client.go)), and the Sonarr
personality resolves adds *by TVDB id* because that is how Jellyseerr
identifies a series ([sonarr.go](../../internal/compat/sonarr.go)). Monarr
speaks TVDB at its edges and thinks in TMDB in the middle.

### The constraint, and the thing that softens it

TheTVDB v4 has no free tier. Every key needs a negotiated commercial licence
or "user-supported" mode, where each end user buys a TVDB subscription and
supplies a PIN alongside the project's key
([TheTVDB FAQ](https://support.thetvdb.com/kb/faq.php?id=62)). Sonarr users
never see this because Sonarr proxies TVDB through its own Skyhook service
under a licensed key. Monarr has no such service and should not build one: a
self-hosted tool that stops working when the vendor's server does is not
self-hosted.

What changes the calculus is a fact discovered while checking TVmaze against
the real API: **TVmaze publishes TheTVDB ids, and they are the right ones.**

```
GET api.tvmaze.com/singlesearch/shows?q=Cunk+on+Earth
  id 63900 | Cunk on Earth | 2022-09-20 | externals: {thetvdb: 414217, imdb: tt16867040}
GET api.tvmaze.com/singlesearch/shows?q=Cunk+on+Britain
  id 35781 | Cunk on Britain | 2016-05-11 | externals: {thetvdb: 339732, imdb: tt8232636}
```

`414217` is the same id thetvdb.com serves for that series. So a library
built today, with no TVDB key and no TVDB account, can still be *keyed* on
TVDB ids. That turns the eventual migration from a re-match into a lookup.

## Decision

### 1. Series identity is resolved by a chain, consulted in order

```
 series lookup
      │
      ▼
 TheTVDB ──── key configured? ── no ──┐
      │ yes                           │
      ├── usable record? ── yes ──▶ USE
      │        no                     │
      ▼                               ▼
 TVmaze ───── usable record? ── yes ──▶ USE
      │        no
      ▼
 TMDB (always present) ──────────────▶ USE, or nothing to match
```

**Fallback only, never override.** A lower link is consulted when the one
above has no usable record — not to second-guess one that does. This is
deliberate and conservative: everything that matches correctly today keeps
matching the same way, and the chain engages exactly where monarr currently
fails. A chain that re-ranks working answers would trade a visible bug for
an invisible one.

Movies stay on TMDB alone. This is the Sonarr/Radarr split, reached for the
reasons they reached it.

### 2. TheTVDB when a key is configured, and the UI says why that is worth doing

A `ports.SeriesProvider` — search, hydrate, episode list, keyed by TVDB id —
with a v4 adapter behind it. Configured like the TMDB and OMDb keys are
today: a field in Settings, a health check that says whether it works, and
no requirement to have one.

The Settings copy must answer "why would I pay for this", in these terms and
not in marketing ones:

> **TheTVDB (optional, paid).** Release groups name TV files using TheTVDB's
> season and episode numbers, and TheTVDB splits some programmes that other
> databases group under one title. With a key, Monarr identifies and numbers
> series the same way the files you download are named — which mostly shows
> up as fewer folders in review and fewer episodes that grab the wrong file.
> Without one, Monarr uses TVmaze and TMDB, which are correct for the large
> majority of series. TheTVDB requires a paid subscription; Monarr has no way
> to provide one for you.

### 3. TVmaze is the keyless middle link, and the source of TVDB ids today

Free, no account, no key, ~20 requests per 10 seconds per IP, and it carries
TheTVDB and IMDb ids on every show. It is consulted for series TMDB cannot
resolve, which is the coverage gap this record exists for.

Its terms permit non-commercial use. Each self-hosted install queries on its
own behalf for its owner's library, which is that; a hosted commercial
service built on monarr would need its own licence, and that is a line worth
writing down before someone builds one.

### 4. Identity is dual-keyed from the start, whichever link answered

An item records every id it can learn — TMDB, TVDB, IMDb — regardless of
which provider hydrated it. TMDB already supplies `external_ids.tvdb_id`;
TVmaze supplies `externals.thetvdb`. This costs a field assignment now and
removes a migration later: when a TVDB key arrives, a library built without
one is re-pointed **by id**, not re-matched by title.

`AddRequest` and the adoption candidate grow a provider discriminator rather
than assuming `TMDBID` — 37 call sites in Go and 6 in the web client
currently assume it.

### 5. Episode numbering follows whichever link supplied the identity

Mixing is worse than either source alone: a series identified on TVDB and
numbered from TMDB matches nothing reliably. The provider that gave the
identity gives the seasons and episodes, and the item records which one that
was, so the choice is inspectable rather than implied.

### 6. When no provider has it right, the answer is a manual record

The chain reduces the gap; it cannot close it. [ADR
0012](0012-manual-entries.md) covers what happens to the remainder, because
"dismiss the folder" means a show the user owns is unmanageable, and that is
not an acceptable floor.

## Consequences

- Two more providers to explain, two more rate-limit policies, and a search
  path that can hit three services — exactly the cost §4.2 of the blueprint
  chose to avoid. That choice was right when the question was "can we find
  the show". It is wrong now that the question includes "are the episodes
  numbered the way the files are".
- The fallback-only rule means the chain is invisible to users whose
  libraries already work. That is the point, and it also means the feature
  is hard to notice — the release notes have to name the failure it fixes,
  not the providers it adds.
- Users with no TVDB key are strictly better off than today (TVmaze covers
  a real gap for free), and the remaining ceiling is documented rather than
  discovered folder by folder.
- The Sonarr personality gets simpler: it already wants TVDB ids and
  currently reaches them only through TMDB's `external_ids`, which exist
  only for series TMDB knows.
- Anime remains unsolved and gets closer. TVDB numbering plus scene naming
  is what the ecosystem's AniDB mapping tables sit on top of; TMDB numbering
  is not a base those tables can use.
- Three id spaces in one table is a correctness burden. Dual-keying is only
  safe if "the same series from two providers" collapses to one row, so the
  id lookup has to try every known id before deciding an item is new.

## Alternatives considered

- **TheTVDB alone, as the original version of this record proposed.** Best
  data for the job and unavailable to anyone without a paid subscription —
  including, on the day this was written, monarr's own author, who could not
  complete signup. A provider decision that leaves the project unable to test
  its own default path is the wrong decision.
- **TVmaze alone, replacing TMDB for series.** Free, keyless, and it has the
  show. Rejected as the *primary* because it would become a third numbering
  authority applied to series that TMDB already handles correctly — churning
  working libraries to fix the ones that are broken. As a fallback it takes
  the gap without touching the rest.
- **A monarr-operated Skyhook.** Solves the key problem for users, creates a
  service the project must fund, operate and keep available, and makes every
  install phone home. Contradicts the self-hosted premise directly.
- **Teach the matcher about seasons-as-programmes.** TMDB's `tv/79063` season
  2 *is* titled "Earth", so the folder could match a series-plus-season pair.
  Real, and not enough: the library entry would still be "Cunk on…", with the
  strand's poster and overview, and monarr would need multi-folder series
  support to hold three folders against one item. It fixes the lookup, not
  the thing the user sees.
- **Accept the umbrella and dismiss the folders.** What the code does today.
  For *Cunk on Earth* that means a show the user owns, which Plex displays
  correctly, is unmanageable in monarr — no monitoring, no upgrades, no
  episode tracking. Fine for a folder that is not media; not fine for one
  that is.
