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

  it('explains eligibility when there are no targets', () => {
    expect(describeAutoSearch({ grabbed: 0, targets: [] })).toContain('Check monitoring selections and air dates')
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

it('explains deferred season comparison without claiming a grab', () => {
 expect(describeAutoSearch({grabbed:0,targets:[target({skipped:'queued'})]})).toContain('Queued 1 season comparison')
})

describe('describeAutoSearch with unsearched indexers', () => {
  // A refused search and an empty one both see zero releases. Only one of
  // them means there is nothing to grab.
  it('says the indexers were not asked instead of claiming nothing exists', () => {
    const line = 'nzb.life: indexer search request allowance exhausted; retry at 2026-10-04T18:42:05Z'
    const sameDay = new Date('2026-10-04T18:36:00Z')
    const msg = describeAutoSearch(result([target({ seen: 0, incomplete: [line] })]), sameDay)
    expect(msg).not.toContain('No releases came back')
    expect(msg).toContain('not every indexer could be searched')
    expect(msg).toContain('nzb.life: indexer search request allowance exhausted')
    expect(msg).toContain('retry after')
    expect(msg).not.toContain('2026-10-04T18:42:05Z')

    // Six minutes away needs no date; three days away does.
    const weekday = new Date('2026-10-04T18:42:05Z').toLocaleDateString([], { weekday: 'short' })
    expect(msg).not.toContain(`retry after ${weekday}`)
    const earlier = describeAutoSearch(
      result([target({ seen: 0, incomplete: [line] })]),
      new Date('2026-10-01T18:36:00Z'),
    )
    expect(earlier).toContain(`retry after ${weekday}`)
  })

  it('mentions an indexer that was not searched alongside a real result', () => {
    const msg = describeAutoSearch(
      result([target({ seen: 6, matched: 0, incomplete: ['drunkenslug: timeout'] })]),
    )
    expect(msg).toContain('none of them were')
    expect(msg).toContain('Not every indexer was searched — drunkenslug: timeout')
  })

  it('does not call a season queued when its comparison was not repeated', () => {
    const all = describeAutoSearch(result([target({ skipped: 'compared_recently' })]))
    expect(all).toContain('Not searched')
    expect(all).not.toContain('Queued')
    const mixed = describeAutoSearch(
      result([target({ skipped: 'queued' }), target({ skipped: 'compared_recently' })]),
    )
    expect(mixed).toContain('Queued 1 season comparison')
    expect(mixed).toContain('1 other season comparison was completed too recently')
  })
})
