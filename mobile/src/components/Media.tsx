import { Image, Pressable, StyleSheet, Text, View } from 'react-native'
import { completeness, posterUrl } from '../format'
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

export function LibraryCard({ item, width, onPress }: { item: MediaItemSummary; width: number; onPress: () => void }) {
  const theme = useTheme()
  const state = completeness(item)
  return (
    <Pressable accessibilityRole="button" onPress={onPress} style={({ pressed }) => [{ width, opacity: pressed ? 0.75 : 1 }, styles.libraryCard]}>
      <Poster path={item.posterPath} title={item.title} width={width} />
      <Text numberOfLines={2} style={[styles.cardTitle, { color: theme.text }]}>{item.title}</Text>
      <Text numberOfLines={1} style={[styles.cardMeta, { color: theme.muted }]}>
        {item.author || item.year || item.kind} {!item.monitored ? ' · unmonitored' : ''}
      </Text>
      <Badge label={state.label} tone={state.tone} />
    </Pressable>
  )
}

export function SearchCard({ item, onAdd, adding = false }: { item: SearchResult; onAdd: () => void; adding?: boolean }) {
  const theme = useTheme()
  return (
    <View style={[styles.searchCard, { backgroundColor: theme.raised, borderColor: theme.border }]}>
      <Poster path={item.posterPath} title={item.title} width={82} />
      <View style={styles.searchBody}>
        <Text numberOfLines={2} style={[styles.searchTitle, { color: theme.text }]}>{item.title}</Text>
        <Text numberOfLines={1} style={[styles.cardMeta, { color: theme.muted }]}>{item.author || item.year || item.kind}</Text>
        <Text numberOfLines={3} style={[styles.overview, { color: theme.muted }]}>{item.overview || 'No overview available.'}</Text>
        {item.inLibrary ? <Badge label="In library" tone="ok" /> : <Button compact label={adding ? 'Adding…' : 'Add'} disabled={adding} onPress={onAdd} />}
      </View>
    </View>
  )
}

const styles = StyleSheet.create({
  posterFallback: { borderWidth: 1, borderRadius: 12, alignItems: 'center', justifyContent: 'center', padding: 10 },
  posterFallbackText: { fontSize: 13, textAlign: 'center', fontWeight: '600' },
  libraryCard: { marginBottom: 20, gap: 5 },
  cardTitle: { fontSize: 14, lineHeight: 18, fontWeight: '700', marginTop: 2 },
  cardMeta: { fontSize: 12 },
  searchCard: { flexDirection: 'row', borderWidth: 1, borderRadius: 16, padding: 10, gap: 12 },
  searchBody: { flex: 1, alignItems: 'flex-start', gap: 5 },
  searchTitle: { fontSize: 16, fontWeight: '700', lineHeight: 20 },
  overview: { fontSize: 12, lineHeight: 17 },
})
