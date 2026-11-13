import { formatBytes } from './format'
import type { MediaItemSummary, OffloadOperation } from './types'

/** Library filter (ADR 0028): what has file records, what is offloaded, or both. */
export type LibraryPresence = 'all' | 'on_disk' | 'offloaded'
export const PRESENCE_OPTIONS: ReadonlyArray<{ id: LibraryPresence; label: string }> = [
  { id: 'on_disk', label: 'Recorded files' },
  { id: 'offloaded', label: 'Offloaded' },
  { id: 'all', label: 'Everything' },
]

export const isPresence = (value: unknown): value is LibraryPresence => PRESENCE_OPTIONS.some((option) => option.id === value)

export function onDisk(item: Pick<MediaItemSummary, 'kind' | 'fileCount' | 'episodeFileCount'>): boolean {
  return (item.kind === 'series' ? item.episodeFileCount : item.fileCount) > 0 || item.fileCount > 0
}

/** A show with one offloaded episode and files elsewhere matches BOTH Recorded files and Offloaded (by design). */
export function matchesPresence(item: MediaItemSummary, presence: LibraryPresence): boolean {
  if (presence === 'on_disk') return onDisk(item)
  if (presence === 'offloaded') return Boolean(item.offloaded)
  return true
}

export const OFFLOAD_RUNNING = new Set(['deleting', 'waiting', 'retrying', 'stopping'])

export const freeText = (bytes: number): string => (bytes < 0 ? 'unknown' : formatBytes(bytes))

/** What a running or finished operation reads as, in plain words. */
export function offloadText(op: OffloadOperation): string {
  switch (op.state) {
    case 'complete': return `Offloaded ${op.label ?? ''}: ${op.filesDone} file${op.filesDone === 1 ? '' : 's'} deleted · ${formatBytes(op.bytesFreed)} freed.`.replace('  ', ' ')
    case 'cancelled': return `Stopped. ${op.filesDone} of ${op.filesTotal} files were deleted; the rest stay on disk.`
    case 'failed': return `The Offload failed: ${op.lastError ?? 'unknown error'}.`
    case 'waiting': return `Waiting: ${op.lastError ?? 'the library folder is not readable'}. Nothing was deleted while it waits.`
    case 'retrying': return `${op.failed?.length ?? 0} file(s) could not be deleted; retrying. ${op.lastError ?? ''}`.trim()
    case 'stopping': return `Stopping… ${op.filesDone} of ${op.filesTotal} files were deleted; the rest stay on disk.`
    default: return `Deleting… ${op.filesDone} of ${op.filesTotal} files.`
  }
}

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
