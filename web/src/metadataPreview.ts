import type { BookType, MetadataPreview, SearchResult } from './api'

const positiveID = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value > 0
const workID = (value: unknown): value is string => typeof value === 'string' && /^OL[0-9]+W$/.test(value)

export function previewKey(item: SearchResult): string | undefined {
  if (item.kind === 'book') return workID(item.olid) ? `book:olid:${item.olid}` : undefined
  // Provider-chain series must be previewed by their original TVDB identity.
  if (item.kind === 'series' && positiveID(item.tvdbId) && item.hydrationSource !== 'tmdb') return `series:tvdb:${item.tvdbId}`
  if (positiveID(item.tmdbId)) return `${item.kind}:tmdb:${item.tmdbId}`
  if (item.kind === 'series' && positiveID(item.tvdbId)) return `series:tvdb:${item.tvdbId}`
  return undefined
}

export function parsePreviewKey(key: unknown) {
  if (typeof key !== 'string') return undefined
  const [kind, provider, id = '', extra] = key.split(':')
  if (extra !== undefined) return undefined
  if (kind === 'book' && provider === 'olid' && workID(id)) return { kind, provider, id } as const
  if ((kind === 'movie' || kind === 'series') && (provider === 'tmdb' || (kind === 'series' && provider === 'tvdb')) && /^[1-9][0-9]*$/.test(id ?? '') && positiveID(Number(id))) return { kind, provider, id } as const
  return undefined
}

export function previewQuery(key: string): string {
  const parsed = parsePreviewKey(key)
  if (!parsed) throw new Error('This result has no supported preview identity.')
  return `kind=${parsed.kind}&${parsed.provider === 'olid' ? 'olid' : `${parsed.provider}Id`}=${encodeURIComponent(parsed.id)}`
}

export function externalLinks(item: SearchResult, preview?: MetadataPreview, identityConflict = false) {
  if (identityConflict) return []
  const ids = { tmdb: item.tmdbId, tvdb: item.tvdbId, imdb: item.imdbId, olid: item.olid, ...preview?.ids }
  const links: { label: string; url: string }[] = []
  if (typeof ids.imdb === 'string' && /^tt[0-9]{7,10}$/.test(ids.imdb)) links.push({ label: 'IMDb', url: `https://www.imdb.com/title/${ids.imdb}/` })
  if (positiveID(ids.tmdb) && item.kind !== 'book') links.push({ label: 'TMDB', url: `https://www.themoviedb.org/${item.kind === 'series' ? 'tv' : 'movie'}/${ids.tmdb}` })
  if (positiveID(ids.tvdb) && item.kind === 'series') links.push({ label: 'TVDB', url: `https://thetvdb.com/?tab=series&id=${ids.tvdb}` })
  if (workID(ids.olid) && item.kind === 'book') links.push({ label: 'Open Library', url: `https://openlibrary.org/works/${ids.olid}` })
  return links
}

export function previewState(item: SearchResult, bookType: BookType, preview?: MetadataPreview, identityConflict = false) {
  const blocked = identityConflict || preview?.addability === 'conflict' || preview?.addability === 'unsupported'
  const owned = preview && preview.ownership !== 'unknown'
    ? preview.ownership === 'present' && (item.kind !== 'book' || (preview.bookTypes ?? []).includes(bookType))
    : item.kind === 'book' ? (item.bookTypes ?? []).includes(bookType) : item.inLibrary
  return { blocked, owned, libraryItemId: !identityConflict && preview?.ownership === 'present' ? preview.libraryItemId : undefined }
}

// A verified lookup may still contradict another identity on the selected row.
export function previewIdentityConflict(item: SearchResult, preview?: MetadataPreview): boolean {
  if (!preview) return false
  const ids = preview.ids
  return preview.kind !== item.kind ||
    !!(item.tmdbId && ids.tmdb && item.tmdbId !== ids.tmdb) ||
    !!(item.tvdbId && ids.tvdb && item.tvdbId !== ids.tvdb) ||
    !!(item.imdbId && ids.imdb && item.imdbId !== ids.imdb) ||
    !!(item.olid && ids.olid && item.olid !== ids.olid)
}

export function matchesPreviewKey(item: SearchResult, key: string): boolean {
  const ref = parsePreviewKey(key)
  return !!ref && item.kind === ref.kind && (ref.provider === 'olid'
    ? item.olid === ref.id
    : ref.provider === 'tvdb' ? item.tvdbId === Number(ref.id) : item.tmdbId === Number(ref.id))
}
