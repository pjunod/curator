import { describe, expect, it } from 'vitest'
import { acceptedCount, candidateKey, filterCandidates, firstSeason, grabbedMessage, scopeLabel } from './releaseSearch'
import type { ReleaseCandidate } from './types'

const candidate = (overrides: Partial<ReleaseCandidate>): ReleaseCandidate => ({
  title: 'Show.S01E01.1080p', downloadUrl: 'https://idx/1', indexer: 'idx', protocol: 'usenet', size: 1, seeders: 0, age: '1d',
  quality: 'WEBDL-1080p', score: 0, accepted: true, isUpgrade: false, rejections: [], match: { version: 1, matched: true, reason: 'ok' }, candidateToken: '',
  ...overrides,
})

describe('scopeLabel', () => {
  it('names episodes, packs, and whole items the way the web panel does', () => {
    expect(scopeLabel({ season: 1, episode: 4 })).toBe('S01E04')
    expect(scopeLabel({ season: 12 })).toBe('Season 12 pack')
    expect(scopeLabel({})).toBe('item')
    expect(scopeLabel({ copyId: 3 }, 'audiobook')).toBe('audiobook')
  })
})

describe('filterCandidates', () => {
  const all = [
    candidate({ title: 'Show.S01E01.2160p', accepted: true }),
    candidate({ title: 'Show.S01E01.720p', accepted: false, rejections: [{ code: 'floor', reason: 'below floor' }] }),
    candidate({ title: 'Other.S01E01.1080p', accepted: true }),
  ]
  it('keeps every candidate by default and narrows to accepted on request', () => {
    expect(filterCandidates(all, 'all', '')).toHaveLength(3)
    expect(filterCandidates(all, 'accepted', '')).toHaveLength(2)
    expect(acceptedCount(all)).toBe(2)
  })
  it('matches titles case-insensitively and trims the query', () => {
    expect(filterCandidates(all, 'all', '  720P ')).toEqual([all[1]])
    expect(filterCandidates(all, 'accepted', '720p')).toEqual([])
  })
})

describe('candidateKey', () => {
  it('prefers the server token and falls back to indexer plus link', () => {
    expect(candidateKey(candidate({ candidateToken: 'tok' }))).toBe('tok')
    const a = candidateKey(candidate({ downloadUrl: 'https://idx/1' }))
    const b = candidateKey(candidate({ downloadUrl: 'https://idx/2' }))
    expect(a).not.toBe(b)
    expect(candidateKey(candidate({ indexer: 'other' }))).not.toBe(a)
  })
})

describe('grabbedMessage', () => {
  it('points at Activity', () => {
    expect(grabbedMessage('X')).toContain('Activity')
  })
})

describe('firstSeason', () => {
  it('opens interactive search on the first real season, or specials when that is all there is', () => {
    expect(firstSeason([{ number: 0 }, { number: 1 }, { number: 2 }])).toBe(1)
    expect(firstSeason([{ number: 0 }])).toBe(0)
    expect(firstSeason([])).toBeUndefined()
  })
})
