import { useCallback, useMemo, useState } from 'react'
import { Pressable, RefreshControl, SectionList, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import {
  addDays,
  calendarStatus,
  calendarSubtitle,
  calendarTime,
  groupCalendar,
  localDateKey,
} from '../format'
import { Poster } from '../components/Media'
import { AppScreen, Badge, Header, IconButton, LoadingState, MessageState, SectionTitle } from '../components/UI'
import { useTheme } from '../theme'
import type { CalendarEntry } from '../types'
import { useResource } from '../useResource'

const DAYS_BACK = 7
const DAYS_AHEAD = 60
const STEP_DAYS = 30

export function CalendarScreen({ client, onBack, onOpen }: { client: MonarrClient; onBack: () => void; onOpen: (id: number) => void }) {
  const theme = useTheme()
  // The horizon grows forward as the list is scrolled. Kept as a day count
  // rather than a date, so the window is derived and cannot drift.
  const [ahead, setAhead] = useState(DAYS_AHEAD)
  const range = useMemo(() => {
    const today = new Date()
    return { start: localDateKey(addDays(today, -DAYS_BACK)), end: localDateKey(addDays(today, ahead)) }
  }, [ahead])
  const resource = useResource(() => client.getCalendar(range.start, range.end), [client, range.start, range.end])
  const sections = useMemo(() => groupCalendar(resource.data ?? []), [resource.data])

  // An open-ended list with an invisible horizon reads as "the app lost my
  // show", so the header keeps stating how far it has loaded.
  const subtitle = `${DAYS_BACK} days back · ${ahead} ahead`
  const loadMore = useCallback(() => setAhead((current) => current + STEP_DAYS), [])

  return (
    <AppScreen>
      <Header title="Calendar" subtitle={subtitle} left={<IconButton label="Back" glyph="‹" onPress={onBack} />} />
      {resource.loading ? <LoadingState label="Loading calendar…" /> : null}
      {resource.error ? <MessageState title="Calendar unavailable" message={resource.error} retry={() => void resource.refresh()} /> : null}
      {!resource.loading && !resource.error ? (
        <SectionList
          sections={sections}
          keyExtractor={(entry, index) => `${entry.mediaItemId}:${entry.date}:${index}`}
          stickySectionHeadersEnabled
          contentContainerStyle={styles.content}
          onEndReached={loadMore}
          onEndReachedThreshold={0.4}
          refreshControl={<RefreshControl refreshing={resource.refreshing} onRefresh={() => void resource.refresh()} />}
          renderSectionHeader={({ section }) => (
            <View style={[styles.sectionHeader, { backgroundColor: theme.bg }]}>
              <SectionTitle>{section.title}</SectionTitle>
            </View>
          )}
          renderItem={({ item }) => <CalendarRow entry={item} onPress={() => onOpen(item.mediaItemId)} />}
          ListEmptyComponent={<MessageState title="Nothing scheduled" message="Upcoming releases and episodes will appear here." />}
        />
      ) : null}
    </AppScreen>
  )
}

function CalendarRow({ entry, onPress }: { entry: CalendarEntry; onPress: () => void }) {
  const theme = useTheme()
  const status = calendarStatus(entry)
  const time = calendarTime(entry.airDateUtc)
  // Unmonitored is a second axis: still news, just not being hunted, so the
  // row dims rather than changing what the badge says.
  const dimmed = entry.monitored === false
  return (
    <Pressable
      accessibilityRole="button"
      onPress={onPress}
      style={({ pressed }) => [
        styles.row,
        { backgroundColor: theme.raised, borderColor: theme.border, opacity: dimmed ? 0.55 : pressed ? 0.75 : 1 },
      ]}
    >
      <Poster path={entry.posterPath ?? ''} title={entry.title} width={48} />
      <View style={styles.rowBody}>
        <Text numberOfLines={1} style={[styles.title, { color: theme.text }]}>{entry.title}</Text>
        <Text numberOfLines={1} style={[styles.detail, { color: theme.muted }]}>
          {calendarSubtitle(entry) || entry.kind}
          {dimmed ? ' · not monitored' : ''}
        </Text>
      </View>
      <View style={styles.rail}>
        {time ? <Text style={[styles.time, { color: theme.text }]}>{time}</Text> : null}
        {entry.network ? <Text numberOfLines={1} style={[styles.network, { color: theme.muted }]}>{entry.network}</Text> : null}
        <Badge label={status.label} tone={status.tone} />
      </View>
    </Pressable>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42, gap: 8 },
  sectionHeader: { paddingTop: 8 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 12, padding: 10, borderWidth: StyleSheet.hairlineWidth, borderRadius: 12 },
  rowBody: { flex: 1 },
  rail: { alignItems: 'flex-end', gap: 4 },
  title: { fontSize: 14, lineHeight: 19, fontWeight: '700' },
  detail: { fontSize: 12, lineHeight: 17, marginTop: 2 },
  time: { fontSize: 13, fontWeight: '700' },
  network: { fontSize: 11, maxWidth: 110 },
})
