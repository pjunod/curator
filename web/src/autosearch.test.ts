import { describe, expect, it } from 'vitest'
import type { AutoSearchResult, AutoSearchTarget } from './api'
import { describeAutoSearch } from './autosearch'

function target(over: Partial<AutoSearchTarget> = {}): AutoSearchTarget {
  return {
    wantableId: 'movie:1',
    label: 'Blade Runner (1982)',
    seen: 0,
    matched: 0,
    accepted: 0,
    ...over,
  }
}

function result(targets: AutoSearchTarget[]): AutoSearchResult {
  return { grabbed: targets.filter((t) => t.grabbed).length, targets }
}

describe('describeAutoSearch', () => {
  it('names the release it grabbed', () => {
    const msg = describeAutoSearch(
      result([target({ seen: 9, matched: 3, accepted: 2, grabbed: 'Blade.Runner.1982.2160p' })]),
    )
    expect(msg).toContain('Blade.Runner.1982.2160p')
  })

  it('counts multiple grabs rather than naming one', () => {
    const msg = describeAutoSearch(
      result([
        target({ wantableId: 'season:1:1', label: 'Show season 1', grabbed: 'Show.S01' }),
        target({ wantableId: 'season:1:2', label: 'Show season 2', grabbed: 'Show.S02' }),
      ]),
    )
    expect(msg).toContain('2 releases')
  })

  it('says the item wants nothing when there are no targets', () => {
    expect(describeAutoSearch({ grabbed: 0, targets: [] })).toContain('already has')
  })

  // The three no-grab outcomes have to read differently. They were all one
  // sentence before, which is how an auto search that never ran at all looked
  // exactly like one that ran and found nothing.
  it('distinguishes empty indexers from a non-match from a profile rejection', () => {
    const nothing = describeAutoSearch(result([target({ seen: 0 })]))
    const noMatch = describeAutoSearch(result([target({ seen: 6, matched: 0 })]))
    const declined = describeAutoSearch(result([target({ seen: 6, matched: 4, accepted: 0 })]))

    expect(nothing).toContain('No releases came back')
    expect(noMatch).toContain('none of them were')
    expect(declined).toContain('quality profile turned every one down')
    expect(new Set([nothing, noMatch, declined]).size).toBe(3)
  })

  it('tells the user monitoring is off, and how to fix it', () => {
    const msg = describeAutoSearch(result([target({ skipped: 'unmonitored' })]))
    expect(msg).toContain('not monitored')
    expect(msg).toContain('Turn monitoring on')
  })

  it('tells the user a download is already running', () => {
    const msg = describeAutoSearch(result([target({ skipped: 'downloading' })]))
    expect(msg).toContain('already in progress')
  })

  it('breaks down a mixed set of skips', () => {
    const msg = describeAutoSearch(
      result([target({ skipped: 'unmonitored' }), target({ skipped: 'downloading' })]),
    )
    expect(msg).toContain('1 not monitored')
    expect(msg).toContain('1 already downloading')
  })

  it('mentions skipped targets alongside a searched one', () => {
    const msg = describeAutoSearch(
      result([target({ seen: 3, matched: 0 }), target({ skipped: 'downloading' })]),
    )
    expect(msg).toContain('1 other target(s) skipped')
  })

  it('surfaces an error over a bare count', () => {
    const msg = describeAutoSearch(result([target({ error: 'indexer refused the query' })]))
    expect(msg).toContain('indexer refused the query')
  })
})
