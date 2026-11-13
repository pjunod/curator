import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from '../api'
import type { ItemStatus } from '../types'
import { ItemStatusPanel } from './ItemStatusPanel'

const { alert } = vi.hoisted(() => ({ alert: vi.fn() }))
vi.mock('react-native', () => ({
  Text: 'text', View: 'view', Pressable: 'pressable',
  StyleSheet: { create: (value: unknown) => value, hairlineWidth: 1 },
  Alert: { alert },
}))
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc', ok: '#0f0', warning: '#ff0', error: '#f00', border: '#333' }) }))
vi.mock('./UI', async () => {
  const { createElement } = await import('react')
  const host = (name: string) => (props: Record<string, unknown>) => createElement(name, props, props.children as never)
  return { Button: host('button'), Panel: host('panel'), SectionTitle: host('sectiontitle') }
})
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const stage = (key: string, state: string, actions?: string[]) => ({ key, state, actions })
const status = (over: Partial<ItemStatus> = {}): ItemStatus => ({
  mediaItemId: 7, kind: 'movie', generatedAt: 1, active: true, done: false,
  headline: { state: 'active', title: 'Downloading, 3 min 48 s left' },
  stages: [stage('searched', 'done'), stage('downloading', 'active', ['open_activity', 'cancel_download']), stage('processed', 'todo'),
    stage('moved', 'todo'), stage('cinema_notified', 'todo'), stage('in_cinema', 'todo', ['confirm_cinema'])] as ItemStatus['stages'],
  download: { id: 501, release: 'Past.Lives.2023', state: 'downloading', bytes: 6.3e9, total: 18.4e9, progress: 0.34, rateBps: 48.2e6, etaSeconds: 228 },
  ...over,
})

let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined; alert.mockReset(); vi.useRealTimers() })
const labelled = (label: string) => renderer!.root.findAll((node) => typeof node.type === 'string' && node.props.label === label)
const text = () => renderer!.root.findAll((node) => node.type === 'text').flatMap((node) => node.children).filter((c) => typeof c === 'string').join('')

describe('ItemStatusPanel (mobile)', () => {
  it('shows the stages and Runner numbers, polls every 2 s while active, and reports each action under its buttons', async () => {
    vi.useFakeTimers()
    const client = {
      getItemStatus: vi.fn().mockResolvedValue(status()),
      cancelDownload: vi.fn().mockResolvedValue(undefined),
      confirmItemInCinema: vi.fn().mockResolvedValue({ queued: 1 }),
    }
    const openActivity = vi.fn()
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="movie" onInteractiveSearch={vi.fn()} onOpenActivity={openActivity} />) })
    expect(text()).toContain('Downloading, 3 min 48 s left')
    expect(text()).toContain('6.3 of 18.4 GB · 48.2 MB/s · 3 min 48 s left')
    await act(async () => { await vi.advanceTimersByTimeAsync(2_100) })
    expect(client.getItemStatus).toHaveBeenCalledTimes(2)

    await act(async () => { labelled('Cancel download')[0]!.props.onPress() })
    const buttons = alert.mock.calls[0]![2] as { text: string; onPress?: () => void }[]
    await act(async () => { buttons.find((b) => b.text === 'Cancel download')!.onPress!() })
    expect(client.cancelDownload).toHaveBeenCalledWith(501)
    expect(text()).toContain('Download cancelled in Runner; the release is blocklisted.')

    await act(async () => { labelled('Ask cinema to confirm')[0]!.props.onPress() })
    expect(text()).toContain('Asked cinema to confirm 1 folder')
    await act(async () => { labelled('Open in Activity')[0]!.props.onPress() })
    expect(openActivity).toHaveBeenCalled()
  })

  it('collapses a finished item to one line that opens again', async () => {
    const done = status({ active: false, done: true, headline: { state: 'done', title: 'In cinema, confirmed 18:36' }, download: undefined })
    const client = { getItemStatus: vi.fn().mockResolvedValue(done) }
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="movie" onInteractiveSearch={vi.fn()} />) })
    expect(text()).toContain('In cinema, confirmed 18:36')
    expect(text()).not.toContain('Processed')
    await act(async () => { renderer!.root.findAll((n) => (n.type as unknown) === 'pressable')[0]!.props.onPress() })
    expect(text()).toContain('Processed')
  })

  it('does not fold the panel away when a background poll finds it done', async () => {
    vi.useFakeTimers()
    const done = status({ active: false, done: true, headline: { state: 'done', title: 'In cinema, confirmed' }, download: undefined })
    const client = { getItemStatus: vi.fn().mockResolvedValueOnce(status()).mockResolvedValue(done) }
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="movie" onInteractiveSearch={vi.fn()} />) })
    await act(async () => { await vi.advanceTimersByTimeAsync(2_100) })
    expect(text()).toContain('In cinema, confirmed')
    expect(text()).toContain('Processed')
  })

  it("acts on an episode alone: Search now targets its wantable and Retry import its download", async () => {
    const sk = ['searched', 'downloading', 'processed', 'moved', 'cinema_notified', 'in_cinema']
    const season = status({ kind: 'series', active: false, stages: undefined, download: undefined,
      seasons: [{ season: 4, episodes: 2, inCinema: 0, onDisk: 0, active: false, summary: '0 of 2 in cinema' }],
      episodes: [
        { episodeId: 201, season: 4, episode: 1, wantableId: 'episode:7:4:1', summary: 'not searched', inPack: false,
          stages: sk.map((key, i) => ({ key, state: 'todo', actions: i === 0 ? ['search'] : undefined })) as ItemStatus['stages'] & object },
        { episodeId: 202, season: 4, episode: 2, wantableId: 'episode:7:4:2', summary: 'moved · import failed', inPack: false, downloadId: 702,
          stages: sk.map((key, i) => ({ key, state: i < 3 ? 'done' : i === 3 ? 'failed' : 'todo', actions: i === 3 ? ['retry_import'] : undefined })) as ItemStatus['stages'] & object },
      ] })
    const client = {
      getItemStatus: vi.fn().mockResolvedValue(season),
      searchWantable: vi.fn().mockResolvedValue({ scopeLabel: 'Show S04E01' }),
      retryImport: vi.fn().mockResolvedValue(undefined),
      autoSearchItem: vi.fn(),
    }
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="series" onInteractiveSearch={vi.fn()} />) })
    const pressables = () => renderer!.root.findAll((n) => (n.type as unknown) === 'pressable')
    await act(async () => { pressables()[0]!.props.onPress() }) // Season 4
    await act(async () => { pressables()[1]!.props.onPress() }) // 4×01
    await act(async () => { labelled('Search now')[0]!.props.onPress() })
    expect(client.searchWantable).toHaveBeenCalledWith('episode:7:4:1')
    expect(client.autoSearchItem).not.toHaveBeenCalled()
    expect(text()).toContain('Searching Show S04E01')
    await act(async () => { pressables()[2]!.props.onPress() }) // 4×02
    await act(async () => { labelled('Retry import')[0]!.props.onPress() })
    expect(client.retryImport).toHaveBeenCalledWith(702)
    expect(text()).toContain('Import started again.')
  })

  it('shows the open Re-match as Moving files with progress and warnings, and Stop reports under the button', async () => {
    vi.useFakeTimers()
    const moving = status({
      active: true, done: false,
      rematch: { operationId: 'op1', state: 'moving', role: 'target', ended: false, sourceItemId: 1, targetItemId: 7, sourceTitle: 'Dune (1984)', targetTitle: 'Dune (2021)',
        headline: 'Files moving here from Dune (1984)', bytesDone: 5e6, bytesTotal: 10e6, filesDone: 1, filesTotal: 2, stoppable: true,
        moves: [{ from: '/a/Dune.mkv', to: '/b/Dune.mkv', state: 'moved', bytes: 5e6 }],
        cinemaLine: 'Cinema is being told.', warnings: ['cinema still has /a/Dune.mkv: it is listed'] },
    })
    const stopped = status({ active: false, done: false })
    const client = { getItemStatus: vi.fn().mockResolvedValueOnce(moving).mockResolvedValue(stopped), cancelRematch: vi.fn().mockResolvedValue(undefined) }
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="movie" onInteractiveSearch={vi.fn()} />) })
    expect(text()).toContain('Files moving here from Dune (1984)')
    expect(text()).toContain('1 of 2 files')
    expect(text()).toContain('/a/Dune.mkv → /b/Dune.mkv')
    expect(text()).toContain('Cinema is being told.')
    expect(text()).toContain('cinema still has /a/Dune.mkv')
    await act(async () => { labelled('Stop')[0]!.props.onPress() })
    expect(client.cancelRematch).toHaveBeenCalledWith('op1')
    expect(text()).toContain('Stopping the move')
    expect(text()).not.toContain('Files moving here')
  })

  it('keeps an ended Re-match until dismissed, and drops Stop\'s answer once another operation shows', async () => {
    vi.useFakeTimers()
    const rm = (over: object) => ({ operationId: 'op1', state: 'failed', role: 'target', ended: true, sourceItemId: 1, targetItemId: 7, sourceTitle: 'Dune (1984)', targetTitle: 'Dune (2021)',
      headline: 'Moving files from Dune (1984) to Dune (2021) failed; every file is back where it was', bytesDone: 0, bytesTotal: 0, filesDone: 0, filesTotal: 0, stoppable: false,
      error: 'the target disk is full', failedMoves: [{ from: '/a.mkv', reason: 'no space left' }], ...over }) as NonNullable<ItemStatus['rematch']>
    const client = {
      getItemStatus: vi.fn().mockResolvedValueOnce(status({ active: false, rematch: rm({}) }))
        .mockResolvedValueOnce(status({ active: false }))
        .mockResolvedValue(status({ active: true, rematch: rm({ operationId: 'op2', state: 'moving', ended: false, stoppable: true, error: undefined, failedMoves: undefined, headline: 'Files moving here from Other (1999)' }) })),
      dismissRematch: vi.fn().mockResolvedValue(undefined),
    }
    await act(async () => { renderer = create(<ItemStatusPanel client={client as unknown as MonarrClient} id={7} kind="movie" onInteractiveSearch={vi.fn()} />) })
    expect(text()).toContain('the target disk is full')
    expect(text()).toContain('/a.mkv: no space left')
    await act(async () => { labelled('Dismiss')[0]!.props.onPress() })
    expect(client.dismissRematch).toHaveBeenCalledWith('op1')
    expect(text()).toContain('Dismissed.')
    await act(async () => { await vi.advanceTimersByTimeAsync(30_100) })
    expect(text()).toContain('Files moving here from Other (1999)')
    expect(text()).not.toContain('Dismissed.')
  })
})
