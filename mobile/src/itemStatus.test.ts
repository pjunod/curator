import { describe, expect, it } from 'vitest'
import { downloadLine, fmtDuration, stageActions, statusPollMs } from './itemStatus'

describe('item status helpers', () => {
  it('polls fast only while a download moves', () => {
    expect(statusPollMs({ active: true } as never)).toBe(2_000)
    expect(statusPollMs(null)).toBe(30_000)
  })
  it('relays runner numbers', () => {
    expect(fmtDuration(228)).toBe('3 min 48 s')
    expect(downloadLine({ id: 1, release: 'r', state: 'downloading', bytes: 6.3e9, total: 18.4e9, progress: 0.34, rateBps: 48.2e6, etaSeconds: 228 }))
      .toBe('6.3 of 18.4 GB · 48.2 MB/s · 3 min 48 s left')
  })
  it('offers each action once', () => {
    expect(stageActions([{ key: 'searched', state: 'waiting', actions: ['search'] }, { key: 'in_cinema', state: 'todo', actions: ['confirm_cinema', 'search'] }]))
      .toEqual(['search', 'confirm_cinema'])
  })

  it('words an open Re-match counts', async () => {
    const { rematchCounts } = await import('./itemStatus')
    expect(rematchCounts({ filesDone: 1, filesTotal: 2, bytesDone: 0, bytesTotal: 0 })).toBe('1 of 2 files')
    expect(rematchCounts({ filesDone: 0, filesTotal: 1, bytesDone: 5e6, bytesTotal: 10e6 })).toMatch(/^0 of 1 file · .* of /)
  })
})
