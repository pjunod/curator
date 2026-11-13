# ADR 0029 — shared observed availability for owner discovery

**Status:** accepted for implementation · **Date:** 2026-11-13.
Read with [discovery](0015-discovery.md), [recommendations](0023-smart-media-discovery.md),
[offload](0028-offload.md) and the [household plan](../HOUSEHOLD_DISCOVERY_PLAN.md).

## One identity, independent facts

Catalogue identity is present, absent, ambiguous or unknown. Presence is present,
partial, missing, offloaded or unknown. Acquisition is independently active,
none or unknown; no work is called searching merely because it is monitored.
Future request decisions/subscriptions remain a separate service. Numeric IDs
are keyed by media kind and provider; conflicting IDs stay ambiguous. Titles and
years never establish membership. A library catalogue entry proves neither files
nor playback. Initial presence covers primary copy 0 and known aired regular
episodes; additional copies and unknown episode dates are not silently merged.

## Observations are read-only evidence with an explicit age

Migration 0083 records per-item/copy root identity, scope, observed file IDs,
checked time and title/shared configuration revisions. A separate verification
pass captures revisions before reading filesystem state, then persists only if
those revisions and root binding still match. Subsequent browsing compares a
stable presence-facts fingerprint (identity, scopes, file generations, episode
attribution/dates and offload intent); unchanged refreshes, ratings and quality
probes do not expire it. The broad lifecycle revisions still fence writes. It does not place, prune, acquire
or delete anything. Directory-entry metadata and copy-path failures invalidate
observations and preserve prior file records. A missing file is a negative only
beneath a readable root with the expected volume identity. Empty roots need
explicitly established trust; emptiness cannot establish a first pin.

Discovery reads a consistent local database snapshot with no per-poster disk
probes. First-run, failed, changed, future-dated or older-than-13h proof is Unknown.
Normal reconciliation is scheduled every 12h; queue delay can legitimately expire
proof. Show Last checked. Open file-changing operations invalidate presence.
A newly imported title may remain Unknown until verified; false current presence
is worse than a conservative unknown. Intentional root replacement is not an
implicit claim that old file records belong to the new volume. Returning the
original root allows verification to recover without discarding its identity.

This bounded as-of view is a browsing fact. A future household approval must
perform a targeted current scope check before admitting new work; neither this
projection nor a client badge authorizes acquisition or cleanup.

## Scan reconciliation and volume trust

Reconciliation captures the title, file-generation and shared settings snapshot
before directory walking. A single transaction compares revisions and root
binding generations, refuses open file-changing operations and foreign-owned
paths, inserts discovered files and prunes only the captured records for trusted
copy scopes. An interleaved import rejects the transaction. Incomplete walks and
metadata errors preserve records. Imported episode mappings remain authoritative;
filename inference fills adopted or unlinked files only. Filename quality writes
compare the file generation and never overwrite measured/imported quality.

A known volume mismatch blocks both insertion and pruning because insertion can
clear intentional offload flags. Uninitialized roots may index ordinary files
but cannot prune or clear offload intent. Migration 0083 snapshots pre-upgrade
file identities for first-pin evidence. Later scans cannot bootstrap themselves.
New root registration explicitly pins the observed volume. An owner can review
and acknowledge a replacement through a fingerprint bound to the root identity,
generation, library/settings revision and active custody. Confirmation reobserves
the volume and updates trust atomically. Old scans and proofs cannot commit
against the previous generation. No filesystem file is deleted by rebinding.

## Provider shelves reuse outbound controls

The owner explicitly picks a paged library seed. Related titles read the first
TMDB recommendation page, capped at 30 unique titles. Movies use
[/movie/{id}/recommendations](https://developer.themoviedb.org/reference/movie-recommendations);
TV IDs never enter that route. Collections read a verified movie's
[details](https://developer.themoviedb.org/reference/movie-details), then the exact
[collection](https://developer.themoviedb.org/reference/collection-details), at
most two provider reads and 100 members. A mismatched collection or absent seed
is rejected. All reads reuse credential snapshots, cache, limiter and cooldown,
with a 15s shelf deadline. No provider traffic is scheduled for unseen shelves.

Fresh local availability is joined on every shelf response. Provider outages
stay local to their shelf and never imply missing ownership. The existing
advanced TV recommendation/ranking service stays available independently.
Missing-season review comes from the existing episode inventory and links to the
owner's existing season controls. It does not create another search/import path.

## Rollout boundary

Owner discovery ships first with Add/View/Review controls on web and native.
Cinema delegated sign-in and deny-by-default member authorization precede every
member Request control. Both apps retain independent accounts. Optional Cinema sign-in links a verified
external identity to a Curator account; local sign-in and permissions remain
usable without Cinema. No watch-history dependency, hosted AI requirement, setup
wizard or migration flow is introduced by this decision.
