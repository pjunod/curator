import { act, create } from 'react-test-renderer'
import type { ReactTestRenderer } from 'react-test-renderer'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'
import type { MonarrClient } from '../api'
import type { SearchResult } from '../types'
import { Button, IconButton } from '../components/UI'
import { TopNavigationProvider } from '../navigation-context'
import { AddSheet, DiscoverScreen } from './DiscoverScreen'

vi.mock('react-native', () => ({
  ActivityIndicator: 'spinner', Modal: 'modal', ScrollView: 'scroll', Switch: 'switch', Text: 'text', TextInput: 'input', View: 'view', Image: 'image', Pressable: 'pressable',
  StyleSheet: { create: (value: unknown) => value },
  Keyboard: { dismiss: vi.fn() }, Linking: { openURL: vi.fn() },
  AppState: { addEventListener: () => ({ remove: vi.fn() }) },
}))
vi.mock('react-native-safe-area-context', () => ({ SafeAreaProvider: 'safe-area-provider', SafeAreaView: 'safe-area-view' }))
vi.mock('../theme', () => ({ useTheme: () => ({ text: '#fff', muted: '#aaa', accent: '#abc' }) }))
vi.mock('../preferences-context', () => ({ usePreferences: () => ({ itemSize: 'medium' }) }))
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

const original: SearchResult = { kind: 'series', tmdbId: 0, tvdbId: 414217, imdbId: 'tt16867040', hydrationSource: 'tvmaze', title: 'Cunk', year: 2022, overview: 'Original synopsis', posterPath: '', inLibrary: false }
let renderer: ReactTestRenderer | undefined
afterEach(async () => { if (renderer) await act(async () => renderer!.unmount()); renderer = undefined })
const button = (label: string) => renderer!.root.findAll((node) => (node.type === Button || node.type === IconButton) && node.props.label === label)[0]!
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

  it.each([false, true])('keeps the modal safe area independent of top navigation (%s)', async (topNavigation) => {
    const onClose = vi.fn()
    await act(async () => { renderer = create(
      <TopNavigationProvider active={topNavigation}>
        <AddSheet client={makeClient() as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep="preview" onAdded={vi.fn()} onClose={onClose} />
      </TopNavigationProvider>,
    ) })
    const modal = renderer!.root.findByType('modal' as never)
    const provider = modal.findByType('safe-area-provider' as never)
    const safeAreas = provider.findAllByType('safe-area-view' as never)
    expect(safeAreas[0]!.props.edges).toContain('top')
    expect(safeAreas[1]!.props.edges).toEqual({ bottom: 'maximum' })
    // Close remains outside the scrollable synopsis, in the modal's safe area.
    expect(modal.findByType('scroll' as never).findAllByType(IconButton)).toHaveLength(0)
    await act(async () => { provider.findByType(IconButton).findByType('pressable' as never).props.onPress() })
    expect(onClose).toHaveBeenCalledOnce()
  })

  it.each(['preview', 'options'] as const)('shows the entire synopsis before adding from %s', async (initialStep) => {
    const client = makeClient()
    const overview = 'A long synopsis paragraph. '.repeat(100) + '\n\nThe final paragraph.'
    let resolvePreview!: (value: unknown) => void
    client.getMetadataPreview.mockImplementationOnce(() => new Promise((resolve) => { resolvePreview = resolve }))
    await act(async () => { renderer = create(<AddSheet client={client as unknown as MonarrClient} item={original} initialBookType="ebook" initialStep={initialStep} onAdded={vi.fn()} onClose={vi.fn()} />) })
    const synopsis = (value: string) => renderer!.root.findByType('scroll' as never).findAll((node) => node.type === 'text' && node.props.children === value)[0]!
    expect(synopsis(original.overview).props.numberOfLines).toBeUndefined()
    await act(async () => { resolvePreview({ kind: original.kind, ids: { tvdb: original.tvdbId }, overview, ownership: 'absent', addability: 'supported' }) })
    expect(synopsis(overview).props.numberOfLines).toBeUndefined()
    expect(client.addLibraryItem).not.toHaveBeenCalled()
  })

  it('opens the synopsis from a result without adding and preserves the search on close', async () => {
    const client = {
      ...makeClient(),
      getDiscoverLists: vi.fn().mockResolvedValue([]),
      searchMetadata: vi.fn().mockResolvedValue([original]),
    }
    await act(async () => { renderer = create(<DiscoverScreen client={client as unknown as MonarrClient} onAdded={vi.fn()} />) })
    await act(async () => { renderer!.root.findByType('input' as never).props.onChangeText('Cunk') })
    await press('Search')
    const previewTarget = () => renderer!.root.findAll((node) => node.type === ('pressable' as never) && node.props.accessibilityLabel === 'View details for Cunk')[0]!
    expect(previewTarget().findAll((node) => node.type === 'text' && node.props.children === 'Read full synopsis')).toHaveLength(1)
    await act(async () => { previewTarget().props.onPress() })
    expect(button('Continue to add')).toBeDefined()
    expect(JSON.stringify(renderer!.toJSON())).toContain('Full synopsis')
    expect(client.addLibraryItem).not.toHaveBeenCalled()
    await press('Close')
    expect(renderer!.root.findAllByType('modal' as never)).toHaveLength(0)
    expect(renderer!.root.findByType('input' as never).props.value).toBe('Cunk')
    expect(previewTarget()).toBeDefined()
    expect(client.searchMetadata).toHaveBeenCalledOnce()
  })

})
