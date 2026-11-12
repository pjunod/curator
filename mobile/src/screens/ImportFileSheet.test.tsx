import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { MonarrClient } from '../api'
import { Button } from '../components/UI'
import type { MediaItemDetail } from '../types'
import { ImportFileSheet } from './ImportFileSheet'

vi.mock('react-native', () => ({
  ActivityIndicator: 'spinner', Modal: 'modal', ScrollView: 'scroll', Switch: 'switch', Text: 'text', TextInput: 'input', View: 'view', Image: 'image', Pressable: 'pressable',
  StyleSheet: { create: (value: unknown) => value, hairlineWidth: 1 },
  Keyboard: { dismiss: vi.fn() }, Linking: { openURL: vi.fn() },
  AppState: { addEventListener: () => ({ remove: vi.fn() }) },
}))
vi.mock('react-native-safe-area-context', () => ({ SafeAreaProvider: 'safe-area-provider', SafeAreaView: 'safe-area-view' }))
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc', border: '#222', raised: '#111' }) }))
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const item = { id: 4242, kind: 'series', title: 'Slow Horses', path: '/tv/Slow Horses', copies: [],
  seasons: [{ number: 4, monitored: true, episodes: [4, 5].map((n) => ({ id: n, seasonNumber: 4, episodeNumber: n, title: '', airDate: '', monitored: true, hasFile: false })) }] } as unknown as MediaItemDetail
const loose = '/downloads/Slow.Horses.S04E05.mkv'

let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined })
const button = (prefix: string) => renderer!.root.findAll((n) => n.type === Button && String(n.props.label).startsWith(prefix))[0]!
const flush = () => act(async () => { await new Promise((r) => setTimeout(r, 0)) })

describe('ImportFileSheet', () => {
  it('opens at the completed folder, confirms a plan, and starts the import with Remove original off', async () => {
    const client = {
      getRootFolders: vi.fn().mockResolvedValue([]),
      getManualImportDefaultPath: vi.fn().mockResolvedValue({ path: '/downloads' }),
      browseFilesystem: vi.fn().mockResolvedValue({ path: '/downloads/', parent: '/', dirs: [] }),
      scanImportFolder: vi.fn().mockResolvedValue([{ path: loose, name: 'Slow.Horses.S04E05.mkv', size: 10, kind: 'video', quality: 'WEBDL-1080p', season: 4, episodes: [5] }]),
      planImport: vi.fn().mockResolvedValue([{ path: loose, quality: 'WEBDL-1080p', season: 4, episodes: [5], subtitles: 0,
        destination: '/tv/Slow Horses/Season 04/Slow Horses - S04E05 [WEBDL-1080p].mkv', action: 'new', reason: 'nothing is there yet', mode: 'hardlink' }]),
      manualImport: vi.fn().mockResolvedValue({ downloadId: 91 }),
      getQueueItem: vi.fn().mockResolvedValue({ id: 91, state: 'imported', handoff: [{ step: 'originals_removal_done', at: 1, detail: 'Remove original: removed 1 original(s)' }] }),
    }
    await act(async () => { renderer = create(<ImportFileSheet client={client as unknown as MonarrClient} item={item} onClose={vi.fn()} />) })
    await flush()
    expect(client.scanImportFolder).toHaveBeenCalledWith('/downloads')
    const select = renderer!.root.findAll((n) => n.type === ('switch' as never) && n.props.accessibilityLabel === 'Select Slow.Horses.S04E05.mkv')[0]!
    await act(async () => { select.props.onValueChange(true) })
    await act(async () => { button('Next').props.onPress() })
    await flush()
    expect(client.planImport).toHaveBeenCalledWith({ mediaItemId: 4242, copyId: undefined, removeOriginal: false, files: [{ path: loose, season: 4, episodes: [5] }] })
    const remove = renderer!.root.findAll((n) => n.type === ('switch' as never) && n.props.accessibilityLabel === 'Remove original')[0]!
    expect(remove.props.value).toBe(false)
    await act(async () => { button('Import 1 file').props.onPress() })
    await flush()
    expect(client.manualImport).toHaveBeenCalledWith(expect.objectContaining({ path: '/downloads', paths: [loose], removeOriginal: false }))
    expect(button('Done')).toBeTruthy()
    await flush()
    expect(client.getQueueItem).toHaveBeenCalledWith(91)
    const texts = renderer!.root.findAll((n) => n.type === ('text' as never)).map((n) => [].concat(n.props.children).join(''))
    expect(texts).toContain('Activity #91: imported')
    expect(texts).toContain('Remove original: removed 1 original(s)')
  })
})
