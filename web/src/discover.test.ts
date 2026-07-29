import { describe, expect, it } from 'vitest'
import type { DiscoverList } from './api'
import { cardKey, rowCounts, sourceLabel, visibleLists } from './discover'

const lists: DiscoverList[] = [
  { id: 'tmdb-trending-movies', title: 'Trending this week', blurb: 'a', kind: 'movie', source: 'tmdb' },
  { id: 'tmdb-trending-series', title: 'Trending this week', blurb: 'b', kind: 'series', source: 'tmdb' },
  { id: 'tmdb-top-movies', title: 'Top rated movies', blurb: 'c', kind: 'movie', source: 'tmdb' },
  { id: 'trakt-boxoffice', title: 'Box office', blurb: 'd', kind: 'movie', source: 'trakt' },
]

describe('visibleLists', () => {
  it('returns everything for "all", in the order the server sent', () => {
    expect(visibleLists(lists, 'all').map((l) => l.id)).toEqual([
      'tmdb-trending-movies',
      'tmdb-trending-series',
      'tmdb-top-movies',
      'trakt-boxoffice',
    ])
  })

  it('filters by kind without reordering', () => {
    expect(visibleLists(lists, 'movie').map((l) => l.id)).toEqual([
      'tmdb-trending-movies',
      'tmdb-top-movies',
      'trakt-boxoffice',
    ])
    expect(visibleLists(lists, 'series').map((l) => l.id)).toEqual(['tmdb-trending-series'])
  })

  it('survives an empty catalogue', () => {
    expect(visibleLists([], 'movie')).toEqual([])
  })
})

describe('rowCounts', () => {
  it('counts rows per tab, not results', () => {
    expect(rowCounts(lists)).toEqual({ all: 4, movie: 3, series: 1 })
  })

  it('is all zeroes with no providers configured', () => {
    expect(rowCounts([])).toEqual({ all: 0, movie: 0, series: 0 })
  })
})

describe('cardKey', () => {
  // The same title appears in several rows — trending and popular overlap
  // heavily — so "just added" has to be recognisable across rows, and a movie
  // must not collide with a series that happens to share a TMDB id.
  it('is stable across rows and distinguishes kinds', () => {
    expect(cardKey({ kind: 'movie', tmdbId: 601 })).toBe('movie-601')
    expect(cardKey({ kind: 'movie', tmdbId: 601 })).toBe(cardKey({ kind: 'movie', tmdbId: 601 }))
    expect(cardKey({ kind: 'series', tmdbId: 601 })).not.toBe(cardKey({ kind: 'movie', tmdbId: 601 }))
  })
})

describe('sourceLabel', () => {
  it('renders the providers we know by their real capitalisation', () => {
    expect(sourceLabel('tmdb')).toBe('TMDB')
    expect(sourceLabel('trakt')).toBe('Trakt')
  })

  it('shows an unknown provider rather than hiding it', () => {
    expect(sourceLabel('simkl')).toBe('Simkl')
    expect(sourceLabel('')).toBe('')
  })
})
