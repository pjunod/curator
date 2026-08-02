import { describe, expect, it } from 'vitest'
import type { CalendarEntry } from './api'
import {
  compareEntries,
  dayLabel,
  entryState,
  entrySubtitle,
  extendBackward,
  extendForward,
  formatTime,
  groupByDay,
  initialWindow,
  iso,
  monthGrid,
  parseISODate,
  windowLabel,
} from './calendar'

// Every test pins its "now" — a calendar whose tests drift with the wall
// clock passes in April and fails in October.
const NOW = new Date(2026, 7, 2, 12, 0, 0) // Sunday, 2 August 2026, local noon

function entry(over: Partial<CalendarEntry> = {}): CalendarEntry {
  return {
    date: '2026-08-02',
    kind: 'episode',
    mediaItemId: 1,
    title: 'A Show',
    detail: 'S01E01',
    hasFile: false,
    ...over,
  }
}

describe('date helpers', () => {
  it('parses and renders ISO dates in local time', () => {
    // The bug this guards: new Date('2020-01-01') is UTC midnight, which is
    // 31 December anywhere west of Greenwich.
    expect(iso(parseISODate('2020-01-01'))).toBe('2020-01-01')
    expect(parseISODate('2020-01-01').getDate()).toBe(1)
  })
})

describe('entryState', () => {
  it('is have whenever the file exists, whatever the date', () => {
    expect(entryState(entry({ hasFile: true, date: '2030-01-01' }), NOW)).toBe('have')
  })
  it('is upcoming for something that has not aired yet', () => {
    expect(entryState(entry({ date: '2026-08-05' }), NOW)).toBe('upcoming')
  })
  it('is missing only once it has aired', () => {
    expect(entryState(entry({ date: '2026-07-01' }), NOW)).toBe('missing')
  })
  it('uses the exact instant when there is one', () => {
    // Airing tonight is not missing — it has not happened yet.
    const tonight = new Date(2026, 7, 2, 21, 0, 0).toISOString()
    expect(entryState(entry({ airDateUtc: tonight }), NOW)).toBe('upcoming')
    const earlier = new Date(2026, 7, 2, 9, 0, 0).toISOString()
    expect(entryState(entry({ airDateUtc: earlier }), NOW)).toBe('missing')
  })
  it("treats today's date-only entry as not yet aired", () => {
    expect(entryState(entry({ date: '2026-08-02' }), NOW)).toBe('upcoming')
  })
})

describe('within-day order', () => {
  it('puts date-only entries first, then timed ones in time order', () => {
    const nine = entry({ title: 'Nine', airDateUtc: '2026-08-03T01:00:00Z' })
    const eight = entry({ title: 'Eight', airDateUtc: '2026-08-03T00:00:00Z' })
    const movie = entry({ title: 'Zed Movie', kind: 'movie' })
    const book = entry({ title: 'Anthology', kind: 'book' })
    const got = [nine, movie, eight, book].sort(compareEntries).map((e) => e.title)
    expect(got).toEqual(['Anthology', 'Zed Movie', 'Eight', 'Nine'])
  })
  it('falls back to title, then season and episode', () => {
    const e2 = entry({ title: 'Show', seasonNumber: 1, episodeNumber: 2 })
    const e1 = entry({ title: 'Show', seasonNumber: 1, episodeNumber: 1 })
    expect([e2, e1].sort(compareEntries).map((e) => e.episodeNumber)).toEqual([1, 2])
  })
  it('ignores an unparsable instant rather than sorting on NaN', () => {
    const bad = entry({ title: 'Bad', airDateUtc: 'not a date' })
    const good = entry({ title: 'Good', airDateUtc: '2026-08-03T01:00:00Z' })
    expect([good, bad].sort(compareEntries).map((e) => e.title)).toEqual(['Bad', 'Good'])
  })
})

describe('groupByDay', () => {
  it('buckets by local day, sorted, oldest first', () => {
    const groups = groupByDay([
      entry({ date: '2026-08-04', title: 'Later' }),
      entry({ date: '2026-08-02', title: 'Sooner' }),
    ])
    expect(groups.map((g) => g.key)).toEqual(['2026-08-02', '2026-08-04'])
  })
  it('keeps an empty today so the anchor always has a target', () => {
    const groups = groupByDay([entry({ date: '2026-08-04' })], '2026-08-02')
    expect(groups.map((g) => g.key)).toEqual(['2026-08-02', '2026-08-04'])
    expect(groups[0].entries).toHaveLength(0)
  })
  it('does not duplicate a today that already has entries', () => {
    const groups = groupByDay([entry({ date: '2026-08-02' })], '2026-08-02')
    expect(groups).toHaveLength(1)
    expect(groups[0].entries).toHaveLength(1)
  })
})

describe('dayLabel', () => {
  it('names today, tomorrow and yesterday', () => {
    expect(dayLabel('2026-08-02', NOW)).toBe('Today — Sunday, August 2')
    expect(dayLabel('2026-08-03', NOW)).toBe('Tomorrow — Monday, August 3')
    expect(dayLabel('2026-08-01', NOW)).toBe('Yesterday — Saturday, August 1')
  })
  it('gives a plain weekday and date otherwise', () => {
    expect(dayLabel('2026-08-12', NOW)).toBe('Wednesday, August 12')
  })
  it('appends the year only when it is not the current one', () => {
    expect(dayLabel('2020-01-01', NOW)).toBe('Wednesday, January 1, 2020')
  })
})

describe('formatTime', () => {
  // timeZone is passed explicitly: CI runs in UTC and a laptop does not.
  it('drops the minutes on the hour', () => {
    expect(formatTime('2026-08-03T01:00:00Z', 'America/New_York')).toBe('9 PM')
  })
  it('keeps them otherwise', () => {
    expect(formatTime('2026-08-03T01:30:00Z', 'America/New_York')).toBe('9:30 PM')
  })
  it('is empty for an absent or unparsable instant', () => {
    expect(formatTime(undefined)).toBe('')
    expect(formatTime('sometime on Thursday')).toBe('')
  })
})

describe('entrySubtitle', () => {
  it('prefers the structured episode fields', () => {
    expect(
      entrySubtitle(entry({ seasonNumber: 2, episodeNumber: 4, episodeTitle: 'Pilot', runtime: 34 })),
    ).toBe('S02E04 — Pilot · 34 min')
  })
  it('falls back to detail when an old server sent no fields', () => {
    expect(entrySubtitle(entry({ detail: 'S01E01 — Pilot' }))).toBe('S01E01 — Pilot')
  })
  it('describes movies and books by kind', () => {
    expect(entrySubtitle(entry({ kind: 'movie', detail: '', runtime: 118 }))).toBe('Movie · 118 min')
    expect(entrySubtitle(entry({ kind: 'book', detail: 'by Ann Leckie' }))).toBe('Book · by Ann Leckie')
  })
})

describe('monthGrid', () => {
  it('spans whole weeks, so every row has seven cells', () => {
    const { days, start, end } = monthGrid(new Date(2026, 7, 15))
    expect(days.length % 7).toBe(0)
    expect(start.getDay()).toBe(0)
    expect(end.getDay()).toBe(6)
    // August 2026 starts on a Saturday: the grid opens on 26 July.
    expect(iso(start)).toBe('2026-07-26')
  })
})

describe('agenda window', () => {
  it('opens a week back and six weeks ahead of the anchor', () => {
    expect(initialWindow(NOW)).toEqual({ start: '2026-07-26', end: '2026-09-16' })
  })
  it('grows in either direction without moving the other end', () => {
    const w = initialWindow(NOW)
    expect(extendForward(w).end).toBe('2026-10-16')
    expect(extendForward(w).start).toBe(w.start)
    expect(extendBackward(w).start).toBe('2026-06-26')
    expect(extendBackward(w).end).toBe(w.end)
  })
  it('states the loaded horizon in days', () => {
    expect(windowLabel(initialWindow(NOW), NOW)).toBe('7 days back · 45 ahead')
  })
})
