import { Image, Pressable, StyleSheet, Text, View } from 'react-native'
import { completeness, posterUrl } from '../format'
import type { ItemSize } from '../preferences'
import { useTheme } from '../theme'
import type { MediaItemSummary, SearchResult } from '../types'
import { Badge, Button } from './UI'

export function Poster({ path, title, width = 150 }: { path: string; title: string; width?: number }) {
  const theme = useTheme()
  const height = Math.round(width * 1.5)
  const uri = posterUrl(path)
  if (!uri) {
    return (
      <View style={[styles.posterFallback, { width, height, backgroundColor: theme.hover, borderColor: theme.border }]}>
        <Text numberOfLines={3} style={[styles.posterFallbackText, { color: theme.muted }]}>{title}</Text>
      </View>
    )
  }
  return <Image accessibilityLabel={`${title} poster`} source={{ uri }} style={{ width, height, borderRadius: 12, backgroundColor: theme.hover }} />
}

export function LibraryCard({ item, width, itemSize, onPress }: { item: MediaItemSummary; width: number; itemSize: ItemSize; onPress: () => void }) {
  const theme = useTheme()
  const state = completeness(item)
  const textSize = itemSize === 'small' ? 12 : itemSize === 'large' ? 16 : 14
  return (
    <Pressable accessibilityRole="button" onPress={onPress} style={({ pressed }) => [{ width, opacity: pressed ? 0.75 : 1 }, styles.libraryCard]}>
      <Poster path={item.posterPath} title={item.title} width={width} />
      <Text numberOfLines={2} style={[styles.cardTitle, { color: theme.text, fontSize: textSize, lineHeight: textSize + 4 }]}>{item.title}</Text>
      <Text numberOfLines={1} style={[styles.cardMeta, { color: theme.muted, fontSize: Math.max(10, textSize - 2) }]}>
        {item.kind === 'book'
          ? [item.author, item.bookType === 'audiobook' ? 'Audiobook' : 'Ebook'].filter(Boolean).join(' · ')
          : item.year || item.kind} {!item.monitored ? ' · unmonitored' : ''}
      </Text>
      <Badge label={state.label} tone={state.tone} />
    </Pressable>
  )
}

export function SearchCard({ item, itemSize, onAdd, adding = false }: { item: SearchResult; itemSize: ItemSize; onAdd: () => void; adding?: boolean }) {
  const theme = useTheme()
  const sizing = itemSize === 'small'
    ? { poster: 64, padding: 8, gap: 10, title: 14, overviewLines: 2 }
    : itemSize === 'large'
      ? { poster: 108, padding: 12, gap: 14, title: 18, overviewLines: 4 }
      : { poster: 82, padding: 10, gap: 12, title: 16, overviewLines: 3 }
  return (
    <View style={[styles.searchCard, { backgroundColor: theme.raised, borderColor: theme.border, padding: sizing.padding, gap: sizing.gap }]}>
      <Poster path={item.posterPath} title={item.title} width={sizing.poster} />
      <View style={styles.searchBody}>
        <Text numberOfLines={2} style={[styles.searchTitle, { color: theme.text, fontSize: sizing.title, lineHeight: sizing.title + 4 }]}>{item.title}</Text>
        <Text numberOfLines={1} style={[styles.cardMeta, { color: theme.muted }]}>{item.author || item.year || item.kind}</Text>
        <Text numberOfLines={sizing.overviewLines} style={[styles.overview, { color: theme.muted }]}>{item.overview || 'No overview available.'}</Text>
        {item.inLibrary ? <Badge label="In library" tone="ok" /> : <Button compact label={adding ? 'Adding…' : 'Add'} disabled={adding} onPress={onAdd} />}
      </View>
    </View>
  )
}

const styles = StyleSheet.create({
  posterFallback: { borderWidth: 1, borderRadius: 12, alignItems: 'center', justifyContent: 'center', padding: 10 },
  posterFallbackText: { fontSize: 13, textAlign: 'center', fontWeight: '600' },
  libraryCard: { marginBottom: 20, gap: 5 },
  cardTitle: { fontWeight: '700', marginTop: 2 },
  cardMeta: { fontSize: 12 },
  searchCard: { flexDirection: 'row', borderWidth: 1, borderRadius: 16 },
  searchBody: { flex: 1, alignItems: 'flex-start', gap: 5 },
  searchTitle: { fontWeight: '700' },
  overview: { fontSize: 12, lineHeight: 17 },
})
