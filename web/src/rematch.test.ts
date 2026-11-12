import { describe, expect, it } from 'vitest'
import type { MediaItemSummary, SearchResult } from './api'
import { episodeToken, rematchCandidates } from './rematch'

const lib = (id: number, title: string, year: number) => ({ id, kind: 'movie', title, year } as MediaItemSummary)
const meta = (tmdbId: number, title: string, year: number, inLibrary = false) =>
  ({ kind: 'movie', tmdbId, title, year, overview: '', posterPath: '', inLibrary } as SearchResult)

describe('rematchCandidates', () => {
  it('lists library titles first, then provider titles not in the library', () => {
    const got = rematchCandidates([lib(1, 'Dune', 1984), lib(2, 'Arrival', 2016)], [meta(841, 'Dune', 1984, true), meta(438631, 'Dune', 2021)], 'dune', 1)
    expect(got.map((c) => c.key)).toEqual(['lib-1', 'meta-438631--'])
    expect(got[0]).toMatchObject({ current: true, inLibrary: true, target: { itemId: 1 } })
    expect(got[1]).toMatchObject({ inLibrary: false, target: { kind: 'movie', tmdbId: 438631 } })
  })
  it('lists nothing without a query', () => {
    expect(rematchCandidates([lib(1, 'Dune', 1984)], [], '  ', 1)).toEqual([])
  })
})

describe('episodeToken', () => {
  it('names one or a range of episodes', () => {
    expect(episodeToken(4, [5])).toBe('S04E05')
    expect(episodeToken(1, [1, 2])).toBe('S01E01-E02')
    expect(episodeToken(1, [])).toBe('')
  })
})

describe('cinemaFeatureState', () => {
  it('reads removed_paths support and when it was read', async () => {
    const { cinemaFeatureState } = await import('./pages/CinemaFeatures')
    const base = { id: 1, type: 'plurx', name: 'cinema' } as never
    expect(cinemaFeatureState(base).supported).toBeNull()
    expect(cinemaFeatureState({ ...(base as object), features: ['removed_paths'], featuresReadAt: 1 } as never).supported).toBe(true)
    expect(cinemaFeatureState({ ...(base as object), features: [], featuresReadAt: 1 } as never).supported).toBe(false)
    // Offload is advertised separately from removed_paths (ADR 0028).
    const withOffload = { ...(base as object), features: ['removed_paths', 'offload'], featuresReadAt: 1 } as never
    expect(cinemaFeatureState(withOffload, 'offload').supported).toBe(true)
    expect(cinemaFeatureState({ ...(base as object), features: ['removed_paths'], featuresReadAt: 1 } as never, 'offload').supported).toBe(false)
  })
})
