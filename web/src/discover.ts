import type { DiscoverList, MediaKind, SearchResult } from './api'

/** Which kinds the Discover page can filter by. Books are absent on purpose:
 *  Open Library has no popularity surface worth a row (ADR 0015). */
export type DiscoverFilter = 'all' | Extract<MediaKind, 'movie' | 'series'>

export const DISCOVER_FILTERS: { key: DiscoverFilter; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'movie', label: 'Movies' },
  { key: 'series', label: 'Shows' },
]

/** visibleLists applies the kind filter to the catalogue, preserving the
 *  server's ordering — the providers decide what leads the page. */
export function visibleLists(lists: DiscoverList[], filter: DiscoverFilter): DiscoverList[] {
  if (filter === 'all') return lists
  return lists.filter((l) => l.kind === filter)
}

/** cardKey identifies a discover result. Rows have no server-side id and the
 *  same title legitimately appears in several rows (trending and popular
 *  overlap heavily), so the kind+id pair is the identity — and it is also
 *  what "this one was just added" is keyed on across every row on the page. */
export function cardKey(r: Pick<SearchResult, 'kind' | 'tmdbId'>): string {
  return `${r.kind}-${r.tmdbId}`
}

/** rowCounts summarizes the catalogue for the filter tabs. A tab that would
 *  show nothing is worth knowing about before it is clicked. */
export function rowCounts(lists: DiscoverList[]): Record<DiscoverFilter, number> {
  return {
    all: lists.length,
    movie: lists.filter((l) => l.kind === 'movie').length,
    series: lists.filter((l) => l.kind === 'series').length,
  }
}

/** sourceLabel renders a provider id for display. Unknown providers fall
 *  through capitalized rather than being hidden — a row from somewhere the UI
 *  has not heard of should still say where it came from. */
export function sourceLabel(source: string): string {
  if (source === 'tmdb') return 'TMDB'
  if (source === 'trakt') return 'Trakt'
  return source.charAt(0).toUpperCase() + source.slice(1)
}
