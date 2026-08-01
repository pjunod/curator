import { useState } from 'react'
import { FlatList, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { AppScreen, Badge, Button, Header, InlineError, LoadingState, MessageState, Panel, Wordmark } from '../components/UI'
import { useTheme } from '../theme'
import type { WantedItem } from '../types'
import { useResource } from '../useResource'

export function WantedScreen({ client, onOpen }: { client: MonarrClient; onOpen: (id: number) => void }) {
  const theme = useTheme()
  const resource = useResource(() => client.getWanted(), [client])
  const [searchingAll, setSearchingAll] = useState(false)
  const [searching, setSearching] = useState<Set<number>>(new Set())
  const [error, setError] = useState('')

  const searchAll = async () => {
    setSearchingAll(true)
    setError('')
    try {
      await client.runTask('backlog.search')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not start the backlog search.')
    } finally {
      setSearchingAll(false)
    }
  }

  const searchOne = async (id: number) => {
    setSearching((current) => new Set(current).add(id))
    setError('')
    try {
      await client.autoSearchItem(id)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not start the search.')
    } finally {
      setSearching((current) => {
        const next = new Set(current)
        next.delete(id)
        return next
      })
    }
  }

  return (
    <AppScreen>
      <Header title="Wanted" subtitle={`${resource.data?.length ?? 0} targets`} left={<Wordmark size={19} />} />
      <FlatList<WantedItem>
        data={resource.data ?? []}
        keyExtractor={(item) => item.wantableId}
        contentContainerStyle={styles.content}
        refreshing={resource.refreshing}
        onRefresh={() => void resource.refresh()}
        ListHeaderComponent={
          <View style={styles.intro}>
            <Text style={[styles.explainer, { color: theme.muted }]}>Everything monitored that is missing or below its quality cutoff.</Text>
            <Button label={searchingAll ? 'Starting…' : 'Search all now'} disabled={searchingAll} onPress={() => void searchAll()} />
            <InlineError message={error || resource.error} />
          </View>
        }
        ListEmptyComponent={
          resource.loading ? <LoadingState /> : resource.error ? <MessageState title="Wanted unavailable" message={resource.error} retry={() => void resource.refresh()} /> : <MessageState title="Nothing wanted" message="Everything monitored is on disk at or above its cutoff." />
        }
        renderItem={({ item }) => (
          <Panel style={styles.row}>
              <View style={styles.rowHead}>
                <View style={styles.rowTitleBlock}>
                  <Text accessibilityRole="link" numberOfLines={2} onPress={() => onOpen(item.mediaItemId)} style={[styles.title, { color: theme.text }]}>{item.title}</Text>
                  <Text style={[styles.detail, { color: theme.muted }]}>{item.detail || item.wantableId}</Text>
                </View>
                <Badge label={item.missing ? 'Missing' : `Upgrade ${item.current}`} tone="warning" />
              </View>
              {item.copy ? <Badge label={item.copy} /> : null}
              <Button compact secondary label={searching.has(item.mediaItemId) ? 'Searching…' : 'Search now'} disabled={searching.has(item.mediaItemId)} onPress={() => void searchOne(item.mediaItemId)} />
          </Panel>
        )}
      />
    </AppScreen>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 36, gap: 9 },
  intro: { gap: 12, marginBottom: 16 },
  explainer: { fontSize: 13, lineHeight: 19 },
  row: { gap: 10, marginBottom: 9 },
  rowHead: { flexDirection: 'row', alignItems: 'flex-start', gap: 10 },
  rowTitleBlock: { flex: 1 },
  title: { fontSize: 15, lineHeight: 20, fontWeight: '700' },
  detail: { fontSize: 12, lineHeight: 17, marginTop: 2 },
})
