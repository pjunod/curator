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
})
