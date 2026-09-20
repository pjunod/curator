import { describe, expect, it } from 'vitest'
import type { MetadataPreview, SearchResult } from './api'
import { externalLinks, parsePreviewKey, previewIdentityConflict, previewKey, previewQuery, previewState } from './metadataPreview'

const series: SearchResult = { kind: 'series', tmdbId: 0, tvdbId: 414217, imdbId: 'tt16867040', hydrationSource: 'tvmaze', title: 'Cunk on Earth', year: 2022, overview: 'Original synopsis', posterPath: '', inLibrary: false }
const details: MetadataPreview = { kind: 'series', title: series.title, overview: 'Full synopsis', ids: { tmdb: 999, tvdb: 414217, imdb: series.imdbId }, ownership: 'present', libraryItemId: 42, addability: 'supported' }

describe('metadata preview identity and actions', () => {
  it('uses the original provider key and leaves the add identity untouched', () => {
    const original = Object.freeze({ ...series })
    expect(previewKey(original)).toBe('series:tvdb:414217')
    expect(previewQuery(previewKey(original)!)).toBe('kind=series&tvdbId=414217')
    externalLinks(original, details)
    expect(previewState(original, 'ebook', details)).toEqual({ blocked: false, owned: true, libraryItemId: 42 })
    expect(original).toEqual(series)
    expect(original.tmdbId).toBe(0)
  })
  it('only builds links from validated database identities', () => {
    expect(externalLinks(series).map((link) => link.label)).toEqual(['IMDb', 'TVDB'])
    expect(externalLinks({ ...series, tvdbId: -1, imdbId: 'javascript:alert(1)' })).toEqual([])
    expect(externalLinks(series, details, true)).toEqual([])
    expect(parsePreviewKey('series:tvdb:1:extra')).toBeUndefined()
    expect(parsePreviewKey('movie:tvdb:1')).toBeUndefined()
    expect(parsePreviewKey('book:olid:OL12M')).toBeUndefined()
    expect(parsePreviewKey('movie:tmdb:9007199254740992')).toBeUndefined()
  })
  it('keeps ambiguity facts but blocks Add and Open', () => {
    const state = previewState(series, 'ebook', { ...details, ownership: 'ambiguous', addability: 'conflict', libraryItemId: undefined })
    expect(state).toEqual({ blocked: true, owned: false, libraryItemId: undefined })
    expect(externalLinks(series, details)).toHaveLength(3)
    expect(previewState(series, 'ebook', undefined, true).blocked).toBe(true)
    expect(previewIdentityConflict(series, { ...details, ids: { tvdb: 12 } })).toBe(true)
  })
  it('refreshes ownership without treating one book edition as both', () => {
    const book: SearchResult = { ...series, kind: 'book', tmdbId: 0, tvdbId: undefined, imdbId: undefined, olid: 'OL12W', inLibrary: true, bookTypes: ['ebook'] }
    const preview: MetadataPreview = { ...details, kind: 'book', ids: { olid: 'OL12W' }, bookTypes: ['ebook'] }
    expect(previewState(book, 'audiobook', preview)).toEqual({ blocked: false, owned: false, libraryItemId: 42 })
    expect(previewState(book, 'ebook', preview).owned).toBe(true)
    expect(previewState(book, 'ebook', { ...preview, ownership: 'absent' }).owned).toBe(false)
    expect(previewState(book, 'ebook', { ...preview, ownership: 'unknown' }).owned).toBe(true)
    expect(externalLinks(book)).toEqual([{ label: 'Open Library', url: 'https://openlibrary.org/works/OL12W' }])
  })
})
