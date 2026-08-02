// Pure calendar logic — grouping, ordering, labelling, window arithmetic.
//
// It lives outside the component so it can be tested against fixed dates
// without a DOM, and so the month grid, the agenda and the day panel all
// order and label entries the same way. Nothing here touches React or the
// network.
import type { CalendarEntry } from './api'

/** iso renders a Date as YYYY-MM-DD in LOCAL time. Local, not UTC: the
 *  calendar's unit is the day a person is looking at, and toISOString would
 *  put an 8 PM entry on tomorrow for anyone west of Greenwich. */
export function iso(d: Date): string {
  return (
    `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}` +
    `-${String(d.getDate()).padStart(2, '0')}`
  )
}

/** parseISODate reads YYYY-MM-DD as a LOCAL midnight, which is what every
 *  date-keyed comparison here assumes. `new Date('2020-01-01')` is UTC
 *  midnight and lands on the previous day in the Americas. */
export function parseISODate(s: string): Date {
  const [y, m, d] = s.slice(0, 10).split('-').map(Number)
  return new Date(y, (m ?? 1) - 1, d ?? 1)
}

export function addDays(d: Date, n: number): Date {
  const out = new Date(d)
  out.setDate(out.getDate() + n)
  return out
}

/** The four states an entry can be in, in priority order. State is the only
 *  thing carried by color; `kind` gets a glyph instead, so the two never
 *  compete for the same channel. */
export type EntryState = 'have' | 'missing' | 'upcoming'

export function entryState(e: CalendarEntry, now = new Date()): EntryState {
  if (e.hasFile) return 'have'
  // "Missing" is a claim about something that already happened. An episode
  // that airs tonight is not missing, it is upcoming, and coloring it red
  // would make a healthy library look permanently broken.
  return hasAired(e, now) ? 'missing' : 'upcoming'
}

export function hasAired(e: CalendarEntry, now = new Date()): boolean {
  if (e.airDateUtc) return new Date(e.airDateUtc).getTime() <= now.getTime()
  // Date-only: the day is over when the next day starts, locally.
  return addDays(parseISODate(e.date), 1).getTime() <= now.getTime()
}

/** unmonitored is its own axis — an unmonitored entry is still news, it is
 *  just not being hunted, so it dims rather than changing color. `monitored`
 *  is absent on an older server, where everything read as monitored. */
export function isUnmonitored(e: CalendarEntry): boolean {
  return e.monitored === false
}

/** Within a day: date-only entries first, then timed ones in time order.
 *  That is the order a reader wants — "what's out today, then tonight in
 *  order" — and it keeps schedule-less entries from being scattered through
 *  the evening by a time they do not have. */
export function compareEntries(a: CalendarEntry, b: CalendarEntry): number {
  const at = a.airDateUtc ? Date.parse(a.airDateUtc) : NaN
  const bt = b.airDateUtc ? Date.parse(b.airDateUtc) : NaN
  const aHas = !Number.isNaN(at)
  const bHas = !Number.isNaN(bt)
  if (aHas !== bHas) return aHas ? 1 : -1
  if (aHas && bHas && at !== bt) return at - bt
  if (a.title !== b.title) return a.title < b.title ? -1 : 1
  return (a.seasonNumber ?? 0) - (b.seasonNumber ?? 0) ||
    (a.episodeNumber ?? 0) - (b.episodeNumber ?? 0)
}

export interface DayGroup {
  key: string // YYYY-MM-DD
  date: Date
  entries: CalendarEntry[]
}

/** groupByDay buckets entries onto local days and sorts within each.
 *  `todayKey`, when given, is always present in the result even with nothing
 *  on it — the agenda's Today button and its scroll anchor both need a row
 *  to aim at, and "Nothing today" is information. */
export function groupByDay(entries: CalendarEntry[], todayKey?: string): DayGroup[] {
  const byDay = new Map<string, CalendarEntry[]>()
  for (const e of entries) {
    const key = e.date.slice(0, 10)
    const list = byDay.get(key)
    if (list) list.push(e)
    else byDay.set(key, [e])
  }
  if (todayKey && !byDay.has(todayKey)) byDay.set(todayKey, [])
  return [...byDay.keys()]
    .sort()
    .map((key) => ({
      key,
      date: parseISODate(key),
      entries: (byDay.get(key) ?? []).slice().sort(compareEntries),
    }))
}

const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
export const MONTHS = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
]

/** dayLabel names a day the way a person would: Today and Tomorrow by name,
 *  everything else by weekday and date, and the year only when it is not the
 *  current one (which is what makes a 2020 fixture window readable). */
export function dayLabel(key: string, now = new Date()): string {
  const d = parseISODate(key)
  const todayKey = iso(now)
  const long = `${WEEKDAYS[d.getDay()]}, ${MONTHS[d.getMonth()]} ${d.getDate()}` +
    (d.getFullYear() !== now.getFullYear() ? `, ${d.getFullYear()}` : '')
  if (key === todayKey) return `Today — ${long}`
  if (key === iso(addDays(now, 1))) return `Tomorrow — ${long}`
  if (key === iso(addDays(now, -1))) return `Yesterday — ${long}`
  return long
}

/** formatTime renders an entry's air time in the reader's local zone.
 *  `timeZone` is injectable so a test can assert a fixed string instead of
 *  whatever zone CI happens to run in. Minutes are dropped on the hour, the
 *  way a broadcast schedule is spoken: "9 PM", "9:30 PM". */
export function formatTime(airDateUtc: string | undefined, timeZone?: string): string {
  if (!airDateUtc) return ''
  const t = new Date(airDateUtc)
  if (Number.isNaN(t.getTime())) return ''
  const parts = new Intl.DateTimeFormat(undefined, {
    hour: 'numeric',
    minute: '2-digit',
    timeZone,
  }).formatToParts(t)
  const get = (type: string) => parts.find((p) => p.type === type)?.value ?? ''
  const hour = get('hour')
  const minute = get('minute')
  const period = get('dayPeriod')
  const clock = minute === '00' ? hour : `${hour}:${minute}`
  return period ? `${clock} ${period}` : clock
}

/** The agenda's second line, per kind: what a reader needs to place the row
 *  once the title has told them what it is. */
export function entrySubtitle(e: CalendarEntry): string {
  const bits: string[] = []
  if (e.kind === 'episode') {
    const code =
      e.seasonNumber !== undefined && e.episodeNumber !== undefined
        ? `S${String(e.seasonNumber).padStart(2, '0')}E${String(e.episodeNumber).padStart(2, '0')}`
        : ''
    // detail already reads "S01E03 — Pilot" on every server; prefer the
    // structured fields when the server is new enough to send them.
    const titled = e.episodeTitle ? `${code} — ${e.episodeTitle}` : code
    bits.push(titled || e.detail)
  } else {
    bits.push(e.kind === 'book' ? 'Book' : 'Movie')
    if (e.detail) bits.push(e.detail)
  }
  if (e.runtime) bits.push(`${e.runtime} min`)
  return bits.filter(Boolean).join(' · ')
}

/** The month grid's span: whole weeks around the anchor's month, so every
 *  row has seven cells. */
export function monthGrid(anchor: Date): { start: Date; end: Date; days: Date[] } {
  const first = new Date(anchor.getFullYear(), anchor.getMonth(), 1)
  const start = addDays(first, -first.getDay())
  const last = new Date(anchor.getFullYear(), anchor.getMonth() + 1, 0)
  const end = addDays(last, 6 - last.getDay())
  const days: Date[] = []
  for (let d = new Date(start); d <= end; d = addDays(d, 1)) days.push(new Date(d))
  return { start, end, days }
}

/** The agenda's initial window, and how it grows. Backwards is a button
 *  rather than an observer: auto-prepending fights scroll anchoring and
 *  yanks the list out from under whoever is reading it. */
export const AGENDA_BACK_DAYS = 7
export const AGENDA_AHEAD_DAYS = 45
export const AGENDA_STEP_DAYS = 30

export interface Window {
  start: string
  end: string
}

export function initialWindow(anchor: Date): Window {
  return {
    start: iso(addDays(anchor, -AGENDA_BACK_DAYS)),
    end: iso(addDays(anchor, AGENDA_AHEAD_DAYS)),
  }
}

export function extendForward(w: Window, days = AGENDA_STEP_DAYS): Window {
  return { ...w, end: iso(addDays(parseISODate(w.end), days)) }
}

export function extendBackward(w: Window, days = AGENDA_STEP_DAYS): Window {
  return { ...w, start: iso(addDays(parseISODate(w.start), -days)) }
}

/** windowLabel states the loaded horizon. An open-ended list whose end is
 *  invisible reads as "the app lost my show". */
export function windowLabel(w: Window, anchor: Date): string {
  // Both ends measured from the anchor's DAY, not its clock: an anchor of
  // "now" carries a time of day that would otherwise round one end up.
  const day = parseISODate(iso(anchor)).getTime()
  const back = Math.round((day - parseISODate(w.start).getTime()) / 86_400_000)
  const ahead = Math.round((parseISODate(w.end).getTime() - day) / 86_400_000)
  return `${back} days back · ${ahead} ahead`
}

/** MONTH_CELL_CHIPS is the density budget for one month cell; the rest
 *  collapse into a "+N more" button rather than stretching the week. */
export const MONTH_CELL_CHIPS = 4
