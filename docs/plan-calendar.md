# Plan — Calendar, rebuilt: a readable month and a rich agenda

**Status:** ready to build · **Written:** 2026-08-02 ·
**Decision:** ADR 0016 (air times from TVmaze — §3.1 writes it)

The calendar is the oldest page in the app — a Phase 3 month grid of text
chips, date-only, no artwork, unchanged while every page around it grew up.
This plan rebuilds it twice over: the month grid becomes readable at a
glance, and a new **Agenda** view lists everything chronologically as rich
media rows — poster, episode, network, air time — scrolling from today
outward, on the web and in the native app.

How to work from this document: read [CLAUDE.md](../CLAUDE.md) first (the
gate, delivery rules), then work the milestones in order, each ending in a
runnable acceptance check. Every identifier in §2 was copied from the code
on 2026-08-02 with the file it came from — re-verify against that file
before building on it, in case it moved. §10 is the non-goals list; if a
step seems to require doing something in it, stop and flag it instead of
improvising. Check [STATUS.md](../STATUS.md) off as milestones land.
⚠ Multiple sessions work this repo concurrently — before committing, re-check
`main` and expect collisions in migration numbers and STATUS.md.

---

## 1. Objective — two views, real times, nothing broken behind you

1. **The month grid, redesigned.** Entries carry state (on disk · missing ·
   unaired · unmonitored) as color, not prose; air times appear on the chip;
   a full day overflows into a "+N more" panel instead of stretching the
   row; a legend replaces the explainer paragraph.
2. **A new Agenda view.** A chronological, scrollable list opening pinned at
   Today: sticky day headers, poster thumbnails, `S01E03 — Title` lines,
   `HBO · 9:00 PM` lines, status pills. Scrolls forward through coming
   weeks, backward through the recent past, loading more as you go. The
   phone layout (web) and the native app both become this.
3. **Real air times underneath.** Episodes today have a bare `air_date`
   (TMDB publishes no clock times). TVmaze — already an adapter, already in
   the series chain (ADR 0011), keyless — publishes a show's schedule, network
   and timezone. Series refresh stores them; the API composes a UTC instant
   per entry; every client renders it in local time. Zero configuration.

Four properties decide whether this was built correctly:

1. **The API change is strictly additive.** plurx reads `/calendar` in
   production (§2.1) and the native app ships separately from the server.
   Every existing field keeps its exact meaning; `detail` keeps its exact
   format. New data arrives only in new, optional fields.
2. **Times are honest.** A clock time appears only when TVmaze states the
   schedule; a show without one (streaming drops, most movies, books) stays
   a date-only entry, visibly, rather than getting midnight guessed onto it.
3. **It costs nothing new when nobody is looking.** Enrichment rides the
   existing `metadata.refresh` path — no new job, no new table. Calendar
   reads stay the same two indexed queries, now selecting more columns.
4. **Nothing existing goes red.** `phase3-automation.spec.ts` and
   `mobile.spec.ts` pin today's class names and behaviours (§2.8); the
   redesign keeps those contracts where they are load-bearing.

---

## 2. Contract — what exists today, verbatim

### 2.1 The API type and its external consumer (`internal/infra/sqlite/automation.go:235`)

```go
type CalendarEntry struct {
	Date        string `json:"date"` // ISO date
	Kind        string `json:"kind"` // episode | movie | book
	MediaItemID int64  `json:"mediaItemId"`
	Title       string `json:"title"`
	Detail      string `json:"detail"` // "S01E03 — Pilot", "by Author", ""
	HasFile     bool   `json:"hasFile"`
	TmdbID int64  `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
}
```

`tmdbId`/`imdbId` were added *for an external consumer*:
`internal/api/callers.go:16` — "plurx pushes watch state and reads the
calendar" — and `internal/api/edges_api_test.go:361` registers
`plurx/0.4.1 → /api/v1/calendar`. plurx resolves entries by id and reads
`detail` as an opaque label. **Additive changes only; `detail` is frozen.**
The mobile app (`mobile/src/types.ts:235`) duplicates this interface minus
the ids.

### 2.2 The two queries (`internal/infra/sqlite/queries/automation.sql:30`)

```sql
-- name: ListEpisodesAiring :many
SELECT e.id, e.media_item_id, e.season_number, e.episode_number,
       e.title AS episode_title, e.air_date, m.title AS series_title,
       m.tmdb_id, m.imdb_id,
       EXISTS(SELECT 1 FROM media_file_episodes mfe WHERE mfe.episode_id = e.id) AS has_file
FROM episodes e JOIN media_items m ON m.id = e.media_item_id
WHERE e.air_date >= ? AND e.air_date <= ?
ORDER BY e.air_date, m.title, e.season_number, e.episode_number;

-- name: ListItemsReleasedBetween :many
SELECT m.id, m.kind, m.title, m.author, m.release_date, m.tmdb_id, m.imdb_id,
       EXISTS(SELECT 1 FROM media_files f WHERE f.media_item_id = m.id) AS has_file
FROM media_items m
WHERE m.kind != 'series' AND m.release_date >= ? AND m.release_date <= ?
ORDER BY m.release_date, m.title;
```

Merged and sorted by `DB.Calendar` (`automation.go:252`), served by
`GetCalendar` (`internal/api/acquisition_handlers.go:758`), specced at
`internal/api/openapi.yaml:3656`, pinned by
`internal/api/surface_test.go:40`. Neither query selects `monitored`,
`poster_path`, `runtime`, or anything airing-related — that is §4's work.
⚠ sqlc traps (both hit before): keep `queries/*.sql` comments ASCII, and
list columns explicitly — never `SELECT *` in new queries.

### 2.3 The schema being extended (`internal/infra/sqlite/migrations/0002_library.sql`)

`episodes.air_date TEXT NOT NULL DEFAULT ''` — ISO date, no time (line 65).
`media_items` already has `poster_path`, `runtime`, `status`,
`release_date`, `monitored` (lines 29–36). Latest migration is
`0027_download_priority.sql`; **0021 exists only as Go**
(`internal/infra/sqlite/migration0021.go`) — when numbering a migration,
check BOTH `migrations/*.sql` and `migration00NN.go`, goose panics on a
duplicate version.

### 2.4 The TVmaze adapter (`internal/adapters/tvmaze/client.go`)

Keyless, implements `ports.SeriesProvider`, wired at
`cmd/monarr/main.go:153` and attached via
`library.New(…).WithSeriesProviders(seriesChain)` (main.go:159). Its `get`
(client.go:76) already carries the rate limiter (TVmaze asks ≤20 req/10 s)
and a URL-keyed response cache. `GetSeriesByTVDB` (client.go:200) is the
existing id-keyed lookup (`/lookup/shows?thetvdb=`). The `show` struct
(client.go:130) does **not** yet decode `schedule`, `network`,
`webChannel`, or the network timezone — §3 adds them. The real API shape:

```json
{ "schedule": { "time": "21:00", "days": ["Sunday"] },
  "network": { "name": "HBO",
               "country": { "timezone": "America/New_York" } },
  "webChannel": null }
```

Streaming shows invert it: `network` null, `webChannel` `{"name":"Netflix",
"country":null}`, `schedule.time` usually `""`.

### 2.5 The enrichment precedent to copy (`internal/app/library/library.go:103`)

```go
func (s *Service) WithSeriesProviders(p ...ports.SeriesProvider) *Service  // :103
func (s *Service) WithRatings(rp ports.RatingsProvider) *Service          // :109
```

`WithRatings` + `enrichRatings` (same file) is the exact shape air-time
enrichment copies: an optional builder-attached provider, consulted inside
`RefreshItem` (library.go:479), **best-effort by contract** — no provider,
no id, or an upstream error leaves the item as it was and never fails the
refresh. `RefreshAll` (library.go:667) is the `metadata.refresh` task, so
existing libraries backfill on their normal cadence (or a manual Refresh).

### 2.6 The page being replaced (`web/src/pages/Calendar.tsx`, 150 lines)

One component: month-grid on desktop, a bare agenda (days with entries
only, month-paged) under `useIsMobile()` (`web/src/useIsMobile.ts`,
breakpoint 767px). Data via `getCalendar(start, end)` (`web/src/api.ts:924`)
fetching the padded month grid range. CSS: `.cal-grid`/`.cal-cell`/
`.cal-entry` at `web/src/styles.css:803–889`, `.cal-agenda` at :1769.
Entry chips are outlined, filled when `hasFile` — state beyond that is a
`title` tooltip. Design tokens available (styles.css `:root`): `--bg`,
`--bg-raised`, `--bg-hover`, `--border`, `--text`, `--text-muted`,
`--accent`, `--accent-soft`, `--on-accent`, `--ok`, `--warning`, `--error`.
⚠ Theme overrides live in **two more blocks** — `:root:not([data-theme='dark'])`
(:24) and `:root[data-theme='light']` (:42); any new token goes in all three.

### 2.7 Poster plumbing, both clients

`posterUrl(path, size)` — `web/src/api.ts:802` and `mobile/src/format.ts:3`,
identical: absolute URLs (Open Library book covers) pass through, TMDB
paths get `https://image.tmdb.org/t/p/<size>` prefixed. Sizes `w185`,
`w342`, `w500`. `.poster-fallback` (first letter on a tile) is the missing-
artwork idiom on the web; `Poster` in `mobile/src/components/Media.tsx` is
the native one.

### 2.8 The tests that pin today's behaviour

- `test/e2e/tests/phase3-automation.spec.ts:19` — API: window
  `2020-01-01..2020-01-31` returns 2 episode entries of "The Test Show".
  `:117` — `/calendar` shows `.cal-dow` ×7 and a visible `.cal-cell`.
- `test/e2e/tests/mobile.spec.ts:34` — phone viewport: `.cal-agenda`
  visible, `.cal-grid` count 0.
- Fixture air dates are **in the past** (`test/e2e/fake-tmdb.mjs:44` —
  2020-01-01/08): a rolling-from-today agenda shows none of them, which is
  why §5.1 gives the page a `date` URL param as its test seam.
- The fake TVmaze already exists **inside fake-tmdb.mjs** (:186–:234,
  shared port; `MONARR_TVMAZE_BASE_URL` at `playwright.config.ts:89`) — add
  schedule/network fields to its show objects, not a new fake.
- Playwright traps (all hit before): spec files run **lexically** (name the
  new spec `zz-…`); specs are `mode: 'serial'` per file (read the FIRST
  failure); by-role name matching is **substring and app-wide** (keep new
  control accessible names to two words, sentences go in `title`); the
  suite is mildly flaky under load — re-run a lone failure, and never run
  `make test` and `make test-e2e` concurrently.

### 2.9 The native screen (`mobile/src/screens/CalendarScreen.tsx`, 75 lines)

Already an agenda (7 days back · 60 ahead, grouped by date, `ScrollView`),
but text-only rows: title, `detail`, an On disk/Missing `Badge`. No
posters, no times, no incremental loading. Mobile gate: `cd mobile && npm
run check` (typecheck + vitest) — there is no e2e for the native app.

---

## 3. M1 — the data: air times and network, from TVmaze

### 3.1 ADR 0016 first

`docs/adr/0016-air-times.md`, one row appended to `docs/adr/README.md`.
The decision it records: **episode air times come from TVmaze's show-level
schedule, stored on the item, composed into a UTC instant per date at read
time.** Alternatives it must dismiss, each with the reason:

- *Per-episode TVmaze airstamps* — more precise, but mapping TVmaze episode
  rows onto TMDB-numbered episodes crosses the numbering fault line ADR
  0011 §5 exists to avoid; a wrong-episode timestamp is worse than none.
- *Trakt's `airs` object* — same data, but gated on a configured client id;
  TVmaze gives it to every install for free, and is already in the chain.
- *Storing a computed UTC per episode at refresh* — breaks twice: DST (a
  9 PM show is a different UTC offset in January than July, composing per
  date handles that for free) and timeslot moves (show changes nights →
  one field update fixes every future episode).
- *TheTVDB* — paid key, per ADR 0011.

### 3.2 Migration 0028 — three columns

```sql
ALTER TABLE media_items ADD COLUMN airs_time     TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN airs_timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN network       TEXT NOT NULL DEFAULT '';
```

`airs_time` is `"21:00"` (24 h, as TVmaze sends it), `airs_timezone` an
IANA name, `network` a display name ("HBO", "Netflix"). Empty = unknown,
matching every other metadata-cache column in the table. Number it against
whatever `main` holds at build time (§2.3's dual-numbering check — 0028 is
correct as of this writing). `domain.MediaItem` gains the three fields;
the upsert path `internal/infra/sqlite/library.go:551` and its queries gain
the columns (explicit column lists, §2.2 traps).

### 3.3 The port and the TVmaze method

```go
// internal/ports/metadata.go (beside RatingsProvider)
type Airing struct {
	Time     string // "21:00", 24h, "" = unknown
	Timezone string // IANA name, "" = unknown
	Network  string // display name, "" = unknown
}
type AiringProvider interface {
	Airing(ctx context.Context, tvdbID int64, imdbID string) (Airing, error)
}
```

The TVmaze implementation extends the `show` struct (§2.4) with `schedule`,
`network`, `webChannel`, decodes both network shapes, and resolves by TVDB
id first (`/lookup/shows?thetvdb=`), IMDb id second (`?imdb=`) — both
already the adapter's request/cache/rate-limit path, one GET per refresh in
practice. Rules the fixtures must cover: `network` present → name +
`country.timezone`; `webChannel` show → name only, timezone stays `""`
(streaming has no broadcast instant — honest date-only, §1 property 2);
`schedule.time: ""` → `Time` empty, network still returned; neither id
known → zero value, no request. Add a `DiscoverProvider`-style row for
`AiringProvider` to `internal/ports/README.md`.

### 3.4 Enrichment inside refresh

`library.Service` gains `.WithAiring(p ports.AiringProvider)` and an
`enrichAiring` called from `RefreshItem` for `kind == series`, copying
`enrichRatings` line for line in spirit (§2.5): best-effort, logs at debug
on miss, **never fails the refresh**. Wire in `cmd/monarr/main.go` reusing
the existing `seriesChain` TVmaze client (one client, one cache, one rate
limiter — do not construct a second). Backfill is the normal
`metadata.refresh` cadence; no new job (§10).

**Acceptance:** `go test ./internal/adapters/tvmaze/ ./internal/app/library/
./internal/infra/sqlite/` green, with httptest fixtures for the three
airing shapes (broadcast, webChannel, no-schedule) and a refresh test
proving an airing error leaves the item refreshed and the fields empty.
`go test ./internal/ -run TestArchitecture` still passes.

---

## 4. M2 — the API: additive fields, UTC composed at read time

### 4.1 Queries and storage

`ListEpisodesAiring` additionally selects `e.monitored, m.poster_path,
m.runtime, m.network, m.airs_time, m.airs_timezone`;
`ListItemsReleasedBetween` additionally selects `m.poster_path, m.runtime,
m.monitored`. `sqlite.CalendarEntry` (§2.1) grows matching fields — every
existing field untouched.

### 4.2 The wire type

`openapi.yaml`'s `CalendarEntry` gains **optional** properties (then
`make gen`; the interface won't compile until the handler is updated,
which is the point):

| Field | Type | Set for | Meaning |
|---|---|---|---|
| `posterPath` | string | all kinds | as the library serves it: TMDB-relative or absolute (books) |
| `airDateUtc` | string (date-time) | episodes | exact UTC instant; **absent when the schedule is unknown** |
| `network` | string | episodes | "HBO", "Netflix" — display only |
| `seasonNumber` / `episodeNumber` | integer | episodes | what `detail` encodes, machine-readable |
| `episodeTitle` | string | episodes | "" when TMDB has none |
| `runtime` | integer | all kinds | minutes, 0 = unknown |
| `monitored` | boolean | all kinds | episode's flag for episodes, item's otherwise |

`date` and `detail` keep their exact current values — plurx (§2.1) and the
shipped mobile app parse today's shape. Absent-when-unknown follows the
`tmdbId` precedent in the same struct (a zero would collide with real
unknowns).

### 4.3 Composing `airDateUtc`

In the handler (`GetCalendar`), presentation logic beside the other
apigen mapping: `air_date` + `airs_time` parsed in `airs_timezone` via
`time.LoadLocation`, rendered RFC3339 UTC. Composing per date is what makes
DST correct (§3.1). Cache `*time.Location` in a small package-level map —
`LoadLocation` reads the disk each call. Any piece missing or unparsable →
omit the field, never guess. **Add `import _ "time/tzdata"` to
`cmd/monarr/main.go`**: the deploy image is minimal and a container without
`/usr/share/zoneinfo` would otherwise silently strip every time.

**Acceptance:** `make gen && git diff --exit-code -- internal/api/gen`
clean after committing the regenerated file; a handler unit test composing
a January date and a July date for `21:00 America/New_York` asserts both
UTC instants (02:00Z vs 01:00Z — the DST proof); `internal/api`
surface/edges tests still green; `curl '/api/v1/calendar?start=…&end=…'`
shows old fields byte-identical for an unenriched item.

---

## 5. M3 — the web page: the month redone, the agenda new

One file rewrite (`web/src/pages/Calendar.tsx` — it may split into
`Calendar.tsx` + a shared `CalendarEntryRow` used by agenda, day panel and
mobile) plus the `.cal-*` CSS blocks. No router registration changes — the
route exists; it gains `validateSearch` for two params.

### 5.1 The view switch and the two URL params

A two-word segmented control in the page head — **Month · Agenda** — on the
Library page's `.tabs` idiom. Choice persists in `localStorage`
(`monarr-cal-view`); desktop defaults to Month, below 768px the page is
always Agenda (the toggle hides — a 7-column grid at 390px is what the old
code already refused to render). Two search params, both optional:
`?view=month|agenda` (overrides the stored choice — deep links and e2e)
and `?date=YYYY-MM-DD` (anchors the month shown / the day the agenda opens
at, instead of today). The params are the test seam for §7's fixture-dates
problem; keep them working.

### 5.2 Month grid — state as color, density handled

Keep the classes the tests pin (`.cal-grid`, `.cal-dow`, `.cal-cell`,
`.cal-agenda` — §2.8). Within a cell:

- **Entry chips carry state, not just existence.** Four visual states, in
  priority order: *on disk* (filled, `--ok`-tinted), *aired & missing*
  (filled, `--error`-tinted), *unaired* (outlined `--accent` — today's
  default look), *unmonitored* (any of the above at reduced opacity, no
  strike-through — the title must stay readable). Soft background variants:
  add `--ok-soft` and `--error-soft` beside `--accent-soft` in **all three**
  token blocks (§2.6).
- **Kind is a glyph, not a color**: a small leading dot/icon distinguishes
  movie ▸ and book ▪ from episodes (no glyph — the common case stays
  quiet). Color stays reserved for state, one axis per channel.
- **Time on the chip** when `airDateUtc` is present: compact local time
  ("9 PM", "9:30 PM") before the title, via `Intl.DateTimeFormat`.
- **Overflow**: a cell renders at most 4 chips, then one `+N more` button.
  Clicking it opens a day panel on the existing `.modal-backdrop`/`.modal`
  pattern: the full day as rich rows (the shared row component from §5.3).
  Equal-height weeks: `grid-auto-rows: minmax(112px, auto)` so one crowded
  Thursday doesn't stretch its whole week.
- **A legend** (four state swatches + two kind glyphs, one line, muted)
  replaces the current explainer paragraph under the grid.
- Today's `.cal-daynum` accent circle stays; add a faint `--accent-soft`
  cell wash so the eye finds the week too.

### 5.3 Agenda — the rolling list

**Window.** State is `{start, end}`, initialised `anchor − 7 days` to
`anchor + 45 days` (anchor = `?date` or today), one `getCalendar` query for
the whole window (react-query key `['calendar', start, end]` — widening
refetches; payloads are rows, not posters, this is cheap). A sentinel
`useInView` (`web/src/useInView.ts` exists) near the bottom extends `end`
+30 days; a **Load earlier** button at the top extends `start` −30 days — a
button, not an observer, because auto-prepending fights scroll anchoring
and yanks the list out from under the reader.

**Day groups.** Sticky day headers (position: sticky against the content
scroll): `Today — Sunday, August 2` / `Tomorrow — Monday, August 3` /
weekday+date otherwise, year appended only when ≠ current. Today's header
renders even with no entries ("Nothing today", muted) so the anchor and the
**Today** button (page head, `scrollIntoView`) always have a target.
Within a day: date-only entries (movies, books, no-schedule episodes)
first, then timed episodes by time — the reader scans "what's out today,
then tonight in order".

**Rows** (the shared component — agenda, day panel, and the model for the
native row):

```
┌──────┬────────────────────────────────────────────┬──────────────┐
│poster│ The Rehearsal                    (link)     │      9:30 PM │
│ w185 │ S02E04 — Episode title · 34 min             │ HBO          │
│56×84 │                                             │ [ On disk ]  │
└──────┴────────────────────────────────────────────┴──────────────┘
```

Poster via `posterUrl(p, 'w185')`, `.poster-fallback` letter tile when
empty. Line 2 for movies: `Movie · 2026 · 118 min`; books:
`Book · by Author`. Right rail: local time (or the date, in the day-panel
context) over network, then the status pill — `pill-ok` "On disk",
`pill-error` "Missing" only when aired, `pill-outline` "Upcoming"
otherwise; unmonitored rows at reduced opacity with a muted "Not monitored"
in line 2. Whole row links to `/library/$id` like today's chips.

**States.** Initial load: 6 skeleton rows (follow `.discover-skeleton`'s
pattern — and its e2e trap: skeletons are *replaced*, measure
`:not(.skeleton)`). Error: the existing `.banner.warning`. Empty window:
one friendly line plus the Load earlier / keep-scrolling affordances.

**Mobile web** is this same agenda full-width — the old bare `.cal-agenda`
markup is deleted, its class name living on as the new list's container
(that keeps `mobile.spec.ts:34` meaningful and green).

### 5.4 Unit tests (vitest, `web/src/calendar.test.ts`)

Extract the pure logic into `web/src/calendar.ts` so it's testable without
DOM: day-grouping and within-day sort (date-only first, then by time);
window-widening math; header labels (Today/Tomorrow/weekday/year rules)
against a **fixed** date; time formatting from `airDateUtc` with an
explicit `timeZone` option so CI's UTC and a laptop's local tz agree.

**Acceptance:** `make test-web` green; `npm run typecheck` clean; existing
`.cal-*` e2e selectors (§2.8) untouched by grep.

---

## 6. M4 — the native app: CalendarScreen rebuilt

`mobile/src/types.ts` `CalendarEntry` gains the §4.2 fields, all optional —
the app must render against an **old server** exactly as it does today
(same reason the server must serve an old app; the two ship separately).

The screen (`mobile/src/screens/CalendarScreen.tsx`) becomes a
`SectionList`: one section per day, sticky section headers with the §5.3
label rules (`localDateKey` + the existing `formatDate` grow, don't fork),
rows following the §5.3 anatomy — `Poster` from
`mobile/src/components/Media.tsx` (export it if it isn't), title, the
kind/episode/runtime line, time + network when present, the existing
`Badge` for status. Within-day sort identical to web (put the comparator in
`mobile/src/format.ts` beside its test). `onEndReached` extends the window
forward +30 days; the header keeps stating the loaded range ("7 days back ·
75 ahead") because an open-ended list with an invisible horizon reads as
"the app lost my show". Pull-to-refresh via `RefreshControl` on the
existing `useResource.refresh`.

**Acceptance:** `cd mobile && npm run check` green (typecheck + vitest,
including new comparator/label tests). **Not verified here:** no e2e
exists for the native app; a device smoke test (Expo, §2.9's README) is
the owner's step and the handoff must say so.

---

## 7. M5 — e2e: fixtures gain schedules, specs gain the new surfaces

**Fixtures.** In `test/e2e/fake-tmdb.mjs`, give the fake-TVmaze show for
"The Test Show" (§2.8) `schedule: { time: '21:00', days: ['Wednesday'] }`
and `network: { name: 'E2E One', country: { timezone: 'America/New_York' } }`;
leave a second show schedule-less so both render paths exist. Air dates
stay in 2020 — the `?date` param (§5.1) is how specs reach them.

**Existing specs.** `phase3-automation.spec.ts:19` (API shape) must still
pass untouched — that is the additive-API proof. `:117` and
`mobile.spec.ts:34` keep passing via the preserved class names (§5.2,
§5.3); extend `:117` to also assert the view toggle exists rather than
writing a competing spec.

**New spec** `test/e2e/tests/zz-calendar.spec.ts` (`zz-` for the lexical
ordering trap, §2.8; set its own state in `beforeAll` like `smoke.spec.ts`
rather than depending on earlier files):

- `/calendar?view=agenda&date=2020-01-01` → a day header for Jan 1, a row
  with a poster block (or fallback tile), "S01E01", and an **On disk**
  pill; the schedule-bearing show's row shows a time matching
  `/\d{1,2}:\d{2}\s?(AM|PM)/` (regex, not a literal — the runner's tz is
  not pinned) and "E2E One".
- Times appear only after enrichment: run the `metadata.refresh` task via
  the API (the `runTask` helper pattern, `phase3-automation.spec.ts:14`)
  in `beforeAll` before asserting.
- Month view: a `+N more` cell opens the day panel (seed one day with 5+
  entries via the fake's episode list); state classes present for an
  on-disk vs missing entry.
- The toggle: switch to Agenda, reload, still Agenda (localStorage);
  `?view=month` overrides.

**Acceptance:** `make test-e2e` green, full suite — and remember the
standing rules: re-run a lone failure before believing it, never
concurrently with `make test`.

---

## 8. M6 — docs, version, ledger

Per [CLAUDE.md](../CLAUDE.md) §3, in the same delivery:

- `docs/usage.md` — the calendar part of "Activity, calendar, wanted"
  (usage.md:589) rewritten for both views, the state colors, where times
  come from (TVmaze, keyless, "streaming and movies are date-only — that is
  the source being honest, not a bug") and the two URL params. The phone
  paragraph (usage.md:183) updated — the agenda is now the rich list.
- `docs/adr/README.md` — the 0016 row (§3.1).
- `README.md` — the Phase 3 feature-map line mentions the agenda view; no
  new screenshot obligations, but if screenshots exist for the calendar
  they are now stale — flag rather than silently ship them.
- `mobile/README.md` — one line under "What the app owns" if the wording
  no longer covers the richer calendar.
- `VERSION` — minor bump (`make release BUMP=minor` at ship time is the
  owner's call; the bump itself lands with the change).
- `STATUS.md` — check off / append under the current phase snapshot, and
  this file gains `**Status:** built <date>` at the top.

**Acceptance:** docs name the same behaviour the code has — spot-check the
three claims above against the running app, not the plan.

---

## 9. Order of work, and the gate

§3 → §4 → §5 ∥ §6 → §7 → §8. The two clients (§5, §6) are independent once
§4's shapes are in the spec; each can start against a hand-written JSON
fixture. Nothing ships until the full gate passes on the delivered code
([CLAUDE.md](../CLAUDE.md) §1 — "it builds" is not verification):

```bash
make lint        # golangci-lint + gofmt
make test        # go test ./...
make test-web    # vitest
make test-e2e    # playwright, full suite
cd mobile && npm run check   # the native app's whole gate
```

Delivery is files in the working tree with explicit `git add` paths
(CLAUDE.md §2 — never `git add -A`; concurrent sessions may have their own
work in the tree).

---

## 10. Non-goals — do not do these

- **Do not change or reorder existing `CalendarEntry` fields, and do not
  touch `detail`'s format.** plurx parses it in production (§2.1). New data
  = new optional fields, only.
- **Do not map TVmaze episodes onto TMDB episodes.** Per-episode airstamps
  are the tempting version of this feature and they cross ADR 0011 §5's
  numbering fault line. Show-level schedule only, composed per date.
- **Do not store computed UTC instants.** DST and timeslot moves are why
  composition happens at read time (§3.1). If a step seems to need a
  stored timestamp, stop and flag it.
- **Do not add a settings key, a scheduled job, or a cache table.** TVmaze
  is keyless; enrichment rides `metadata.refresh`; the adapter's response
  cache already exists. The only schema change is migration 0028.
- **Do not fetch anything per-calendar-request.** The calendar renders
  from the two queries alone; enrichment happens at refresh time. A page
  load that talks to TVmaze has gone wrong.
- **Do not put posters in the month grid cells.** Chips are the density
  budget; artwork lives in the agenda, the day panel, and detail pages. A
  month of thumbnails is a wall, not a calendar.
- **Do not guess times.** No airing data → no `airDateUtc` → a date-only
  row. Midnight-looking times that mean "unknown" poison the sort and the
  reader's trust.
- **Do not add a week/day view or an iCal feed.** Real candidates, separate
  plans; iCal in particular has an external-consumer contract worth its own
  ADR. Trigger: someone asks to subscribe from Google Calendar/Outlook.
- **Do not touch `internal/compat`.** The Sonarr/Radarr personalities have
  no `/calendar` today; adding one is compat surface work, not this.
- **Do not rename the pinned classes** (`.cal-grid`, `.cal-dow`,
  `.cal-cell`, `.cal-agenda`) — two spec files and muscle memory hold them.

