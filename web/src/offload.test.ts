import { describe, expect, it } from 'vitest'
import type { MediaItemSummary } from './api'
import { matchesPresence, onDisk, seasonOffload, validPresence } from './libraryPresence'
import { offloadText, freeText } from './OffloadDialog'

const item = (over: Partial<MediaItemSummary>) => ({ kind: 'movie', fileCount: 0, episodeFileCount: 0, ...over }) as MediaItemSummary

describe('library presence filter', () => {
  it('defaults to showing everything, offloaded titles included', () => {
    expect(matchesPresence(item({ offloaded: true }), 'all')).toBe(true)
    expect(matchesPresence(item({}), 'all')).toBe(true)
  })
  it('On disk keeps only what has files; Offloaded only what was offloaded', () => {
    const have = item({ fileCount: 1 })
    const gone = item({ offloaded: true })
    const show = item({ kind: 'series', episodeFileCount: 3, offloaded: true, offloadedEpisodes: 2 })
    expect(matchesPresence(have, 'on_disk')).toBe(true)
    expect(matchesPresence(gone, 'on_disk')).toBe(false)
    expect(matchesPresence(show, 'on_disk')).toBe(true) // a partly offloaded show still has episodes on disk
    expect(matchesPresence(have, 'offloaded')).toBe(false)
    expect(matchesPresence(gone, 'offloaded')).toBe(true)
    expect(matchesPresence(show, 'offloaded')).toBe(true)
    expect(onDisk(gone)).toBe(false)
  })
  it('only accepts the three remembered values', () => {
    expect(['all', 'on_disk', 'offloaded'].every(validPresence)).toBe(true)
    expect(validPresence('missing')).toBe(false)
  })
})

describe('offload dialog text', () => {
  const base = { operationId: 'of-1', requestId: 'r', itemId: 1, scope: 'item' as const, filesTotal: 2, filesDone: 2, bytesTotal: 10, bytesFreed: 10, stoppable: false }
  it('says what happened in plain words', () => {
    expect(offloadText({ ...base, state: 'complete', label: 'Dune' })).toContain('2 files deleted')
    expect(offloadText({ ...base, state: 'waiting', filesDone: 0, lastError: 'the share is unmounted' })).toContain('Nothing was deleted while it waits')
    expect(offloadText({ ...base, state: 'retrying', failed: ['a: permission denied'] })).toContain('retrying')
    expect(offloadText({ ...base, state: 'cancelled', filesDone: 1 })).toContain('the rest stay on disk')
  })
  it('free space unknown is said, not shown as 0', () => {
    expect(freeText(-1)).toBe('unknown')
    expect(freeText(2 * 1024 ** 3)).toMatch(/2(\.0)? GB/)
  })
})

describe('season offload state (the season flag is only inheritance)', () => {
  it('counts episodes by their own flag, so a re-acquired episode of a flagged season is not offloaded', () => {
    const s = { offloaded: true, episodes: [{ offloaded: false }, { offloaded: true }] }
    expect(seasonOffload(s)).toEqual({ episodes: 1, fully: false })
    expect(seasonOffload({ offloaded: true, episodes: [{ offloaded: true }, { offloaded: true }] })).toEqual({ episodes: 2, fully: true })
  })
  it('an episode-less flagged season is fully offloaded; an unflagged one is not', () => {
    expect(seasonOffload({ offloaded: true, episodes: [] }).fully).toBe(true)
    expect(seasonOffload({ episodes: [] }).fully).toBe(false)
  })
})

describe('stopping', () => {
  it('reads as stopping and counts as running', () => {
    expect(offloadText({ state: 'stopping', filesDone: 1, filesTotal: 3, bytesFreed: 0 } as never)).toMatch(/^Stopping/)
  })
})
