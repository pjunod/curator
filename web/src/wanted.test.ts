import { describe, expect, it } from 'vitest'
import type { WantedItem } from './api'
import { selectWanted, wantedKind, wantedReason } from './wanted'

function item(over: Partial<WantedItem> = {}): WantedItem {
  return {
    wantableId: 'movie:1',
    mediaItemId: 1,
    title: 'Arrival',
    detail: '2016',
    missing: true,
    current: '',
    copy: '',
    ...over,
  }
}

const rows = [
  item(),
  item({
    wantableId: 'season:2:1',
    mediaItemId: 2,
    title: 'Severance',
    detail: 'Season 1',
    missing: false,
    current: 'HDTV-720p',
  }),
  item({
    wantableId: 'book:3:c9',
    mediaItemId: 3,
    title: 'Ancillary Justice',
    detail: 'by Ann Leckie',
    copy: 'Audiobook',
  }),
]

describe('wanted list controls', () => {
  it('filters missing and upgrade reasons independently', () => {
    expect(selectWanted(rows, 'missing', '', 'title', 'asc').map((row) => row.title)).toEqual([
      'Ancillary Justice',
      'Arrival',
    ])
    expect(selectWanted(rows, 'upgrade', '', 'title', 'asc').map((row) => row.title)).toEqual([
      'Severance',
    ])
  })

  it('searches reason, current quality, media type, detail, and copy label', () => {
    expect(selectWanted(rows, 'all', 'upgrade', 'title', 'asc').map((row) => row.title)).toEqual(['Severance'])
    expect(selectWanted(rows, 'all', '720p', 'title', 'asc').map((row) => row.title)).toEqual(['Severance'])
    expect(selectWanted(rows, 'all', 'series', 'title', 'asc').map((row) => row.title)).toEqual(['Severance'])
    expect(selectWanted(rows, 'all', 'ann leckie', 'title', 'asc').map((row) => row.title)).toEqual(['Ancillary Justice'])
    expect(selectWanted(rows, 'all', 'audiobook', 'title', 'asc').map((row) => row.title)).toEqual(['Ancillary Justice'])
  })

  it('sorts by reason and normalizes episode/season targets as series', () => {
    expect(wantedReason(rows[1])).toBe('upgrade from HDTV-720p')
    expect(wantedKind(rows[1])).toBe('series')
    expect(selectWanted(rows, 'all', '', 'reason', 'desc')[0].title).toBe('Severance')
  })

  it('does not mutate the API response while sorting', () => {
    const original = [...rows]
    selectWanted(rows, 'all', '', 'title', 'desc')
    expect(rows).toEqual(original)
  })
})
