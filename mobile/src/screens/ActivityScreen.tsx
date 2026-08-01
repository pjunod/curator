import { useState } from 'react'
import { RefreshControl, ScrollView, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { formatBytes, formatRelative } from '../format'
import { AppScreen, Badge, Chip, Header, InlineError, LoadingState, MessageState, Panel, Wordmark } from '../components/UI'
import { useTheme } from '../theme'
import type { QueueItem } from '../types'
import { useResource } from '../useResource'

type ActivityFilter = 'active' | 'imported' | 'failed'

export function ActivityScreen({ client, onOpen }: { client: MonarrClient; onOpen: (id: number) => void }) {
  const theme = useTheme()
  const [filter, setFilter] = useState<ActivityFilter>('active')
  const resource = useResource(async () => {
    const [summary, items] = await Promise.all([client.getQueueSummary(), client.getQueue(filter)])
    return { summary, items }
  }, [client, filter])

  return (
    <AppScreen>
      <Header title="Activity" subtitle={`${resource.data?.summary.active ?? 0} active`} left={<Wordmark size={19} />} />
      <ScrollView
        contentContainerStyle={styles.content}
        refreshControl={<RefreshControl refreshing={resource.refreshing} tintColor={theme.accent} onRefresh={() => void resource.refresh()} />}
      >
        <View style={styles.summary}>
          <Stat label="Active" value={resource.data?.summary.active ?? 0} />
          <Stat label="Total" value={resource.data?.summary.total ?? 0} />
          <Stat label="Retention" value={`${resource.data?.summary.retentionDays ?? '—'}d`} />
        </View>
        <View style={styles.filters}>
          {(['active', 'imported', 'failed'] as const).map((value) => <Chip key={value} label={value.charAt(0).toUpperCase() + value.slice(1)} selected={filter === value} onPress={() => setFilter(value)} />)}
        </View>
        <InlineError message={resource.error} />
        {resource.loading && !resource.data ? <LoadingState label="Loading activity…" /> : null}
        {!resource.loading && !resource.error && resource.data?.items.length === 0 ? <MessageState title={`No ${filter} activity`} message={filter === 'active' ? 'Downloads and imports in progress will appear here.' : `No retained ${filter} rows.`} /> : null}
        <View style={styles.items}>
          {(resource.data?.items ?? []).map((item) => <ActivityRow key={item.id} item={item} onOpen={() => onOpen(item.mediaItemId)} />)}
        </View>
      </ScrollView>
    </AppScreen>
  )
}

function Stat({ label, value }: { label: string; value: string | number }) {
  const theme = useTheme()
  return (
    <View style={styles.stat}>
      <Text style={[styles.statValue, { color: theme.text }]}>{value}</Text>
      <Text style={[styles.statLabel, { color: theme.muted }]}>{label}</Text>
    </View>
  )
}

function ActivityRow({ item, onOpen }: { item: QueueItem; onOpen: () => void }) {
  const theme = useTheme()
  const fraction = item.total ? (item.bytes ?? 0) / item.total : item.progress || 0
  const progress = Math.max(0, Math.min(100, fraction * 100))
  const tone = item.error || item.state === 'failed' ? 'error' : item.state === 'imported' ? 'ok' : 'neutral'
  return (
    <Panel style={styles.item}>
      <View style={styles.itemHead}>
        <Text numberOfLines={2} onPress={onOpen} style={[styles.itemTitle, { color: theme.text }]}>{item.title}</Text>
        <Badge label={item.stage || item.state} tone={tone} />
      </View>
      <Text style={[styles.itemMeta, { color: theme.muted }]}>{[item.quality, item.protocol, formatRelative(item.addedAt)].filter(Boolean).join(' · ')}</Text>
      {item.stageDetail ? <Text style={[styles.itemMeta, { color: theme.muted }]}>{item.stageDetail}</Text> : null}
      {item.total ? <Text style={[styles.itemMeta, { color: theme.muted }]}>{formatBytes(item.bytes ?? 0)} / {formatBytes(item.total)}{item.bytesPerSecond ? ` · ${formatBytes(item.bytesPerSecond)}/s` : ''}</Text> : null}
      {filterNeedsProgress(item) ? (
        <View style={[styles.track, { backgroundColor: theme.hover }]}>
          <View style={[styles.progress, { backgroundColor: theme.accent, width: `${progress}%` }]} />
        </View>
      ) : null}
      {item.error ? <Text style={[styles.itemError, { color: theme.error }]}>{item.error}</Text> : null}
    </Panel>
  )
}

function filterNeedsProgress(item: QueueItem) {
  return item.state !== 'imported' && item.state !== 'failed' && item.progress > 0
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 36 },
  summary: { flexDirection: 'row', marginBottom: 16 },
  stat: { flex: 1, alignItems: 'center' },
  statValue: { fontSize: 22, fontWeight: '800' },
  statLabel: { fontSize: 11, marginTop: 2 },
  filters: { flexDirection: 'row', gap: 7, marginBottom: 14 },
  items: { gap: 9 },
  item: { gap: 7 },
  itemHead: { flexDirection: 'row', gap: 10, justifyContent: 'space-between' },
  itemTitle: { flex: 1, fontSize: 14, lineHeight: 19, fontWeight: '700' },
  itemMeta: { fontSize: 12, lineHeight: 17 },
  itemError: { fontSize: 12, lineHeight: 17 },
  track: { height: 5, borderRadius: 3, overflow: 'hidden' },
  progress: { height: 5, borderRadius: 3 },
})
