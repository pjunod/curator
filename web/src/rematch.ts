import type { MediaItemSummary, RematchTarget, SearchResult } from './api'

/** S04E05, or S01E01-E02 for a file covering two episodes. */
export const episodeToken = (season: number, episodes: number[]) =>
  episodes.length === 0
    ? ''
    : `S${String(season).padStart(2, '0')}E${String(episodes[0]).padStart(2, '0')}` +
      (episodes.length > 1 ? `-E${String(episodes[episodes.length - 1]).padStart(2, '0')}` : '')

export interface RematchCandidate { key: string; title: string; year?: number; detail: string; target: RematchTarget; inLibrary: boolean; current: boolean }

/**
 * The Re-match dialog's titles: library items whose title matches first (a
 * manual entry has no provider id, so it is only reachable this way), then
 * provider results not already in the library, which a Re-match adds.
 */
export function rematchCandidates(library: MediaItemSummary[], search: SearchResult[], query: string, currentId: number): RematchCandidate[] {
  const q = query.trim().toLowerCase()
  const out: RematchCandidate[] = []
  if (!q) return out
  for (const m of library) {
    if (!m.title.toLowerCase().includes(q)) continue
    out.push({ key: `lib-${m.id}`, title: m.title, year: m.year || undefined, detail: 'in your library', target: { itemId: m.id }, inLibrary: true, current: m.id === currentId })
  }
  for (const r of search) {
    if (r.inLibrary) continue // listed above, by its library id
    const ids = r.tmdbId ? `TMDB ${r.tmdbId}` : r.tvdbId ? `TVDB ${r.tvdbId}` : r.olid ?? ''
    out.push({
      key: `meta-${r.tmdbId}-${r.tvdbId ?? ''}-${r.olid ?? ''}`, title: r.title, year: r.year || undefined, detail: `${ids} · added to the library`, inLibrary: false, current: false,
      target: { kind: r.kind, tmdbId: r.tmdbId || undefined, tvdbId: r.tvdbId || undefined, imdbId: r.imdbId || undefined, olid: r.olid || undefined, hydrationSource: r.hydrationSource || undefined },
    })
  }
  return out
}
