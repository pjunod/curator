import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from '../api'
import type { MediaItemDetail, ReleaseCandidate } from '../types'
import { ReleaseSearchScreen } from './ReleaseSearchScreen'

const { alert } = vi.hoisted(() => ({ alert: vi.fn() }))
vi.mock('react-native', async () => {
  const { createElement } = await import('react')
  return {
    ActivityIndicator: 'spinner', ScrollView: 'scroll', Text: 'text', View: 'view', Pressable: 'pressable',
    // The list renders its header and every row inline so the test can find them.
    FlatList: (props: { data: unknown[]; renderItem: (info: { item: unknown }) => unknown; ListHeaderComponent: unknown; keyExtractor: (item: unknown) => string }) =>
      createElement('list', null, props.ListHeaderComponent as never, ...props.data.map((item) => createElement('row', { key: props.keyExtractor(item) }, props.renderItem({ item }) as never))),
    StyleSheet: { create: (value: unknown) => value },
    Alert: { alert },
    Linking: { openURL: vi.fn() },
  }
})
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc', ok: '#0f0', warning: '#ff0', error: '#f00' }) }))
vi.mock('../components/UI', async () => {
  const { createElement } = await import('react')
  const host = (name: string) => (props: Record<string, unknown>) => {
    const { children, left, ...rest } = props
    return createElement(name, rest, children as never, left as never)
  }
  return Object.fromEntries(['AppScreen', 'Badge', 'Button', 'Chip', 'Field', 'Header', 'IconButton', 'InlineError', 'LoadingState', 'MessageState', 'Panel'].map((name) => [name, host(name.toLowerCase())]))
})
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const candidate = (overrides: Partial<ReleaseCandidate>): ReleaseCandidate => ({
  title: 'Show.S01E01.1080p', downloadUrl: 'https://idx/1', indexer: 'idx', protocol: 'usenet', size: 1, seeders: 0, age: '1d',
  quality: 'WEBDL-1080p', score: 0, accepted: true, isUpgrade: false, rejections: [], match: { version: 1, matched: true, reason: 'ok' }, candidateToken: '',
  ...overrides,
})
const series = {
  id: 42, kind: 'series', title: 'Show', copies: [],
  seasons: [
    { number: 1, monitored: true, episodes: [{ id: 11, seasonNumber: 1, episodeNumber: 1, title: 'Pilot', airDate: '', monitored: true, hasFile: false }] },
    { number: 2, monitored: true, episodes: [] },
  ],
} as unknown as MediaItemDetail

let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined; alert.mockReset() })
const nodes = (type: string) => renderer!.root.findAll((node) => node.type === type)
const labelled = (label: string) => renderer!.root.findAll((node) => typeof node.type === 'string' && node.props.label === label)
const press = async (label: string) => { await act(async () => { labelled(label)[0]!.props.onPress() }) }
const text = () => renderer!.root.findAll((node) => node.type === 'text').flatMap((node) => node.children).filter((child) => typeof child === 'string').join('')

const makeClient = (candidates: ReleaseCandidate[], partial = false) => ({
  getLibraryItem: vi.fn().mockResolvedValue(series),
  searchReleases: vi.fn().mockResolvedValue({ candidates, partial, reason: partial ? 'one indexer timed out' : undefined }),
  grabRelease: vi.fn().mockResolvedValue({ id: 7 }),
})

describe('ReleaseSearchScreen', () => {
  it('searches the initial scope, lists every candidate, and grabs with that scope', async () => {
    const client = makeClient([
      candidate({ title: 'Show.S01E01.2160p', candidateToken: 'a', accepted: true }),
      candidate({ title: 'Show.S01E01.480p', candidateToken: 'b', accepted: false, rejections: [{ code: 'floor', reason: 'below floor' }, { code: 'x', reason: 'second reason' }] }),
    ], true)
    await act(async () => { renderer = create(<ReleaseSearchScreen client={client as unknown as MonarrClient} id={42} initialScope={{ season: 1, episode: 1 }} onBack={vi.fn()} />) })

    expect(client.searchReleases).toHaveBeenCalledWith(42, { season: 1, episode: 1 })
    expect(nodes('row')).toHaveLength(2)
    expect(text()).toContain('Results are incomplete: one indexer timed out')
    expect(text()).toContain('below floor')
    expect(text()).not.toContain('second reason')

    await press('Grab')
    expect(client.grabRelease).toHaveBeenCalledWith(expect.objectContaining({ mediaItemId: 42, season: 1, episode: 1, title: 'Show.S01E01.2160p', candidateToken: 'a' }))
    expect(text()).toContain('Grabbed Show.S01E01.2160p')
  })

  it('asks before grabbing a rejected release and never hides it', async () => {
    const client = makeClient([candidate({ title: 'Show.S01E01.480p', candidateToken: 'b', accepted: false, rejections: [{ code: 'floor', reason: 'below floor' }] })])
    await act(async () => { renderer = create(<ReleaseSearchScreen client={client as unknown as MonarrClient} id={42} initialScope={{ season: 1 }} onBack={vi.fn()} />) })

    await press('Grab anyway')
    expect(client.grabRelease).not.toHaveBeenCalled()
    expect(alert).toHaveBeenCalledOnce()
    const buttons = alert.mock.calls[0]![2] as { text: string; onPress?: () => void }[]
    await act(async () => { buttons.find((button) => button.text === 'Grab anyway')!.onPress!() })
    expect(client.grabRelease).toHaveBeenCalledWith(expect.objectContaining({ season: 1, episode: undefined, title: 'Show.S01E01.480p' }))
  })

  it('narrows to accepted releases as a view and re-searches when the scope changes', async () => {
    const client = makeClient([
      candidate({ title: 'A', candidateToken: 'a', accepted: true }),
      candidate({ title: 'B', candidateToken: 'b', accepted: false, rejections: [{ code: 'r', reason: 'rejected' }] }),
    ])
    await act(async () => { renderer = create(<ReleaseSearchScreen client={client as unknown as MonarrClient} id={42} initialScope={{ season: 1 }} onBack={vi.fn()} />) })

    expect(nodes('row')).toHaveLength(2)
    await press('Would be grabbed (1)')
    expect(nodes('row')).toHaveLength(1)
    await press('Everything (2)')
    expect(nodes('row')).toHaveLength(2)

    await press('E1')
    expect(client.searchReleases).toHaveBeenLastCalledWith(42, { copyId: undefined, season: 1, episode: 1 })
    await press('S2')
    expect(client.searchReleases).toHaveBeenLastCalledWith(42, { copyId: undefined, season: 2 })
    expect(client.searchReleases).toHaveBeenCalledTimes(3)
  })

  it('shows the empty and failed states instead of a blank list', async () => {
    const client = makeClient([])
    await act(async () => { renderer = create(<ReleaseSearchScreen client={client as unknown as MonarrClient} id={42} initialScope={{}} onBack={vi.fn()} />) })
    expect(text()).toContain('No releases found on any enabled indexer.')
    await act(async () => renderer!.unmount())

    client.searchReleases.mockRejectedValueOnce(new Error('indexer exploded'))
    await act(async () => { renderer = create(<ReleaseSearchScreen client={client as unknown as MonarrClient} id={42} initialScope={{}} onBack={vi.fn()} />) })
    expect(nodes('inlineerror').some((node) => node.props.message === 'indexer exploded')).toBe(true)
  })
})
