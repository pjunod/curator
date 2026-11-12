import { describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from './api'
import { choiceOf, isTerminal, originalsNote, parseChoice, planRequest, startImport } from './itemImport'
import type { MediaItemDetail, ScannedFile } from './types'

const item = { id: 4242, kind: 'series', title: 'Slow Horses', path: '/tv/Slow Horses', seasons: [], copies: [] } as unknown as MediaItemDetail
const runner: ScannedFile = { path: '/done/pack/a7f3c91e.mkv', name: 'a7f3c91e.mkv', size: 1, kind: 'video', quality: '', season: -1, episodes: [],
  publication: { downloadId: 77, publicationId: 5, path: 'a7f3c91e.mkv', outcome: 'unresolved' } }
const runner2: ScannedFile = { ...runner, path: '/done/pack/b81c22d0.mkv', name: 'b81c22d0.mkv', publication: { ...runner.publication!, path: 'b81c22d0.mkv' } }
const loose: ScannedFile = { path: '/downloads/Slow.Horses.S04E05.mkv', name: 'Slow.Horses.S04E05.mkv', size: 1, kind: 'video', quality: '', season: 4, episodes: [5] }

describe('Import file…', () => {
  it('reads and writes episode choices', () => {
    expect(parseChoice(choiceOf(4, [3, 4]))).toEqual({ season: 4, episodes: [3, 4] })
    expect(parseChoice('')).toBeNull()
    expect(planRequest(item, [runner], { [runner.path]: '4:4' }, 0, false).files).toEqual([{ path: runner.path, season: 4, episodes: [4] }])
  })

  it("places a runner delivery's files in one placement and a loose file through the manual import", async () => {
    const client = {
      placePublicationFiles: vi.fn().mockResolvedValue(undefined),
      manualImport: vi.fn().mockResolvedValue({ downloadId: 91 }),
    }
    const out = await startImport(client as unknown as MonarrClient, item, [runner, runner2, loose], { [runner.path]: '4:4', [runner2.path]: '4:3', [loose.path]: '4:5' }, 0, true)
    expect(client.placePublicationFiles).toHaveBeenCalledOnce()
    expect(client.placePublicationFiles).toHaveBeenCalledWith(77, 5, { mediaItemId: 4242, copyId: undefined, files: [
      { path: 'a7f3c91e.mkv', season: 4, episodes: [4] }, { path: 'b81c22d0.mkv', season: 4, episodes: [3] }] })
    expect(client.manualImport).toHaveBeenCalledWith({ path: '/downloads', paths: [loose.path], mediaItemId: 4242, copyId: undefined, removeOriginal: true,
      episodeTargets: [{ path: loose.path, season: 4, episodes: [5] }] })
    expect(out).toEqual([{ downloadId: 77, label: '2 files of a runner delivery' }, { downloadId: 91, label: 'Slow.Horses.S04E05.mkv' }])
  })

  it('reports a refused file without dropping the rest', async () => {
    const client = {
      placePublicationFiles: vi.fn().mockRejectedValue(new Error('not an unresolved file')),
      manualImport: vi.fn().mockResolvedValue({ downloadId: 91 }),
    }
    const out = await startImport(client as unknown as MonarrClient, item, [runner, loose], {}, 0, false)
    expect(out[0]).toEqual({ label: 'a7f3c91e.mkv', error: 'not an unresolved file' })
    expect(out[1]?.downloadId).toBe(91)
  })

  it('follows a row until its import and Remove original are done', () => {
    expect(isTerminal({ state: 'importing' })).toBe(false)
    expect(isTerminal({ state: 'imported', handoff: [{ step: 'originals_removal_pending' }] })).toBe(false)
    const done = { state: 'imported', handoff: [{ step: 'originals_removal_pending' }, { step: 'originals_removal_done', detail: 'Remove original: removed 0 original(s); original kept: /x: it changed after it was chosen' }] }
    expect(isTerminal(done)).toBe(true)
    expect(originalsNote(done)).toContain('original kept')
  })
})
