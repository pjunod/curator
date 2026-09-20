import { useEffect, useMemo, useRef, useState } from 'react'
import { ActivityIndicator, Keyboard, Modal, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import { useSafeAreaInsets } from 'react-native-safe-area-context'
import { compatibleRootFolders, resolveRootFolderID } from '../addMediaOptions'
import { ApiError } from '../api'
import type { MonarrClient } from '../api'
import { SearchCard } from '../components/Media'
import { PreviewDetails } from '../components/MetadataPreview'
import { previewState } from '../metadataPreview'
import { useMetadataPreview } from '../useMetadataPreview'
import { bookTypeForSource } from '../format'
import { AppScreen, Button, Chip, Field, Header, IconButton, InlineError, LoadingState, MessageState, Panel, SectionTitle, Wordmark } from '../components/UI'
import { usePreferences } from '../preferences-context'
import { useTheme } from '../theme'
import type { AddMediaRequest, BookType, MediaKind, SearchResult } from '../types'
import { useResource } from '../useResource'

export function DiscoverScreen({
  client,
  onAdded,
}: {
  client: MonarrClient
  onAdded: (id: number) => void
}) {
  const theme = useTheme()
  const { itemSize } = usePreferences()
  const [kind, setKind] = useState<MediaKind>('movie')
  const [bookType, setBookType] = useState<BookType>('ebook')
  const [listID, setListID] = useState('')
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult[] | null>(null)
  const [searching, setSearching] = useState(false)
  const [searchError, setSearchError] = useState('')
  const [adding, setAdding] = useState<{ item: SearchResult; step: 'preview' | 'options' } | null>(null)
  const lists = useResource(() => client.getDiscoverLists(), [client])
  const kindLists = useMemo(() => (lists.data ?? []).filter((list) => list.kind === kind), [kind, lists.data])

  useEffect(() => {
    const available = (lists.data ?? []).filter((list) => list.kind === kind)
    if (!available.some((list) => list.id === listID)) setListID(available[0]?.id ?? '')
    setResults(null)
    setSearchError('')
  }, [kind, listID, lists.data])

  const items = useResource(
    () => (listID ? client.getDiscoverItems(listID) : Promise.resolve([])),
    [client, listID],
  )

  const search = async () => {
    const clean = query.trim()
    if (clean.length < 2) {
      setSearchError('Enter at least two characters.')
      return
    }
    setSearching(true)
    setSearchError('')
    try {
      setResults(await client.searchMetadata(kind, clean))
    } catch (cause) {
      if (cause instanceof ApiError) {
        setSearchError({
          invalid_external_id: 'That external ID is invalid. Use tvdb:414217, imdb:tt16867040, or tmdb:550.',
          identity_conflict: 'More than one record claims that identity.',
          provider_unavailable: 'The metadata provider is unavailable. Try again later.',
          unsupported_hydration: 'That title was identified, but no configured provider can safely add it.',
        }[cause.code ?? ''] ?? cause.message)
      } else {
        setSearchError(cause instanceof Error ? cause.message : 'Search failed.')
      }
    } finally {
      setSearching(false)
    }
  }

  const visible = results ?? items.data ?? []
  const activeList = kindLists.find((list) => list.id === listID)

  return (
    <AppScreen>
      <Header title="Discover" left={<Wordmark size={19} />} subtitle="Find something worth adding" />
      <ScrollView contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
        <View style={styles.kinds}>
          {(['movie', 'series', 'book'] as const).map((value) => (
            <Chip key={value} label={value === 'series' ? 'TV' : `${value.charAt(0).toUpperCase()}${value.slice(1)}s`} selected={kind === value} onPress={() => setKind(value)} />
          ))}
        </View>
        {kind === 'book' ? (
          <View style={styles.kinds}>
            <Chip label="Ebooks" selected={bookType === 'ebook'} onPress={() => setBookType('ebook')} />
            <Chip label="Audiobooks" selected={bookType === 'audiobook'} onPress={() => setBookType('audiobook')} />
          </View>
        ) : null}

        <View style={styles.searchRow}>
          <Field
            style={styles.flex}
            placeholder={kind === 'series' ? 'Title, tvdb:414217, or IMDb ID' : kind === 'movie' ? 'Title, IMDb ID, or tmdb:550' : 'Search books'}
            returnKeyType="search"
            value={query}
            onChangeText={(value) => {
              setQuery(value)
              if (!value.trim()) setResults(null)
            }}
            onSubmitEditing={() => void search()}
          />
          <Button compact label={searching ? '…' : 'Search'} disabled={searching} onPress={() => void search()} />
        </View>
        <InlineError message={searchError} />

        {results ? (
          <SectionTitle action={<Button compact secondary label="Clear" onPress={() => { setResults(null); setQuery('') }} />}>Search results</SectionTitle>
        ) : (
          <>
            <SectionTitle>Browse</SectionTitle>
            {lists.loading ? <ActivityIndicator color={theme.accent} /> : null}
            <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.listChips}>
              {kindLists.map((list) => <Chip key={list.id} label={list.title} selected={listID === list.id} onPress={() => setListID(list.id)} />)}
            </ScrollView>
            {activeList ? <Text style={[styles.blurb, { color: theme.muted }]}>{activeList.blurb} · {activeList.source}</Text> : null}
          </>
        )}

        {results && results.length === 0 ? <MessageState title="No results" message="Try another title or author." /> : null}
        {!results && items.loading ? <LoadingState label="Loading recommendations…" /> : null}
        {!results && items.error ? <MessageState title="Discover unavailable" message={items.error} retry={() => void items.refresh()} /> : null}
        <View style={styles.results}>
          {visible.map((item) => (
            <SearchCard
              key={`${item.kind}:${item.tmdbId || item.tvdbId || item.olid}`}
              item={item}
              itemSize={itemSize}
              bookType={item.kind === 'book' ? bookType : undefined}
              onAdd={() => { Keyboard.dismiss(); setAdding({ item, step: 'options' }) }}
              onPreview={() => { Keyboard.dismiss(); setAdding({ item, step: 'preview' }) }}
            />
          ))}
        </View>
      </ScrollView>

      {adding ? (
        <AddSheet
          client={client}
          item={adding.item}
          initialStep={adding.step}
          initialBookType={bookType}
          onClose={() => setAdding(null)}
          onAdded={(id) => {
            setAdding(null)
            onAdded(id)
          }}
        />
      ) : null}
    </AppScreen>
  )
}

export function AddSheet({
  client,
  item,
  initialBookType,
  initialStep,
  onClose,
  onAdded,
}: {
  client: MonarrClient
  item: SearchResult
  initialBookType: BookType
  initialStep: 'preview' | 'options'
  onClose: () => void
  onAdded: (id: number) => void
}) {
  const theme = useTheme()
  const options = useResource(async () => {
    const [roots, profiles] = await Promise.all([client.getRootFolders(), client.getProfiles()])
    return { roots, profiles }
  }, [client, item.kind])
  const compatibleRoots = useMemo(
    () => compatibleRootFolders(options.data?.roots ?? [], item.kind),
    [item.kind, options.data?.roots],
  )
  const insets = useSafeAreaInsets()
  const [step, setStep] = useState(initialStep)
  const preview = useMetadataPreview(client, item)
  const submittingRef = useRef(false)
  const [rootID, setRootID] = useState(0)
  const [profileID, setProfileID] = useState(0)
  const [bookType, setBookType] = useState<BookType>(initialBookType)
  const [monitored, setMonitored] = useState(true)
  const [searchNow, setSearchNow] = useState(false)
  const [monitor, setMonitor] = useState<'all' | 'latest' | 'none'>('all')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const state = previewState(item, bookType, preview.data, preview.conflict)
  state.blocked ||= !!preview.unsupported
  const back = () => {
    if (step === 'options' && initialStep === 'preview') setStep('preview')
    else onClose()
  }

  useEffect(() => {
    setRootID((current) => resolveRootFolderID(current, compatibleRoots))
  }, [compatibleRoots])

  const add = async () => {
    if (submittingRef.current || state.blocked || state.owned) return
    if (!rootID) {
      setError('Choose a compatible root folder before adding this title.')
      return
    }
    submittingRef.current = true
    setSubmitting(true)
    setError('')
    const request: AddMediaRequest = {
      kind: item.kind,
      tmdbId: item.tmdbId || undefined,
      tvdbId: item.tvdbId,
      imdbId: item.imdbId,
      hydrationSource: item.hydrationSource,
      olid: item.olid,
      bookType: item.kind === 'book' ? bookType : undefined,
      rootFolderId: rootID,
      qualityProfileId: profileID || undefined,
      monitored,
      searchNow: monitored && searchNow,
      monitor: item.kind === 'series' ? monitor : undefined,
    }
    try {
      const added = await client.addLibraryItem(request)
      onAdded(added.id)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not add this title.')
    } finally {
      submittingRef.current = false
      setSubmitting(false)
    }
  }

  return (
    <Modal animationType="slide" presentationStyle="fullScreen" onRequestClose={back}>
      <AppScreen forceTopInset>
        <Header title={step === 'preview' ? 'Title details' : `Add ${item.kind === 'book' ? bookType : item.kind === 'series' ? 'series' : item.kind}`} left={<IconButton label={step === 'options' && initialStep === 'preview' ? 'Back to details' : 'Close'} glyph={step === 'options' && initialStep === 'preview' ? '‹' : '×'} onPress={back} />} />
        <ScrollView key={step} contentContainerStyle={styles.sheetContent}>
          {step === 'preview' ? <>
            <PreviewDetails item={item} data={preview.data} loading={preview.loading} error={preview.error} conflict={preview.conflict} unsupported={preview.unsupported} retry={() => void preview.refresh()} />
          </> : <>
          <Text style={[styles.addTitle, { color: theme.text }]}>{item.title}</Text>
          <Text style={[styles.blurb, { color: theme.muted }]}>{item.author || item.year}</Text>

          {options.loading ? <LoadingState label="Loading server defaults…" /> : null}
          {options.error ? <InlineError message={options.error} /> : null}

          <SectionTitle>Root folder</SectionTitle>
          <View style={styles.wrap}>
            {compatibleRoots.map((root) => (
              <Chip key={root.id} label={root.path.split('/').filter(Boolean).pop() ?? root.path} selected={rootID === root.id} onPress={() => setRootID(root.id)} />
            ))}
          </View>
          {!options.loading && options.data && compatibleRoots.length === 0 ? (
            <InlineError message={`No root folder is configured for ${item.kind === 'series' ? 'TV' : `${item.kind}s`}. Add one in the web settings first.`} />
          ) : null}

          <SectionTitle>Quality profile</SectionTitle>
          {item.kind === 'book' ? (
            <View style={styles.wrap}>
              <Chip label={`Ebook${(item.bookTypes ?? []).includes('ebook') ? ' · owned' : ''}`} selected={bookType === 'ebook'} onPress={() => { setBookType('ebook'); setProfileID(0) }} />
              <Chip label={`Audiobook${(item.bookTypes ?? []).includes('audiobook') ? ' · owned' : ''}`} selected={bookType === 'audiobook'} onPress={() => { setBookType('audiobook'); setProfileID(0) }} />
            </View>
          ) : null}
          <View style={styles.wrap}>
            <Chip label="Server default" selected={profileID === 0} onPress={() => setProfileID(0)} />
            {(options.data?.profiles ?? []).filter((profile) => {
              const profileBookType = bookTypeForSource(profile.target.source)
              return item.kind === 'book' ? profileBookType === bookType : profileBookType === undefined
            }).map((profile) => (
              <Chip key={profile.id} label={profile.name} selected={profileID === profile.id} onPress={() => setProfileID(profile.id)} />
            ))}
          </View>

          {item.kind === 'series' ? (
            <>
              <SectionTitle>Monitor seasons</SectionTitle>
              <View style={styles.wrap}>
                {(['all', 'latest', 'none'] as const).map((value) => <Chip key={value} label={value} selected={monitor === value} onPress={() => setMonitor(value)} />)}
              </View>
            </>
          ) : null}

          <Panel style={styles.addOptions}>
            <OptionSwitch label="Monitored" hint="Keep searching until the quality target is met." value={monitored} onChange={setMonitored} />
            <OptionSwitch label="Search now" hint="Start an automatic search as soon as this is added." value={searchNow} onChange={setSearchNow} />
          </Panel>
          <InlineError message={error} />
          </>}

        </ScrollView>
        <View style={[styles.sheetFooter, { borderColor: theme.border, paddingBottom: Math.max(16, insets.bottom) }]}>
          {state.blocked && step === 'options' ? <InlineError message={preview.unsupported || preview.data?.addBlockReason || 'Resolve the identity conflict before adding.'} /> : null}
          {state.owned ? <Text style={{ color: theme.muted }}>Already in your library{item.kind === 'book' ? ` · ${bookType}` : ''}</Text> : step === 'preview' ? <Button label="Continue to add" disabled={state.blocked} onPress={() => setStep('options')} /> : <Button label={submitting ? 'Adding…' : `Add ${item.title}`} disabled={submitting || options.loading || !rootID || state.blocked} onPress={() => void add()} />}
          {state.libraryItemId ? <Button secondary label="Open in library" onPress={() => onAdded(state.libraryItemId!)} /> : null}
        </View>
      </AppScreen>
    </Modal>
  )
}

function OptionSwitch({ label, hint, value, onChange }: { label: string; hint: string; value: boolean; onChange: (value: boolean) => void }) {
  const theme = useTheme()
  return (
    <View style={styles.optionRow}>
      <View style={styles.flex}>
        <Text style={[styles.optionLabel, { color: theme.text }]}>{label}</Text>
        <Text style={[styles.optionHint, { color: theme.muted }]}>{hint}</Text>
      </View>
      <Switch value={value} onValueChange={onChange} trackColor={{ true: theme.accent }} />
    </View>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 36 },
  kinds: { flexDirection: 'row', gap: 7, marginBottom: 13 },
  searchRow: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  flex: { flex: 1 },
  listChips: { gap: 7, paddingRight: 16 },
  blurb: { fontSize: 12, lineHeight: 18, marginTop: 8 },
  results: { gap: 10, marginTop: 12 },
  sheetFooter: { padding: 16, borderTopWidth: 1, gap: 10 },
  sheetContent: { padding: 18, paddingBottom: 42 },
  addTitle: { fontSize: 25, lineHeight: 31, fontWeight: '800' },
  wrap: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  addOptions: { gap: 17, marginTop: 24, marginBottom: 16 },
  optionRow: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  optionLabel: { fontSize: 14, fontWeight: '700' },
  optionHint: { fontSize: 12, lineHeight: 17, marginTop: 2 },
})
