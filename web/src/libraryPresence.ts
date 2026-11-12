import type { MediaItemSummary } from './api'

/** Library filter (ADR 0028): what is on disk, what is offloaded, or both. */
export type Presence = 'all' | 'on_disk' | 'offloaded'

export const PRESENCES: { key: Presence; label: string }[] = [
  { key: 'on_disk', label: 'On disk' },
  { key: 'offloaded', label: 'Offloaded' },
  { key: 'all', label: 'Everything' },
]

/** Has anything on disk: a movie or book with a file, a show with an episode file. */
export const onDisk = (m: Pick<MediaItemSummary, 'kind' | 'fileCount' | 'episodeFileCount'>): boolean =>
  (m.kind === 'series' ? m.episodeFileCount : m.fileCount) > 0 || m.fileCount > 0

/** The default is Everything; an offloaded title is never hidden unless asked. */
export function matchesPresence(m: MediaItemSummary, p: Presence): boolean {
  switch (p) {
    case 'on_disk': return onDisk(m)
    case 'offloaded': return Boolean(m.offloaded)
    default: return true
  }
}

export const validPresence = (v: unknown): v is Presence => PRESENCES.some((p) => p.key === v)
