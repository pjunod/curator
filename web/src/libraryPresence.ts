import type { MediaItemSummary } from './api'

/** Library filter (ADR 0028): what has file records, what is offloaded, or both. */
export type Presence = 'all' | 'on_disk' | 'offloaded'

export const PRESENCES: { key: Presence; label: string }[] = [
  { key: 'on_disk', label: 'Recorded files' },
  { key: 'offloaded', label: 'Offloaded' },
  { key: 'all', label: 'Everything' },
]

/** Has anything on disk: a movie or book with a file, a show with an episode file. */
export const onDisk = (m: Pick<MediaItemSummary, 'kind' | 'fileCount' | 'episodeFileCount'>): boolean =>
  (m.kind === 'series' ? m.episodeFileCount : m.fileCount) > 0 || m.fileCount > 0

/** The default is Everything; an offloaded title is never hidden unless asked.
 *  A show with one offloaded episode and files elsewhere matches BOTH Recorded files and Offloaded (by design). */
export function matchesPresence(m: MediaItemSummary, p: Presence): boolean {
  switch (p) {
    case 'on_disk': return onDisk(m)
    case 'offloaded': return Boolean(m.offloaded)
    default: return true
  }
}

export const validPresence = (v: unknown): v is Presence => PRESENCES.some((p) => p.key === v)

/**
 * What is offloaded inside a season. The season's own flag only means "new
 * episodes inherit it"; each episode's flag says what is offloaded, so a
 * re-acquired episode of a flagged season is wanted again. `fully`: nothing in
 * the season is wanted (every episode, or an episode-less flagged season).
 */
export function seasonOffload(s: { offloaded?: boolean; episodes: { offloaded?: boolean }[] }): { episodes: number; fully: boolean } {
  const episodes = s.episodes.filter((e) => e.offloaded).length
  return { episodes, fully: s.episodes.length > 0 ? episodes === s.episodes.length : Boolean(s.offloaded) }
}
