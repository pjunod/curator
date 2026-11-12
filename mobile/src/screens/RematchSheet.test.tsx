import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from '../api'
import { Button, Chip } from '../components/UI'
import type { MediaItemDetail } from '../types'
import { RematchSheet } from './RematchSheet'

vi.mock('react-native', () => ({
  ActivityIndicator: 'spinner', Modal: 'modal', ScrollView: 'scroll', Switch: 'switch', Text: 'text', TextInput: 'input', View: 'view', Image: 'image', Pressable: 'pressable',
  StyleSheet: { create: (value: unknown) => value, hairlineWidth: 1 },
  Keyboard: { dismiss: vi.fn() }, Linking: { openURL: vi.fn() },
  AppState: { addEventListener: () => ({ remove: vi.fn() }) },
}))
vi.mock('react-native-safe-area-context', () => ({ SafeAreaProvider: 'safe-area-provider', SafeAreaView: 'safe-area-view' }))
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc', border: '#222', raised: '#111', warning: '#f80', error: '#f00' }) }))
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const ep5 = '/tv/Slow Horses/Season 04/Slow Horses - S04E05 - Five [WEBDL-1080p].mkv'
const ep6 = '/tv/Slow Horses/Season 04/Slow Horses - S04E06 - Six [WEBDL-1080p].mkv'
const item = { id: 4244, kind: 'series', title: 'Slow Horses', path: '/tv/Slow Horses', copies: [],
  files: [{ id: 61, path: ep5, size: 10, episodeIds: [905] }],
  seasons: [{ number: 4, monitored: true, episodes: [5, 6].map((n) => ({ id: 900 + n, seasonNumber: 4, episodeNumber: n, title: '', airDate: '', monitored: true, hasFile: n === 5 })) }] } as unknown as MediaItemDetail

let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined })
const flush = () => act(async () => { await new Promise((r) => setTimeout(r, 0)) })
const find = (type: unknown, prefix: string) => renderer!.root.findAll((n) => n.type === type && String(n.props.label).startsWith(prefix))[0]!
const texts = () => renderer!.root.findAll((n) => n.type === ('text' as never)).map((n) => [].concat(n.props.children).join(''))

describe('RematchSheet', () => {
  it('moves one file to another episode of the same show and shows the result', async () => {
    const preview = (chosen: boolean) => ({ source: { id: 4244, kind: 'series', title: 'Slow Horses', inLibrary: true }, target: { id: 4244, kind: 'series', title: 'Slow Horses', inLibrary: true },
      moves: [{ fileId: 61, from: ep5, to: chosen ? ep6 : ep5, season: 4, episodes: [chosen ? 6 : 5], mode: chosen ? 'rename' : 'relink', bytes: 10 }], leftOut: [],
      sourceOutcome: 'stays', sourceReason: 'the files stay in Slow Horses', blocklist: [], blocklistOn: true,
      episodes: [{ season: 4, episode: 5, hasFile: true }, { season: 4, episode: 6, hasFile: false }], sourceRevision: 3, targetRevision: 3, fingerprint: chosen ? 'fp6' : 'fp5', summary: 'Move 1 file' })
    const client = {
      getLibrary: vi.fn().mockResolvedValue([]), searchMetadata: vi.fn().mockResolvedValue([]),
      previewRematch: vi.fn().mockImplementation((_: number, req: { choices?: { episodes?: number[] }[] }) => Promise.resolve(preview(req.choices?.[0]?.episodes?.[0] === 6))),
      startRematch: vi.fn().mockResolvedValue({ operationId: 'rm-2', state: 'complete', bytesDone: 0, bytesTotal: 0, result: { moved: [{ from: ep5, to: ep6 }] }, cinema: [] }),
      getRematch: vi.fn(), cancelRematch: vi.fn(),
    }
    await act(async () => { renderer = create(<RematchSheet client={client as unknown as MonarrClient} item={item} onClose={vi.fn()} />) })
    await act(async () => { find(Chip, 'Move Slow Horses - S04E05').props.onPress() })
    await flush()
    expect(client.previewRematch).toHaveBeenLastCalledWith(4244, { target: { itemId: 4244 }, fileIds: [61], choices: [] })
    await act(async () => { find(Chip, 'S04E06').props.onPress() })
    await flush()
    expect(texts()).toContain(`→ ${ep6}`)
    await act(async () => { find(Button, 'Move').props.onPress() })
    await flush()
    expect(client.startRematch).toHaveBeenCalledWith(4244, expect.objectContaining({ target: { itemId: 4244 }, fileIds: [61],
      choices: [{ fileId: 61, season: 4, episodes: [6] }], fingerprint: 'fp6', sourceRevision: 3, targetRevision: 3 }))
    expect(texts()).toContain('Moved 1 file.')
    expect(find(Button, 'Done')).toBeTruthy()
  })
})
