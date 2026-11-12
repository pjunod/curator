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
    expect(text()).toContain('Download cancelled in Runner.')

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
})
