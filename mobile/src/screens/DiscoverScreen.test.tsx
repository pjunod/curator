import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'
import type { MonarrClient } from '../api'
import type { SearchResult } from '../types'
import { AddSheet } from './DiscoverScreen'

vi.mock('react-native', () => ({
  ActivityIndicator: 'spinner', Modal: 'modal', ScrollView: 'scroll', Switch: 'switch', Text: 'text', View: 'view', Image: 'image', Pressable: 'pressable',
  StyleSheet: { create: (value: unknown) => value },
  Keyboard: { dismiss: vi.fn() }, Linking: { openURL: vi.fn() },
  AppState: { addEventListener: () => ({ remove: vi.fn() }) },
}))
vi.mock('react-native-safe-area-context', () => ({ useSafeAreaInsets: () => ({ bottom: 34, top: 0, left: 0, right: 0 }) }))
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc' }) }))
vi.mock('../preferences-context', () => ({ usePreferences: () => ({ itemSize: 'medium' }) }))
vi.mock('../components/UI', async () => {
  const { createElement } = await import('react')
  const host = (name: string) => (props: Record<string, unknown>) => createElement(name, props, props.children as never, props.left as never)
  return Object.fromEntries(['AppScreen', 'Button', 'Chip', 'Field', 'Header', 'IconButton', 'InlineError', 'LoadingState', 'MessageState', 'Panel', 'SectionTitle', 'Wordmark', 'Badge'].map((name) => [name, host(name.toLowerCase())]))
})
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const original: SearchResult = { kind: 'series', tmdbId: 0, tvdbId: 414217, imdbId: 'tt16867040', hydrationSource: 'tvmaze', title: 'Cunk', year: 2022, overview: 'Original synopsis', posterPath: '', inLibrary: false }
let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined })
const button = (label: string) => renderer!.root.findAll((node) => typeof node.type === 'string' && node.props.label === label)[0]!
const press = async (label: string) => { await act(async () => { button(label).props.onPress() }) }
const makeClient = () => ({
  getRootFolders: vi.fn().mockResolvedValue([{ id: 8, path: '/tv', kind: 'series' }]),
  getProfiles: vi.fn().mockResolvedValue([]),
  getMetadataPreview: vi.fn().mockResolvedValue({ kind: 'series', title: 'Cunk', overview: 'Full synopsis', ids: { tvdb: 414217, tmdb: 999 }, ownership: 'absent', addability: 'supported' }),
  addLibraryItem: vi.fn().mockResolvedValue({ id: 77 }),
})

describe('native preview and add steps', () => {
  it('retains options in one Modal and posts the original identity once', async () => {
    const client = makeClient(), onAdded = vi.fn(), onClose = vi.fn()
    await act(async () => { renderer = create(<AddSheet client={client as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep="preview" onAdded={onAdded} onClose={onClose} />) })
    expect(renderer!.root.findAllByType('modal' as never)).toHaveLength(1)
    await press('Continue to add')
    await act(async () => { renderer!.root.findAllByType('switch' as never)[0]!.props.onValueChange(false) })
    await press('Back to details')
    await press('Continue to add')
    expect(renderer!.root.findAllByType('switch' as never)[0]!.props.value).toBe(false)
    await act(async () => { button('Add Cunk').props.onPress(); button('Add Cunk').props.onPress() })
    expect(client.addLibraryItem).toHaveBeenCalledOnce()
    expect(client.addLibraryItem).toHaveBeenCalledWith(expect.objectContaining({ tvdbId: 414217, tmdbId: undefined, imdbId: original.imdbId, hydrationSource: 'tvmaze', rootFolderId: 8, monitored: false, searchNow: false }))
    expect(onAdded).toHaveBeenCalledWith(77)
  })
  it('direct Add goes straight to options and system Back closes', async () => {
    const client = makeClient(), onClose = vi.fn()
    await act(async () => { renderer = create(<AddSheet client={client as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep="options" onAdded={vi.fn()} onClose={onClose} />) })
    expect(button('Add Cunk')).toBeDefined()
    await act(async () => { renderer!.root.findByType('modal' as never).props.onRequestClose() })
    expect(onClose).toHaveBeenCalledOnce()
  })
  it('ignores an old response after selecting another title', async () => {
    const client = makeClient()
    let resolveOld!: (value: unknown) => void
    client.getMetadataPreview.mockImplementationOnce(() => new Promise((resolve) => { resolveOld = resolve }))
    const props = { client: client as unknown as MonarrClient, initialBookType: 'ebook' as const, initialStep: 'preview' as const, onAdded: vi.fn(), onClose: vi.fn() }
    await act(async () => { renderer = create(<AddSheet {...props} item={original} />) })
    await act(async () => { renderer!.update(<AddSheet {...props} item={{ ...original, title: 'Another', tvdbId: 777 }} />) })
    // The second response contradicts its requested identity and must block Add.
    await act(async () => { resolveOld({ kind: 'series', title: 'STALE TITLE', overview: 'STALE OVERVIEW', ids: { tvdb: 414217 }, ownership: 'absent', addability: 'supported' }) })
    expect(JSON.stringify(renderer!.toJSON())).not.toContain('STALE TITLE')
    expect(button('Continue to add').props.disabled).toBe(true)
  })
  it('keeps a conflict blocked across failed retries until verified success', async () => {
    const client = makeClient()
    client.getMetadataPreview.mockRejectedValueOnce(new ApiError('Conflict', 409, 'identity_conflict'))
      .mockRejectedValueOnce(new ApiError('Offline', 503, 'provider_unavailable'))
    await act(async () => { renderer = create(<AddSheet client={client as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep="preview" onAdded={vi.fn()} onClose={vi.fn()} />) })
    expect(button('Continue to add').props.disabled).toBe(true)
    await press('Retry details')
    expect(button('Continue to add').props.disabled).toBe(true)
    await press('Retry details')
    expect(button('Continue to add').props.disabled).toBe(false)
    const footer = renderer!.root.findAll((node) => node.type === ('view' as never) && Array.isArray(node.props.style) && node.props.style.some((value: { paddingBottom?: number }) => value?.paddingBottom === 34))
    expect(footer).toHaveLength(1)
  })

  it('keeps unsupported hydration blocked through unavailable retries', async () => {
    const client = makeClient()
    client.getMetadataPreview.mockRejectedValueOnce(new ApiError('Unsupported title', 422, 'unsupported_hydration'))
      .mockRejectedValueOnce(new ApiError('Offline', 503, 'provider_unavailable'))
    await act(async () => { renderer = create(<AddSheet client={client as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep="preview" onAdded={vi.fn()} onClose={vi.fn()} />) })
    expect(button('Continue to add').props.disabled).toBe(true)
    expect(button('IMDb ↗')).toBeDefined()
    await press('Retry details')
    expect(button('Continue to add').props.disabled).toBe(true)
    await press('Retry details')
    expect(button('Continue to add').props.disabled).toBe(false)
  })

})
