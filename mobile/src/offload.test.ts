import { describe, expect, it } from 'vitest'
import { freeText, isPresence, matchesPresence, offloadText, onDisk, seasonOffload } from './offload'
import type { MediaItemSummary, OffloadOperation } from './types'

const item = (over: Partial<MediaItemSummary>) => ({ kind: 'movie', fileCount: 0, episodeFileCount: 0, ...over }) as MediaItemSummary

describe('library presence filter', () => {
  it('Everything shows offloaded titles too', () => {
    expect(matchesPresence(item({ offloaded: true }), 'all')).toBe(true)
  })
  it('On disk keeps what has files; Offloaded what was put away', () => {
    const have = item({ fileCount: 1 })
    const gone = item({ offloaded: true })
    const partly = item({ kind: 'series', episodeFileCount: 2, offloaded: true, offloadedEpisodes: 1 })
    expect(matchesPresence(have, 'on_disk')).toBe(true)
    expect(matchesPresence(gone, 'on_disk')).toBe(false)
    expect(matchesPresence(partly, 'on_disk')).toBe(true)
    expect(matchesPresence(have, 'offloaded')).toBe(false)
    expect(matchesPresence(gone, 'offloaded')).toBe(true)
    expect(onDisk(gone)).toBe(false)
  })
  it('accepts only the three remembered values', () => {
    expect(['all', 'on_disk', 'offloaded'].every(isPresence)).toBe(true)
    expect(isPresence('missing')).toBe(false)
  })
})

describe('offload progress text', () => {
  const base = { operationId: 'of-1', requestId: 'r', itemId: 1, scope: 'item', filesTotal: 2, filesDone: 2, bytesTotal: 10, bytesFreed: 10, stoppable: false } as const
  it('says what happened in plain words', () => {
    expect(offloadText({ ...base, state: 'complete', label: 'Dune' } as OffloadOperation)).toContain('2 files deleted')
    expect(offloadText({ ...base, state: 'waiting', filesDone: 0, lastError: 'the share is unmounted' } as OffloadOperation)).toContain('Nothing was deleted while it waits')
    expect(offloadText({ ...base, state: 'retrying', failed: ['a'] } as OffloadOperation)).toContain('retrying')
    expect(offloadText({ ...base, state: 'cancelled', filesDone: 1 } as OffloadOperation)).toContain('the rest stay on disk')
  })
  it('free space unknown is said, not shown as zero', () => {
    expect(freeText(-1)).toBe('unknown')
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
