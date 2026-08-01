import { useMemo, useState } from 'react'
import { FlatList, StyleSheet, Text, useWindowDimensions, View } from 'react-native'
import type { MonarrClient } from '../api'
import { LibraryCard } from '../components/Media'
import { AppScreen, Chip, Field, Header, LoadingState, MessageState, Wordmark } from '../components/UI'
import { libraryGrid } from '../preferences'
import { usePreferences } from '../preferences-context'
import { useTheme } from '../theme'
import type { MediaItemSummary, MediaKind } from '../types'
import { useResource } from '../useResource'

type KindFilter = 'all' | MediaKind

export function LibraryScreen({
  client,
  refreshKey,
  onOpen,
}: {
  client: MonarrClient
  refreshKey: number
  onOpen: (id: number) => void
}) {
  const theme = useTheme()
  const { itemSize } = usePreferences()
  const { width } = useWindowDimensions()
  const [kind, setKind] = useState<KindFilter>('all')
  const [query, setQuery] = useState('')
  const resource = useResource(
    () => client.getLibrary(kind === 'all' ? undefined : kind),
    [client, kind, refreshKey],
  )

  const items = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase()
    if (!needle) return resource.data ?? []
    return (resource.data ?? []).filter((item) =>
      `${item.title} ${item.author} ${item.year}`.toLocaleLowerCase().includes(needle),
    )
  }, [query, resource.data])
  const { columns, slotWidth, cardWidth } = libraryGrid(width, itemSize)

  return (
    <AppScreen>
      <Header title="Library" left={<Wordmark size={19} />} subtitle={`${resource.data?.length ?? 0} titles`} />
      <FlatList<MediaItemSummary>
        key={`${itemSize}-${columns}-column-library`}
        data={items}
        keyExtractor={(item) => String(item.id)}
        numColumns={columns}
        columnWrapperStyle={columns > 1 ? styles.columns : undefined}
        contentContainerStyle={styles.content}
        refreshing={resource.refreshing}
        onRefresh={() => void resource.refresh()}
        keyboardShouldPersistTaps="handled"
        ListHeaderComponent={
          <View style={styles.controls}>
            <Field placeholder="Search titles and authors" value={query} onChangeText={setQuery} />
            <View style={styles.chips}>
              {(['all', 'movie', 'series', 'book'] as const).map((value) => (
                <Chip key={value} label={value === 'all' ? 'All' : value === 'series' ? 'TV' : `${value[0]?.toUpperCase()}${value.slice(1)}s`} selected={kind === value} onPress={() => setKind(value)} />
              ))}
            </View>
          </View>
        }
        ListEmptyComponent={
          resource.loading ? (
            <LoadingState label="Loading your library…" />
          ) : resource.error ? (
            <MessageState title="Library unavailable" message={resource.error} retry={() => void resource.refresh()} />
          ) : (
            <MessageState
              title={query ? 'No matches' : 'Your library is empty'}
              message={query ? 'Try a different title, author, or year.' : 'Use Discover to find your first movie, series, or book.'}
            />
          )
        }
        renderItem={({ item }) => (
          <View style={[styles.cardSlot, { width: columns > 1 ? slotWidth : '100%' }]}>
            <LibraryCard item={item} width={cardWidth} itemSize={itemSize} onPress={() => onOpen(item.id)} />
          </View>
        )}
      />
      {resource.error && resource.data ? <Text style={[styles.staleError, { color: theme.error }]}>{resource.error}</Text> : null}
    </AppScreen>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 32 },
  columns: { gap: 12 },
  cardSlot: { alignItems: 'center' },
  controls: { gap: 12, marginBottom: 18 },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  staleError: { fontSize: 12, textAlign: 'center', padding: 6 },
})
