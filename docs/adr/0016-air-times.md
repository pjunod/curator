# ADR 0016 — Episode air times come from TVmaze's show-level schedule, composed per date at read time

- **Status:** Accepted — built 2026-08-02
- **Date:** 2026-08-02
- **Relates to:** ADR [0011](0011-series-metadata-provider.md) (TVmaze is
  already in the series chain, and §5 of that record is why this one refuses
  per-episode mapping), ADR [0002](0002-single-media-table.md) (the three new
  columns live on `media_items`, not a new table), ADR
  [0004](0004-sqlite-only.md) (no cache table; the adapter's response cache is
  the cache)

## Context

The calendar has been date-only since Phase 3. `episodes.air_date` is an ISO
date with no clock time, because TMDB — the always-present last link of the
metadata chain — publishes none. A user looking at Thursday sees that three
episodes air; nothing says which one is on at 8 and which at 10, and the
agenda view added in the same release has an ordering problem it cannot solve
from the data it has.

Every source of a real air time has a cost:

- **TheTVDB** publishes `airsTime`, and a paid key, which is exactly the
  constraint ADR 0011 exists to route around.
- **Trakt** publishes an `airs` object (`{day, time, timezone}`) that is
  precisely the shape wanted — behind a configured client id. Trakt is
  optional in this codebase (ADR 0015), so half the installs would see times
  and half would not, with nothing on screen explaining the difference.
- **TVmaze** publishes the same thing (`schedule.time`, `schedule.days`,
  `network.country.timezone`), keyless, and its client is already
  constructed, rate-limited and cached in `cmd/monarr/main.go` as the series
  chain's second link.

The second question is where a time is stored once fetched. TVmaze also
publishes a per-episode `airstamp` — an exact UTC instant per episode — which
looks strictly better until you have to say *which* episode it belongs to.
Matching TVmaze episode rows against TMDB-numbered episode rows is the
numbering fault line ADR 0011 §5 exists to avoid: the two providers disagree
about specials, multi-part episodes and mid-season renumbering, and a
confidently wrong "S03E07 airs Tuesday 9 PM" is worse for the reader than an
honest date-only row.

## Decision

**A show's air time comes from TVmaze's show-level schedule, is stored on the
media item as a local wall-clock time plus an IANA timezone, and is composed
into a UTC instant per air date when the calendar is read.**

### 1. Three columns, on the item

```sql
ALTER TABLE media_items ADD COLUMN airs_time     TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN airs_timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN network       TEXT NOT NULL DEFAULT '';
```

`airs_time` is `"21:00"` exactly as TVmaze sends it (24-hour, network-local);
`airs_timezone` is an IANA name; `network` is a display string ("HBO",
"Netflix"). Empty means unknown, matching every other metadata-cache column
in that table.

### 2. A narrow optional port

```go
type Airing struct {
	Time     string // "21:00", 24h, "" = unknown
	Timezone string // IANA name, "" = unknown
	Network  string // display name, "" = unknown
}

type AiringProvider interface {
	Airing(ctx context.Context, tvdbID int64, imdbID string) (Airing, error)
}
```

Deliberately not folded into `SeriesProvider`: this is enrichment, in the
same sense `RatingsProvider` is, and it is consulted the same way — inside
`RefreshItem`, best-effort, an error leaving the item exactly as it was.
Nothing about the port names TVmaze, so a TheTVDB adapter can implement it
later for installs that have a key.

### 3. Composition happens at read time, not write time

The calendar handler parses `air_date` in `airs_timezone` at `airs_time` and
renders RFC3339 UTC. Storing a computed instant per episode instead would be
wrong twice over:

- **DST.** A 9 PM Eastern show is `02:00Z` in January and `01:00Z` in July.
  Composing per date gets both right with no dated logic; storing an instant
  bakes in whichever offset applied on refresh day.
- **Timeslot moves.** When a show changes nights, one field update fixes
  every future episode. With stored instants, every future row is stale until
  something rewrites it.

### 4. Absent beats guessed

`airDateUtc` is **omitted** from the API response when any piece is missing or
unparsable. Streaming shows (TVmaze `webChannel`, `schedule.time: ""`),
movies and books are date-only entries, visibly. A midnight-looking timestamp
that actually means "unknown" would poison both the agenda's sort and the
reader's trust in every other row.

## Consequences

- **Free, on every install.** No key, no setting, no new job. Backfill is the
  existing `metadata.refresh` cadence; a library that never refreshes never
  gets times, and never breaks either.
- **One request per series refresh**, on a client that already holds the rate
  limiter (≤20 req/10 s) and a URL-keyed response cache.
- **The API stays additive.** `airDateUtc`, `network`, `seasonNumber`,
  `episodeNumber`, `episodeTitle`, `posterPath`, `runtime` and `monitored`
  are new optional fields on `CalendarEntry`; `date` and `detail` keep their
  exact current values because plurx parses them in production.
- **`time/tzdata` is now imported by `cmd/monarr`.** The deploy image is
  minimal and has no `/usr/share/zoneinfo`; without the embedded database
  every `LoadLocation` would fail and every time would silently vanish.
- **Show-level granularity is the ceiling.** A series that aired its finale an
  hour later than usual will show the usual time. Fixing that needs
  per-episode data, which needs the episode mapping this record declines.
- **Half a library will have no times**, permanently — streaming originals
  publish no broadcast instant. That is the source being honest, and the UI
  says so by simply not showing a time.
- **What discriminates is the timezone, not the channel type.** A regional
  streamer does publish a slot — TVmaze gives BBC iPlayer
  `webChannel.country.timezone: "Europe/London"` and `schedule.time: "22:00"`
  — while a global one gives `country: null`. So the adapter reads the
  timezone off whichever channel object is present and drops the time when
  there is none, rather than deciding from `network` vs `webChannel`. Same
  outcome for Netflix, a real time for iPlayer, and no case where a clock is
  shown without a stated zone to read it in.
