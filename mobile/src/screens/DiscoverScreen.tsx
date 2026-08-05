import { useEffect, useMemo, useState } from 'react'
import { ActivityIndicator, Modal, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import { compatibleRootFolders, resolveRootFolderID } from '../addMediaOptions'
import type { MonarrClient } from '../api'
import { SearchCard } from '../components/Media'
import { AppScreen, Button, Chip, Field, Header, IconButton, InlineError, LoadingState, MessageState, Panel, SectionTitle, Wordmark } from '../components/UI'
import { usePreferences } from '../preferences-context'
import { useTheme } from '../theme'
import type { AddMediaRequest, MediaKind, SearchResult } from '../types'
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
  const [listID, setListID] = useState('')
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult[] | null>(null)
  const [searching, setSearching] = useState(false)
  const [searchError, setSearchError] = useState('')
  const [adding, setAdding] = useState<SearchResult | null>(null)
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
      setSearchError(cause instanceof Error ? cause.message : 'Search failed.')
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

        <View style={styles.searchRow}>
          <Field
            style={styles.flex}
            placeholder={`Search ${kind === 'series' ? 'TV series' : `${kind}s`}`}
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
              onAdd={() => setAdding(item)}
            />
          ))}
        </View>
      </ScrollView>

      {adding ? (
        <AddSheet
          client={client}
          item={adding}
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

function AddSheet({
  client,
  item,
  onClose,
  onAdded,
}: {
  client: MonarrClient
  item: SearchResult
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
  const [rootID, setRootID] = useState(0)
  const [profileID, setProfileID] = useState(0)
  const [monitored, setMonitored] = useState(true)
  const [searchNow, setSearchNow] = useState(false)
  const [monitor, setMonitor] = useState<'all' | 'latest' | 'none'>('all')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    setRootID((current) => resolveRootFolderID(current, compatibleRoots))
  }, [compatibleRoots])

  const add = async () => {
    if (!rootID) {
      setError('Choose a compatible root folder before adding this title.')
      return
    }
    setSubmitting(true)
    setError('')
    const request: AddMediaRequest = {
      kind: item.kind,
      tmdbId: item.tmdbId || undefined,
      tvdbId: item.tvdbId,
      olid: item.olid,
      rootFolderId: rootID,
      qualityProfileId: profileID || undefined,
      monitored,
      searchNow,
      monitor: item.kind === 'series' ? monitor : undefined,
    }
    try {
      const added = await client.addLibraryItem(request)
      onAdded(added.id)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not add this title.')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal animationType="slide" presentationStyle="pageSheet" onRequestClose={onClose}>
      <AppScreen forceTopInset>
        <Header title={`Add ${item.kind === 'series' ? 'series' : item.kind}`} left={<IconButton label="Close" glyph="×" onPress={onClose} />} />
        <ScrollView contentContainerStyle={styles.sheetContent}>
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
          <View style={styles.wrap}>
            <Chip label="Server default" selected={profileID === 0} onPress={() => setProfileID(0)} />
            {(options.data?.profiles ?? []).map((profile) => (
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
          <Button label={submitting ? 'Adding…' : `Add ${item.title}`} disabled={submitting || options.loading || !rootID} onPress={() => void add()} />
        </ScrollView>
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
  sheetContent: { padding: 18, paddingBottom: 42 },
  addTitle: { fontSize: 25, lineHeight: 31, fontWeight: '800' },
  wrap: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  addOptions: { gap: 17, marginTop: 24, marginBottom: 16 },
  optionRow: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  optionLabel: { fontSize: 14, fontWeight: '700' },
  optionHint: { fontSize: 12, lineHeight: 17, marginTop: 2 },
})
