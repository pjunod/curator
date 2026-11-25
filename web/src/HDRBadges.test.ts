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

describe('orderLabels', () => {
  it('merges tiers the device ties, the way the server sentence does', async () => {
    const { orderLabels } = await import('./HDRPreferences')
    const d = {
      hdrOrder: ['dv-p7-fel', 'dv-p7-mel', 'dv-p8', 'hdr10plus', 'hdr10'] as const,
      hdrTies: [['dv-p7-fel', 'dv-p7-mel', 'dv-p8']] as const,
    }
    const options = [
      { tier: 'dv-p7-fel', label: 'DV P7 FEL' }, { tier: 'dv-p7-mel', label: 'DV P7 MEL' },
      { tier: 'dv-p8', label: 'DV P8' }, { tier: 'hdr10plus', label: 'HDR10+' }, { tier: 'hdr10', label: 'HDR10' },
    ] as const
    expect(orderLabels({ hdrOrder: [...d.hdrOrder], hdrTies: d.hdrTies.map((g) => [...g]) }, [...options]).join(' › '))
      .toBe('DV P7 FEL = DV P7 MEL = DV P8 › HDR10+ › HDR10')
  })
})
