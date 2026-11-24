import type { MonarrClient } from './api'
import type { ImportPlanMode, ImportPlanRequest, MediaItemDetail, ScannedFile } from './types'

/** A series file's episode choice, as "season:e1,e2" ('' = none). */
export type EpisodeChoice = string

export const episodeLabel = (season: number, episodes: number[]) =>
  episodes.length === 0
    ? ''
    : `S${String(season).padStart(2, '0')}E${String(episodes[0]).padStart(2, '0')}` +
      (episodes.length > 1 ? `-E${String(episodes[episodes.length - 1]).padStart(2, '0')}` : '')

export const choiceOf = (season: number, episodes: number[]): EpisodeChoice =>
  episodes.length ? `${season}:${episodes.join(',')}` : ''

export function parseChoice(choice: EpisodeChoice): { season: number; episodes: number[] } | null {
  if (!choice) return null
  const [season, eps = ''] = choice.split(':')
  return { season: Number(season), episodes: eps.split(',').filter(Boolean).map(Number) }
}

export const MODE_TEXT: Record<ImportPlanMode, string> = {
  runner: 'Placed from the runner delivery; Curator cleans up the original under its usual rules.',
  hardlink: 'Hardlinked — your copy stays.',
  copy: 'Copied across volumes — your copy stays.',
  rename: 'Renamed into the library.',
  copy_remove: 'Copied, verified, then the original is removed.',
}

export function planRequest(item: MediaItemDetail, files: ScannedFile[], choices: Record<string, EpisodeChoice>, copyId: number, removeOriginal: boolean): ImportPlanRequest {
  return {
    mediaItemId: item.id,
    copyId: copyId || undefined,
    removeOriginal,
    files: files.map((f) => {
      const c = parseChoice(choices[f.path] ?? '')
      return { path: f.path, season: c?.season ?? -1, episodes: c?.episodes ?? [] }
    }),
  }
}

export interface StartedImport { downloadId?: number; label: string; error?: string }

/** What a row's trace says Remove original did, or is doing. */
export function originalsNote(row: { handoff?: { step: string; detail?: string }[] }): string {
  const trace = row.handoff ?? []
  for (let i = trace.length - 1; i >= 0; i--) {
    if (trace[i]!.step === 'originals_removal_done') return trace[i]!.detail ?? ''
    if (trace[i]!.step === 'originals_removal_pending') return 'Removing the originals…'
  }
  return ''
}

export const isTerminal = (row: { state: string; handoff?: { step: string; detail?: string }[] }) =>
  (row.state === 'imported' || row.state === 'failed') && !originalsNote(row).endsWith('…')

/**
 * Start the confirmed import: a runner delivery's file through Resolve's
 * placement, loose files through one queued manual import per folder.
 */
export async function startImport(client: MonarrClient, item: MediaItemDetail, files: ScannedFile[], choices: Record<string, EpisodeChoice>, copyId: number, removeOriginal: boolean): Promise<StartedImport[]> {
  const out: StartedImport[] = []
  const byFolder = new Map<string, ScannedFile[]>()
  // A runner delivery's files go together: one placement, one import.
  const byPublication = new Map<number, ScannedFile[]>()
  for (const f of files) {
    if (f.publication) {
      byPublication.set(f.publication.publicationId, [...(byPublication.get(f.publication.publicationId) ?? []), f])
      continue
    }
    const dir = f.path.slice(0, f.path.lastIndexOf('/')) || '/'
    byFolder.set(dir, [...(byFolder.get(dir) ?? []), f])
  }
  for (const [publicationId, group] of byPublication) {
    const label = group.length === 1 ? group[0]!.name : `${group.length} files of a runner delivery`
    const downloadId = group[0]!.publication!.downloadId
    try {
      await client.placePublicationFiles(downloadId, publicationId, {
        mediaItemId: item.id,
        copyId: copyId || undefined,
        files: group.map((f) => {
          const c = parseChoice(choices[f.path] ?? '')
          return { path: f.publication!.path, season: c?.season, episodes: c?.episodes }
        }),
      })
      out.push({ downloadId, label })
    } catch (cause) {
      out.push({ label, error: cause instanceof Error ? cause.message : 'Could not place these files.' })
    }
  }
  for (const [dir, group] of byFolder) {
    const label = group.length === 1 ? group[0]!.name : `${group.length} files from ${dir}`
    try {
      const res = await client.manualImport({
        path: dir,
        paths: group.map((f) => f.path),
        mediaItemId: item.id,
        copyId: copyId || undefined,
        removeOriginal,
        episodeTargets: item.kind === 'series'
          ? group.flatMap((f) => {
              const c = parseChoice(choices[f.path] ?? '')
              return c ? [{ path: f.path, season: c.season, episodes: c.episodes }] : []
            })
          : undefined,
      })
      out.push({ downloadId: res.downloadId, label })
    } catch (cause) {
      out.push({ label, error: cause instanceof Error ? cause.message : 'Could not start this import.' })
    }
  }
  return out
}
