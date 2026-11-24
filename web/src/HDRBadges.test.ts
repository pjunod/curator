import { describe, expect, it } from 'vitest'
import type { ReleaseCandidate } from './api'
import { probeSummary } from './HDRBadges'

const cand = (bytes?: number): ReleaseCandidate =>
  ({
    title: 't', downloadUrl: 'u', indexer: 'i', protocol: 'usenet', size: 1, seeders: 0, age: '', quality: '',
    score: 0, accepted: true, isUpgrade: false, rejections: [], match: { matched: true } as never, candidateToken: '',
    probe: bytes === undefined ? undefined : { status: 'measured', bytes },
  }) as ReleaseCandidate

describe('probeSummary', () => {
  it('says nothing when no release was probed', () => {
    expect(probeSummary([cand(), cand()])).toBe('')
  })
  it('counts the probed releases and the bytes read', () => {
    expect(probeSummary([cand(3 << 20), cand(1 << 20), cand()])).toMatch(/^3 candidates · top 2 probed · 4(\.0)? MB read in total$/)
  })
})
