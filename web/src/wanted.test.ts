import { describe, expect, it } from 'vitest'
import type { WantedItem } from './api'
import { selectWanted, selectWantedGroups, wantedKind, wantedReason } from './wanted'

function item(over: Partial<WantedItem> = {}): WantedItem {
  return {
    wantableId: 'movie:1',
    mediaItemId: 1,
    title: 'Arrival',
    detail: '2016',
    missing: true,
    current: '',
    copy: '',
    copyId: 0,
    reason: 'missing',
    kind: 'movie',
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
    reason: 'upgrade',
    kind: 'series',
  }),
  item({
    wantableId: 'book:3:c9',
    mediaItemId: 3,
    title: 'Ancillary Justice',
    detail: 'by Ann Leckie',
    copy: 'Audiobook',
    copyId: 9,
    kind: 'book',
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

  it('groups by item id, keeps duplicate titles separate, and orders episode numbers numerically', () => {
    const episodes = [
      item({ wantableId: 'episode:9:1:10', mediaItemId: 9, title: 'Same', kind: 'series', season: 1, episode: 10, detail: 'S01E10' }),
      item({ wantableId: 'episode:10:1:1', mediaItemId: 10, title: 'Same', kind: 'series', season: 1, episode: 1, detail: 'S01E01' }),
      item({ wantableId: 'episode:9:1:2', mediaItemId: 9, title: 'Same', kind: 'series', season: 1, episode: 2, detail: 'S01E02' }),
    ]
    const groups = selectWantedGroups(episodes, 'all', '', 'title', 'asc')
    expect(groups.map((group) => group.mediaItemId)).toEqual([9, 10])
    expect(groups[0].children.map((child) => child.episode)).toEqual([2, 10])
  })

  it('retains all reason-visible siblings when text matches one child', () => {
    const episodes = [
      item({ wantableId: 'episode:9:1:1', mediaItemId: 9, title: 'Show', kind: 'series', season: 1, episode: 1, detail: 'Pilot' }),
      item({ wantableId: 'episode:9:1:2:c4', mediaItemId: 9, title: 'Show', kind: 'series', season: 1, episode: 2, detail: 'Finale', copy: '4K', copyId: 4 }),
    ]
    const groups = selectWantedGroups(episodes, 'missing', '4k', 'title', 'asc')
    expect(groups).toHaveLength(1)
    expect(groups[0].children).toHaveLength(2)
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
