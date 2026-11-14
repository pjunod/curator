import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { Availability, MediaItemSummary, QueueItem } from './api'
import { LibraryCardActivity, LibraryCardFooter } from './LibraryCardFooter'

const fact = (patch: Partial<Availability> = {}): Availability => ({
  catalog: 'present', presence: 'present', expected: 1, present: 1,
  copyId: 0, offloaded: 0, acquisition: 'none', ...patch,
})
const item = (patch: Partial<MediaItemSummary> = {}): MediaItemSummary => ({
  id: 1, kind: 'movie', title: 'A movie', year: 2024, author: '', posterPath: '',
  monitored: true, path: '', rating: 0, ratingVotes: 0, ratings: [],
  episodeCount: 0, episodeFileCount: 0, fileCount: 2, addedAt: '', ...patch,
})
const footer = (patch: Partial<MediaItemSummary> = {}) => renderToStaticMarkup(<LibraryCardFooter item={item(patch)} />)
const activity = (state: string, progress = 0.38) => renderToStaticMarkup(
  <LibraryCardActivity downloads={[{ state, progress } as QueueItem]} active />,
)

describe('library card facts', () => {
  it('never uses file records as proof of availability', () => {
    for (const availability of [undefined, fact({ presence: 'unknown' }), fact({ catalog: 'unknown' })]) {
      const html = footer({ availability })
      expect(html).toContain('Unverified')
      expect(html).toContain('2 records')
      expect(html).not.toContain('status-available')
    }
  })

  it('shows observed availability independently of the recorded file count', () => {
    const html = footer({ availability: fact(), fileCount: 0 })
    expect(html).toContain('status-available')
    expect(html).toContain('0 records')
    expect(footer({ availability: fact({ presence: 'missing' }) })).toContain('Absent at last check')
  })

  it('preserves identity conflicts and affirmative catalog absence', () => {
    expect(footer({ availability: fact({ catalog: 'ambiguous' }) })).toContain('Identity conflict')
    expect(footer({ availability: fact({ catalog: 'absent' }) })).toContain('Not in library')
  })

  it('keeps checked availability authoritative over legacy offload flags', () => {
    expect(footer({ offloaded: true })).toContain('Offloaded')
    expect(footer({ kind: 'series', offloaded: true, offloadedEpisodes: 1 })).toContain('Unverified')
    expect(footer({ offloaded: true, availability: fact() })).toContain('status-available')
    expect(footer({ availability: fact({ presence: 'offloaded' }) })).toContain('Offloaded')
  })

  it('separates checked episode completeness from episode records', () => {
    const html = footer({ kind: 'series', episodeFileCount: 10, episodeCount: 12,
      availability: fact({ presence: 'partial', present: 8, expected: 12 }) })
    expect(html).toContain('8/12 episodes available')
    expect(html).toContain('10 eps recorded')
  })

  it('retains book authors and independently curated editions', () => {
    const html = footer({ kind: 'book', author: 'An Author', bookTypes: ['ebook', 'audiobook'], fileCount: 1 })
    expect(html).toContain('An Author · Ebook · Audiobook')
    expect(html).toContain('1 record')
    expect(html).toContain('Unverified')
  })
})

describe('library card activity', () => {
  it('shows progress only for a measured download', () => {
    expect(activity('downloading')).toContain('aria-valuenow="38"')
    expect(activity('downloading', Number.NaN)).not.toContain('progressbar')
    expect(activity('downloading', 1.5)).toContain('aria-valuenow="100"')
  })

  it('distinguishes queued, downloaded, and importing phases', () => {
    for (const [state, label] of [['grabbed', 'Download queued'], ['downloaded', 'Downloaded'],
      ['awaiting_import', 'Waiting to import'], ['importing', 'Waiting to retry import']]) {
      const html = activity(state)
      expect(html).toContain(label)
      expect(html).not.toContain('progressbar')
    }
  })

  it('does not invent progress for acquisition activity without a queue item', () => {
    expect(renderToStaticMarkup(<LibraryCardActivity active />)).toContain('Work in progress')
    expect(renderToStaticMarkup(<LibraryCardActivity active={false} />)).toBe('')
  })

  it('does not attribute a single job percentage to a title with multiple jobs', () => {
    const html = renderToStaticMarkup(<LibraryCardActivity active downloads={[
      { state: 'downloading', progress: 0.2 } as QueueItem,
      { state: 'downloading', progress: 0.9 } as QueueItem,
    ]} />)
    expect(html).toContain('2 active jobs')
    expect(html).not.toContain('progressbar')
  })

  it('does not show completed or cancelled import attempts as active', () => {
    for (const outcome of ['cancelled', 'completed'] as const) {
      expect(renderToStaticMarkup(<LibraryCardActivity active downloads={[
        { state: 'importing', importControl: { outcome } } as QueueItem,
      ]} />)).toBe('')
    }
  })

  it('labels processing, holds, and cancellation without a transfer percentage', () => {
    for (const [patch, label] of [
      [{ stage: 'unpack' }, 'Extracting'],
      [{ stage: 'par_repair' }, 'Repairing'],
      [{ importState: 'queued' }, 'Waiting to import'],
      [{ importControl: { outcome: 'pending' } }, 'Cancelling import'],
      [{ control: { version: 1, lifecycle: 'held' } }, 'Held'],
    ] as const) {
      const html = renderToStaticMarkup(<LibraryCardActivity active downloads={[
        { state: 'downloading', progress: 1, ...patch } as QueueItem,
      ]} />)
      expect(html).toContain(label)
      expect(html).not.toContain('progressbar')
    }
  })

  it('prefers measured transfer bytes over a stale transfer fraction', () => {
    const html = renderToStaticMarkup(<LibraryCardActivity active downloads={[
      { state: 'downloading', stage: 'downloading', progress: 1, bytes: 25, total: 100 } as QueueItem,
    ]} />)
    expect(html).toContain('aria-valuenow="25"')
  })
})
