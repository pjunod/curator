import { describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from './api'
import { choiceOf, parseChoice, planRequest, startImport } from './itemImport'
import type { MediaItemDetail, ScannedFile } from './types'

const item = { id: 4242, kind: 'series', title: 'Slow Horses', path: '/tv/Slow Horses', seasons: [], copies: [] } as unknown as MediaItemDetail
const runner: ScannedFile = { path: '/done/pack/a7f3c91e.mkv', name: 'a7f3c91e.mkv', size: 1, kind: 'video', quality: '', season: -1, episodes: [],
  publication: { downloadId: 77, publicationId: 5, path: 'a7f3c91e.mkv', outcome: 'unresolved' } }
const loose: ScannedFile = { path: '/downloads/Slow.Horses.S04E05.mkv', name: 'Slow.Horses.S04E05.mkv', size: 1, kind: 'video', quality: '', season: 4, episodes: [5] }

describe('Import file…', () => {
  it('reads and writes episode choices', () => {
    expect(parseChoice(choiceOf(4, [3, 4]))).toEqual({ season: 4, episodes: [3, 4] })
    expect(parseChoice('')).toBeNull()
    expect(planRequest(item, [runner], { [runner.path]: '4:4' }, 0, false).files).toEqual([{ path: runner.path, season: 4, episodes: [4] }])
  })

  it('places a runner file through Resolve and a loose file through the manual import', async () => {
    const client = {
      placePublicationFile: vi.fn().mockResolvedValue(undefined),
      manualImport: vi.fn().mockResolvedValue({ downloadId: 91 }),
    }
    const out = await startImport(client as unknown as MonarrClient, item, [runner, loose], { [runner.path]: '4:4', [loose.path]: '4:5' }, 0, true)
    expect(client.placePublicationFile).toHaveBeenCalledWith(77, 5, 'a7f3c91e.mkv', { mediaItemId: 4242, copyId: undefined, season: 4, episodes: [4] })
    expect(client.manualImport).toHaveBeenCalledWith({ path: '/downloads', paths: [loose.path], mediaItemId: 4242, copyId: undefined, removeOriginal: true,
      episodeTargets: [{ path: loose.path, season: 4, episodes: [5] }] })
    expect(out).toEqual([{ downloadId: 77, label: 'a7f3c91e.mkv' }, { downloadId: 91, label: 'Slow.Horses.S04E05.mkv' }])
  })

  it('reports a refused file without dropping the rest', async () => {
    const client = {
      placePublicationFile: vi.fn().mockRejectedValue(new Error('not an unresolved file')),
      manualImport: vi.fn().mockResolvedValue({ downloadId: 91 }),
    }
    const out = await startImport(client as unknown as MonarrClient, item, [runner, loose], {}, 0, false)
    expect(out[0]).toEqual({ label: 'a7f3c91e.mkv', error: 'not an unresolved file' })
    expect(out[1]?.downloadId).toBe(91)
  })
})
