import type { ReleaseCandidate, ReleaseSearchScope } from './types'

// Pure helpers behind the native interactive search screen. They mirror the
// web panel's rules so the phone shows the same list the browser would:
// nothing is hidden, rejected releases stay visible with their reasons, and
// the "would be grabbed" filter is a view, not a gate.

export type ReleaseFilter = 'all' | 'accepted'

const pad = (value: number) => String(value).padStart(2, '0')

/** What the search is for, in the words the web UI uses: item, pack, or episode. */
export function scopeLabel(scope: ReleaseSearchScope, fallback = 'item'): string {
  if (scope.season !== undefined && scope.episode !== undefined) return `S${pad(scope.season)}E${pad(scope.episode)}`
  if (scope.season !== undefined) return `Season ${scope.season} pack`
  return fallback
}

export function filterCandidates(all: ReleaseCandidate[], filter: ReleaseFilter, query: string): ReleaseCandidate[] {
  const needle = query.trim().toLowerCase()
  return all.filter((candidate) => {
    if (filter === 'accepted' && !candidate.accepted) return false
    if (needle && !candidate.title.toLowerCase().includes(needle)) return false
    return true
  })
}

export function acceptedCount(all: ReleaseCandidate[]): number {
  return all.filter((candidate) => candidate.accepted).length
}

/**
 * Two indexers can return the same title, and one indexer can return it
 * twice with different download links. The token is unique per candidate when
 * the server issued one; the link is the next-best identity.
 */
export function candidateKey(candidate: ReleaseCandidate): string {
  return candidate.candidateToken || `${candidate.indexer}\u0000${candidate.downloadUrl}\u0000${candidate.title}`
}

/** The message shown after a grab, matching the web panel's wording. */
export function grabbedMessage(title: string): string {
  return `Grabbed ${title} — follow it under Activity.`
}

/**
 * The season a series-level interactive search opens on: the first one that
 * is not specials. A series search needs a season — the server has no
 * whole-series scope — so callers that could pass none check for seasons.
 */
export function firstSeason(seasons: { number: number }[]): number | undefined {
  return (seasons.find((season) => season.number > 0) ?? seasons[0])?.number
}
