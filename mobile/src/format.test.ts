import { describe, expect, it } from 'vitest'
import { completeness, formatBytes, formatRating, formatUptime, localDateKey, posterUrl } from './format'
import type { MediaItemSummary } from './types'

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
