import { describe, expect, it } from 'vitest'
import {
  calendarDayLabel,
  calendarStatus,
  calendarSubtitle,
  calendarTime,
  compareCalendarEntries,
  completeness,
  formatBytes,
  formatRating,
  formatUptime,
  groupCalendar,
  localDateKey,
  posterUrl,
} from './format'
import type { CalendarEntry, MediaItemSummary } from './types'

const base: MediaItemSummary = {
  id: 1,
  kind: 'movie',
  title: 'Fight Club',
  year: 1999,
  author: '',
  posterPath: '',
  monitored: true,
  path: '',
  rating: 8.4,
  ratingVotes: 10,
  ratings: [],
  episodeCount: 0,
  episodeFileCount: 0,
  fileCount: 0,
  addedAt: '2026-08-01T00:00:00Z',
}

describe('mobile formatting', () => {
  it('keeps absolute book covers and expands TMDB paths', () => {
    expect(posterUrl('https://covers.openlibrary.org/b/id/1-L.jpg')).toContain('openlibrary.org')
    expect(posterUrl('/poster.jpg', 'w185')).toBe('https://image.tmdb.org/t/p/w185/poster.jpg')
  })

  it('formats device-sized values compactly', () => {
    expect(formatBytes(1_610_612_736)).toBe('1.5 GB')
    expect(formatRating({ source: 'rt', value: 94, scale: 100 })).toBe('94%')
    expect(formatUptime(183_600)).toBe('2d 3h')
  })

  it('describes movie and series completeness from API counts', () => {
    expect(completeness(base)).toEqual({ label: 'Missing', tone: 'warning' })
    expect(completeness({ ...base, fileCount: 1, quality: 'WEB-DL 1080p' })).toEqual({
      label: 'WEB-DL 1080p',
      tone: 'ok',
    })
    expect(completeness({ ...base, kind: 'series', episodeCount: 10, episodeFileCount: 7 })).toEqual({
      label: '7/10 episodes',
      tone: 'warning',
    })
  })

  it('uses the device calendar rather than UTC for API date keys', () => {
    expect(localDateKey(new Date(2026, 7, 1, 23, 30))).toBe('2026-08-01')
  })
})

// ---- calendar (ADR 0016) ----

const NOW = new Date(2026, 7, 2, 12, 0, 0) // Sunday, 2 August 2026, local noon

function calEntry(over: Partial<CalendarEntry> = {}): CalendarEntry {
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

describe('compareCalendarEntries', () => {
  it('puts date-only entries first, then timed ones in time order', () => {
    const nine = calEntry({ title: 'Nine', airDateUtc: '2026-08-03T01:00:00Z' })
    const eight = calEntry({ title: 'Eight', airDateUtc: '2026-08-03T00:00:00Z' })
    const movie = calEntry({ title: 'Zed Movie', kind: 'movie' })
    const got = [nine, movie, eight].sort(compareCalendarEntries).map((e) => e.title)
    expect(got).toEqual(['Zed Movie', 'Eight', 'Nine'])
  })
  it('breaks ties on season and episode', () => {
    const two = calEntry({ seasonNumber: 1, episodeNumber: 2 })
    const one = calEntry({ seasonNumber: 1, episodeNumber: 1 })
    expect([two, one].sort(compareCalendarEntries).map((e) => e.episodeNumber)).toEqual([1, 2])
  })
})

describe('calendarDayLabel', () => {
  it('names today, tomorrow and yesterday', () => {
    expect(calendarDayLabel('2026-08-02', NOW)).toMatch(/^Today · /)
    expect(calendarDayLabel('2026-08-03', NOW)).toMatch(/^Tomorrow · /)
    expect(calendarDayLabel('2026-08-01', NOW)).toMatch(/^Yesterday · /)
  })
  it('names anything else plainly, with the year only when it differs', () => {
    expect(calendarDayLabel('2026-08-12', NOW)).not.toMatch(/2026/)
    expect(calendarDayLabel('2020-01-01', NOW)).toMatch(/2020/)
  })
})

describe('calendarTime', () => {
  // The zone is passed in: CI runs UTC and a phone does not.
  it('drops the minutes on the hour', () => {
    expect(calendarTime('2026-08-03T01:00:00Z', 'America/New_York')).toBe('9 PM')
  })
  it('keeps them otherwise', () => {
    expect(calendarTime('2026-08-03T01:30:00Z', 'America/New_York')).toBe('9:30 PM')
  })
  it('is empty when there is no instant', () => {
    expect(calendarTime(undefined)).toBe('')
    expect(calendarTime('thursday-ish')).toBe('')
  })
})

describe('calendarStatus', () => {
  it('is on disk whenever the file exists', () => {
    expect(calendarStatus(calEntry({ hasFile: true }), NOW).label).toBe('On disk')
  })
  it('does not call tonight’s episode missing', () => {
    const tonight = new Date(2026, 7, 2, 21, 0, 0).toISOString()
    expect(calendarStatus(calEntry({ airDateUtc: tonight }), NOW).label).toBe('Upcoming')
  })
  it('calls an aired episode with no file missing', () => {
    expect(calendarStatus(calEntry({ date: '2026-07-01' }), NOW).label).toBe('Missing')
  })
})

describe('calendarSubtitle', () => {
  it('prefers the structured fields and falls back to detail', () => {
    expect(calendarSubtitle(calEntry({ seasonNumber: 2, episodeNumber: 4, episodeTitle: 'Pilot', runtime: 34 })))
      .toBe('S02E04 — Pilot · 34 min')
    expect(calendarSubtitle(calEntry({ detail: 'S01E01 — Pilot' }))).toBe('S01E01 — Pilot')
    expect(calendarSubtitle(calEntry({ kind: 'book', detail: 'by Ann Leckie' }))).toBe('Book · by Ann Leckie')
  })
})

describe('groupCalendar', () => {
  it('buckets by day, oldest first, sorted within the day', () => {
    const sections = groupCalendar([
      calEntry({ date: '2026-08-04', title: 'Later' }),
      calEntry({ date: '2026-08-02', title: 'Bee' }),
      calEntry({ date: '2026-08-02', title: 'Ape' }),
    ])
    expect(sections.map((s) => s.key)).toEqual(['2026-08-02', '2026-08-04'])
    expect(sections[0]?.data.map((e) => e.title)).toEqual(['Ape', 'Bee'])
  })
})
