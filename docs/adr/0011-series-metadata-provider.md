# ADR 0011 — Series identity comes from TheTVDB; TMDB alone cannot describe a Sonarr-shaped library

- **Status:** Proposed
- **Date:** 2026-07-25
- **Relates to:** ADR [0005](0005-filesystem-adoption.md) (adoption is the
  migration path — this is a limit on how far matching can take it), ADR
  [0010](0010-scan-adopts.md) (the matcher, which is not the thing at fault
  here), ADR [0003](0003-compat-personalities.md) (the Sonarr personality,
  which already speaks TVDB ids)

## Context

The blueprint chose one metadata provider for both video kinds (§4.2, "one
adapter, one key, one rate-limit policy"). TMDB serves movies and TV, and
that has held for everything until now. This record is about the case where
it does not hold, which turns out not to be an edge.

### The case that surfaced it

A folder named `Cunk on Earth` would not match, and no amount of matcher
work fixed it. Alternate-title lookups (added 2026-07-25) made it worse: the
folder matched, but to the wrong thing, and so did every other folder in the
franchise. The reason is that the two databases model the programmes
differently, and only one of them agrees with how the files are laid out on
disk:

| Source | What it holds | Shape |
|---|---|---|
| TMDB | [`tv/79063`](https://www.themoviedb.org/tv/79063-cunk-on) "Cunk on…" (2018– ) | One series; *Cunk on Earth* is **season 2, titled "Earth"** |
| TheTVDB | `series/414217` "Cunk on Earth" (2022) | Standalone series, 1 season, 5 episodes |
| TVmaze | [`shows/63900`](https://www.tvmaze.com/shows/63900/cunk-on-earth) "Cunk on Earth" | Standalone series |
| Plex (the user's) | "Cunk on Earth", 2022, Season 1, 5 episodes | Matches TheTVDB |

There is no TMDB entry to match, so there is nothing for the matcher to
find. TMDB's alternate titles for `tv/79063` list every programme in the
strand — *Cunk on Britain*, *Cunk on Earth*, *Cunk on Christmas* — which is
why alternate-title matching pointed all of them at one entry and only the
first could have it.

**This is a provider coverage problem wearing a matching problem's clothes.**
No scoring rule recovers a record the provider does not have.

### Why it is not a one-off

Two facts make the same failure systematic rather than anecdotal:

1. **Users' folders were laid out by Sonarr, and Sonarr is TVDB-first.** The
   audience ADR 0005 targets — people with 600-title libraries to migrate —
   have libraries shaped by TVDB's grouping decisions. Every series where
   TVDB splits and TMDB lumps (documentary strands, anthologies, UK panel
   formats, miniseries under an umbrella) fails exactly as Cunk did.

2. **Scene release names use TVDB episode numbering.** This is the larger
   half and it has nothing to do with adoption. Monarr matches release names
   to episodes in acquisition; the numbering in those names is TVDB's. Where
   TMDB numbers differently — specials placement, season splits, absolute
   numbering for anime — a correctly identified series still grabs the wrong
   episode, or grabs nothing. Adoption made the mismatch visible; the
   acquisition pipeline is where it costs something.

The current code is already half-committed to TVDB ids without holding the
data: `domain.ExternalIDs` carries `TVDB`, the schema has `tvdb_id`, the
TMDB adapter reads `external_ids.tvdb_id`
([client.go](../../internal/adapters/tmdb/client.go)), and the Sonarr
personality resolves adds *by TVDB id* because that is how Jellyseerr
identifies a series ([sonarr.go](../../internal/compat/sonarr.go)). Monarr
speaks TVDB at its edges and thinks in TMDB in the middle.

### The constraint that makes this awkward

TheTVDB v4 has no free tier. Every API key needs either a negotiated
commercial licence or "user-supported" mode, where each end user buys a TVDB
subscription and supplies a PIN alongside the project's key
([TheTVDB FAQ](https://support.thetvdb.com/kb/faq.php?id=62)). Sonarr users
never see this because Sonarr proxies TVDB through its own Skyhook service
under a licensed key. Monarr has no such service and should not acquire one:
a self-hosted tool that cannot work without the vendor's server running is
not self-hosted.

## Decision

### 1. Series metadata comes from TheTVDB when a key is configured

A new `ports.SeriesProvider` — search, hydrate, episode list, keyed by TVDB
id — with a TheTVDB v4 adapter behind it. Configured exactly like the TMDB
and OMDb keys are today: a field in Settings, a health check that says
whether it is working, and no requirement to have one.

Movies stay on TMDB. This is the Sonarr/Radarr split, arrived at for the
same reason they arrived at it.

### 2. Without a key, TMDB continues to serve series, and says what it costs

TMDB series support is not removed and not deprecated. It is the default,
and it is correct for the large majority of series. What changes is that the
degraded case is named rather than presented as a matching failure: when a
folder's only candidate is an umbrella entry (ADR 0010's `heldBy` /
`sharedWith`), the row should say that a TVDB key would likely resolve it,
because that is the actual remedy and "stop offering" is not.

### 3. Identity is dual-keyed from the start, whichever provider hydrated it

An item records both ids whenever both are known. TMDB already hands us
`external_ids.tvdb_id` for free, so this costs one field assignment today
and removes the migration later: a library built without a TVDB key can be
re-pointed at TVDB by id, not by re-matching titles.

Where only one id is known, the item is keyed on that one. `AddRequest` and
the adoption candidate grow a provider discriminator rather than assuming
`TMDBID` — 37 call sites in Go and 6 in the web client currently assume it.

### 4. Episode numbering follows the provider that identified the series

Mixing is worse than either source alone: a series identified on TVDB and
numbered from TMDB matches nothing reliably. The provider that supplied the
identity supplies the seasons and episodes, and the item records which one
that was.

## Consequences

- A second key to explain, and a second rate-limit policy — exactly the cost
  §4.2 of the blueprint chose to avoid. That choice was right when the only
  question was "can we find the show"; it is wrong now that the question
  includes "are the episodes numbered the way the files are".
- Users who supply no TVDB key are no worse off than today, but the ceiling
  on adoption accuracy is now documented rather than discovered folder by
  folder.
- The Sonarr personality gets simpler: it already wants TVDB ids and
  currently reaches them through TMDB's `external_ids`, which is only
  populated for series TMDB knows.
- Anime remains unsolved and gets closer. TVDB numbering plus scene naming
  is the combination the *arr ecosystem's AniDB mapping tables are built on
  top of; TMDB numbering is not a base those tables can sit on.
- Nothing here helps a folder whose programme genuinely has no standalone
  record anywhere. Those still end in review, correctly.

## Alternatives considered

- **TVmaze instead of TheTVDB.** Free, no key, no account, and it has *Cunk
  on Earth* as a standalone show — it would fix the case that prompted this
  record with none of the licensing friction. Rejected as the primary
  because it introduces a *third* numbering authority: scene names follow
  TVDB, so a TVmaze-numbered series inherits the episode-matching mismatch
  that §2 above identifies as the expensive half. Worth revisiting as a
  no-key fallback ranked above TMDB for series *identity* while episode
  numbering stays with whoever the user has keyed.
- **A monarr-operated Skyhook.** Solves the key problem for users and
  creates a service the project must fund, operate, and keep available, plus
  a dependency that makes every install phone home to us. Contradicts the
  self-hosted premise directly.
- **Teach the matcher about seasons-as-programmes.** TMDB's `tv/79063`
  season 2 *is* titled "Earth", so a folder named `Cunk on Earth` could be
  matched to a series-plus-season pair. This is real and it is not enough:
  the library entry would still be "Cunk on…", the poster and overview would
  be the strand's, and monarr would need multi-folder series support to hold
  three folders against one item. It fixes the lookup and not the thing the
  user sees, which is the Plex screenshot.
- **Accept the umbrella and dismiss the folders.** What the code does today.
  For *Cunk on Earth* it means a show the user owns, that Plex displays
  correctly, is unmanageable in monarr — no monitoring, no upgrades, no
  episode tracking. Acceptable for a folder that is genuinely not media;
  not acceptable for one that is.
