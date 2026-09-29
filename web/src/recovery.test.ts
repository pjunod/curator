import { describe, expect, it } from 'vitest'
import type { MediaItemSummary, RunnerRecovery } from './api'
import { baseName, buildRecoveryRequest, filterTitles, firstBadEpisode, fmtAgo, handoffBytes, handoffName, handoffStatus, parseEpisodeTarget, previewBlocker, titleOptions } from './pages/Recovery'

const handoff = (extra: Partial<RunnerRecovery> = {}): RunnerRecovery => ({
  id: '70a9015f567db76ce0183b149c371c84', client_id: 1, state: 'published', manifest_digest: 'd', installation: 'i',
  files: [{ id: 'a', path: 'Fright.Night.1985.2160p-xpost.mkv]-[1_2] - __ yEnc 50232852618 (1_70080)', bytes: 50232852618, sha256: 'x' }],
  ...extra,
})

describe('handoffName', () => {
  it('names a handoff by the folder Runner staged it from', () => {
    expect(handoffName(handoff({ source: '/working/monarr/completed/Fright.Night.1985.2160p.UHD-RU4HD-xpost' }))).toBe('Fright.Night.1985.2160p.UHD-RU4HD-xpost')
  })
  it('falls back to the first file for a handoff from an older Runner, then to the id', () => {
    expect(handoffName(handoff())).toMatch(/^Fright\.Night/)
    expect(handoffName(handoff({ files: [] }))).toBe('70a9015f567db76ce0183b149c371c84')
  })
  it('baseName strips trailing slashes', () => {
    expect(baseName('/a/b/c/')).toBe('c')
    expect(baseName('c')).toBe('c')
    expect(baseName(undefined)).toBe('')
  })
})

describe('handoffStatus', () => {
  // Field report 2026-09-28: a handoff whose staging had FAILED in Runner was
  // offered in the select as "… · failed · 1 files", its error printed as an
  // anonymous alert above, and Preview then died on the missing mount.
  it('only a published or claimed handoff can be chosen', () => {
    expect(handoffStatus(handoff()).selectable).toBe(true)
    expect(handoffStatus(handoff({ state: 'claimed', consumer: 'curator' })).selectable).toBe(true)
    for (const state of ['staging', 'publishing', 'failed', 'cancel_pending', 'imported', 'partial', 'cancelled', 'weird']) {
      expect(handoffStatus(handoff({ state })).selectable, state).toBe(false)
    }
  })
  it('a failed staging carries Runner’s reason and says where to fix it', () => {
    const st = handoffStatus(handoff({ state: 'failed', error: 'filesystem: write /processing/recovery/.staging/x/payload/a.mkv: Invalid argument (os error 22)' }))
    expect(st.pill).toBe('error')
    expect(st.label).toBe('staging failed')
    expect(st.note).toContain('Invalid argument (os error 22)')
    expect(st.note).toContain('Runner’s Files tab')
  })
  it('a copy still in flight says so instead of failing later', () => {
    expect(handoffStatus(handoff({ state: 'staging' })).note).toMatch(/still copying/)
  })
})

describe('previewBlocker', () => {
  const ready = handoff()
  it('explains each missing precondition, in the order a person meets them', () => {
    expect(previewBlocker({ mount: true, handoff: undefined, itemId: 0, pending: false })).toBe('Choose a handoff first.')
    expect(previewBlocker({ mount: true, handoff: handoff({ state: 'failed' }), itemId: 0, pending: false })).toBe('That handoff is not ready to import.')
    expect(previewBlocker({ mount: true, handoff: handoff({ files: [] }), itemId: 0, pending: false })).toBe('That handoff has no files.')
    expect(previewBlocker({ mount: true, handoff: ready, itemId: 0, pending: false })).toMatch(/Pick the library title/)
    expect(previewBlocker({ mount: false, handoff: ready, itemId: 7, pending: false })).toMatch(/recovery mount is not available.*Settings → Dev/)
  })
  it('a malformed episode mapping blocks the preview instead of being dropped from the request', () => {
    expect(previewBlocker({ mount: true, handoff: ready, itemId: 7, pending: false, badEpisode: 'a.mkv' })).toBe('Fix the episode mapping for a.mkv (season:episodes), or clear it.')
    expect(firstBadEpisode(handoff({ files: [{ id: 'a', path: 'a.mkv', bytes: 1, sha256: '' }, { id: 'b', path: 'b.mkv', bytes: 1, sha256: '' }] }), { a: '1:2', b: 'S01E02' })).toBe('b.mkv')
    expect(firstBadEpisode(handoff(), { a: '  ' })).toBe('')
    expect(firstBadEpisode(undefined, { a: 'junk' })).toBe('')
  })
  it('is empty when everything is in place, or while a preview is already running', () => {
    expect(previewBlocker({ mount: true, handoff: ready, itemId: 7, pending: false })).toBe('')
    expect(previewBlocker({ mount: null, handoff: ready, itemId: 7, pending: false })).toBe('')
    expect(previewBlocker({ mount: false, handoff: undefined, itemId: 0, pending: true })).toBe('')
  })
})

describe('parseEpisodeTarget', () => {
  it('reads season:episodes with commas or spaces', () => {
    expect(parseEpisodeTarget('1:2')).toEqual({ season: 1, episodes: [2] })
    expect(parseEpisodeTarget(' 3 : 4, 5 ')).toEqual({ season: 3, episodes: [4, 5] })
    expect(parseEpisodeTarget('2 7 8')).toEqual({ season: 2, episodes: [7, 8] })
  })
  it('treats blank or malformed text as "let the filename decide"', () => {
    for (const bad of ['', '1', 'S01E02', '1:', ':2', '1:0', 'a:b']) expect(parseEpisodeTarget(bad), bad).toBeNull()
  })
})

describe('buildRecoveryRequest', () => {
  // The body the backend expects — every field, in the shape the old page sent.
  it('sends every field, keeps only parseable episode mappings', () => {
    const h = handoff({ client_id: 3, files: [{ id: 'a', path: 'a.mkv', bytes: 1, sha256: '' }, { id: 'b', path: 'b.mkv', bytes: 1, sha256: '' }] })
    expect(buildRecoveryRequest({ handoff: h, itemId: 7, copyId: 2, episodeText: { a: '1:2,3', b: '' }, unverified: true })).toEqual({
      client_id: 3, recovery_id: h.id, media_item_id: 7, copy_id: 2, file_ids: ['a', 'b'],
      episode_targets: { a: { season: 1, episodes: [2, 3] } }, target_generation: '', accept_unverified: true,
    })
  })
})

describe('titleOptions', () => {
  // Review finding: a search that no longer matches the chosen title left the
  // controlled <select> blank while the request still targeted that title.
  const items = [{ id: 1, kind: 'movie', title: 'Fright Night', year: 1985 }, { id: 2, kind: 'series', title: 'Archer', year: 2009 }] as unknown as MediaItemSummary[]
  it('always includes the chosen title, first', () => {
    expect(titleOptions(items, 'arch', 1).map(i => i.id)).toEqual([1, 2])
    expect(titleOptions(items, 'arch', 2).map(i => i.id)).toEqual([2])
    expect(titleOptions(items, 'zzz', 0).map(i => i.id)).toEqual([])
    expect(titleOptions(undefined, '', 5)).toEqual([])
  })
})

describe('filterTitles and sizes', () => {
  const items = [
    { id: 1, kind: 'movie', title: 'Fright Night', year: 1985 },
    { id: 2, kind: 'series', title: 'Archer', year: 2009 },
    { id: 3, kind: 'book', title: 'Night Shift', year: 1978, author: 'Stephen King' },
  ] as unknown as MediaItemSummary[]
  it('matches title, year and author, case-insensitively, keeping server order', () => {
    expect(filterTitles(items, '').map(i => i.id)).toEqual([1, 2, 3])
    expect(filterTitles(items, 'night').map(i => i.id)).toEqual([1, 3])
    expect(filterTitles(items, '1985').map(i => i.id)).toEqual([1])
    expect(filterTitles(items, 'king').map(i => i.id)).toEqual([3])
    expect(filterTitles(undefined, 'x')).toEqual([])
  })
  it('sums the payload and renders age', () => {
    expect(handoffBytes(handoff({ files: [{ id: 'a', path: 'a', bytes: 10, sha256: '' }, { id: 'b', path: 'b', bytes: 5, sha256: '' }] }))).toBe(15)
    const now = 1_790_000_000_000
    expect(fmtAgo(now / 1000 - 30, now)).toBe('30s ago')
    expect(fmtAgo(now / 1000 - 3600 * 5, now)).toBe('5h ago')
    expect(fmtAgo(undefined, now)).toBe('')
  })
})
