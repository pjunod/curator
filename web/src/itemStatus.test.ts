import { describe, expect, it } from 'vitest'
import { REMATCH_STATE, noticeBelongsTo, doneAbout, fmtBytes, fmtDuration, fmtRate, rematchCounts, stageActions, statusPollMs, updatedAgo } from './itemStatus'
import type { ItemStatus } from './api'

describe('item status display', () => {
  it('polls fast only while a download is moving', () => {
    expect(statusPollMs({ active: true } as ItemStatus)).toBe(2_000)
    expect(statusPollMs({ active: false } as ItemStatus)).toBe(30_000)
    expect(statusPollMs(undefined)).toBe(30_000)
  })

  it('formats runner numbers without recomputing them', () => {
    expect(fmtBytes(18.4e9)).toBe('18.4 GB')
    expect(fmtRate(48.2e6)).toBe('48.2 MB/s')
    expect(fmtDuration(66)).toBe('1 min 06 s')
    expect(fmtDuration(9)).toBe('9 s')
    expect(fmtDuration(3 * 3600 + 120)).toBe('3 h 02 min')
    const now = new Date(2026, 10, 11, 18, 20, 0)
    expect(doneAbout({ id: 1, release: 'r', state: 'downloading', bytes: 0, total: 0, progress: 0, etaSeconds: 660 }, now)).toBe('18:31')
    expect(doneAbout({ id: 1, release: 'r', state: 'downloading', bytes: 0, total: 0, progress: 0 }, now)).toBe('')
  })

  it('lists each offered action once, in stage order', () => {
    expect(stageActions([
      { key: 'searched', state: 'waiting', actions: ['search', 'interactive_search'] },
      { key: 'downloading', state: 'todo' },
      { key: 'in_cinema', state: 'todo', actions: ['confirm_cinema', 'search'] },
    ])).toEqual(['search', 'interactive_search', 'confirm_cinema'])
  })

  it('says how fresh the panel is', () => {
    expect(updatedAgo(10_000, 12_000)).toBe('updated 2 s ago')
    expect(updatedAgo(0, 180_000)).toBe('updated 3 min ago')
  })

  it('words an open Re-match: counts, bytes, state', () => {
    expect(rematchCounts({ filesDone: 1, filesTotal: 2, bytesDone: 5e6, bytesTotal: 10e6 })).toEqual(['1 of 2 files', '5 MB of 10 MB'])
    expect(rematchCounts({ filesDone: 0, filesTotal: 1, bytesDone: 0, bytesTotal: 0 })).toEqual(['0 of 1 file'])
    expect(REMATCH_STATE.cancel_pending).toContain('putting the moved files back')
    expect(statusPollMs({ active: true, rematch: { state: 'moving' } } as ItemStatus)).toBe(2_000)
  })

  it('keeps a Stop or Dismiss answer only for the operation it was given for', () => {
    expect(noticeBelongsTo('op1', undefined)).toBe(true) // the block is gone: the answer stays
    expect(noticeBelongsTo('op1', 'op1')).toBe(true)
    expect(noticeBelongsTo('op1', 'op2')).toBe(false) // a new operation opened
    expect(REMATCH_STATE.failed).toBe('Failed')
  })
})
