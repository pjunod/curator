import { useMemo } from 'react'
import { ScrollView, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { localDateKey } from '../format'
import { AppScreen, Badge, Header, IconButton, LoadingState, MessageState, Panel, SectionTitle } from '../components/UI'
import { useTheme } from '../theme'
import type { CalendarEntry } from '../types'
import { useResource } from '../useResource'

export function CalendarScreen({ client, onBack, onOpen }: { client: MonarrClient; onBack: () => void; onOpen: (id: number) => void }) {
  const theme = useTheme()
  const range = useMemo(() => {
    const start = new Date()
    start.setDate(start.getDate() - 7)
    const end = new Date()
    end.setDate(end.getDate() + 60)
    return { start: localDateKey(start), end: localDateKey(end) }
  }, [])
  const resource = useResource(() => client.getCalendar(range.start, range.end), [client, range.start, range.end])
  const groups = groupEntries(resource.data ?? [])

  return (
    <AppScreen>
      <Header title="Calendar" subtitle="7 days back · 60 days ahead" left={<IconButton label="Back" glyph="‹" onPress={onBack} />} />
      <ScrollView contentContainerStyle={styles.content}>
        {resource.loading ? <LoadingState label="Loading calendar…" /> : null}
        {resource.error ? <MessageState title="Calendar unavailable" message={resource.error} retry={() => void resource.refresh()} /> : null}
        {!resource.loading && !resource.error && groups.length === 0 ? <MessageState title="Nothing scheduled" message="Upcoming releases and episodes will appear here." /> : null}
        {groups.map(([date, entries]) => (
          <View key={date}>
            <SectionTitle>{formatDate(date)}</SectionTitle>
            <Panel style={styles.day}>
              {entries.map((entry, index) => (
                <View key={`${entry.mediaItemId}:${entry.detail}:${index}`} style={[styles.entry, index > 0 && { borderTopColor: theme.border, borderTopWidth: StyleSheet.hairlineWidth }]}>
                  <View style={styles.entryBody}>
                    <Text onPress={() => onOpen(entry.mediaItemId)} style={[styles.title, { color: theme.text }]}>{entry.title}</Text>
                    <Text style={[styles.detail, { color: theme.muted }]}>{entry.detail || entry.kind}</Text>
                  </View>
                  <Badge label={entry.hasFile ? 'On disk' : 'Missing'} tone={entry.hasFile ? 'ok' : 'warning'} />
                </View>
              ))}
            </Panel>
          </View>
        ))}
      </ScrollView>
    </AppScreen>
  )
}

function groupEntries(entries: CalendarEntry[]): [string, CalendarEntry[]][] {
  const groups = new Map<string, CalendarEntry[]>()
  for (const entry of entries) {
    const existing = groups.get(entry.date) ?? []
    existing.push(entry)
    groups.set(entry.date, existing)
  }
  return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
}

function formatDate(value: string): string {
  const date = new Date(`${value}T12:00:00`)
  const today = localDateKey(new Date())
  const label = new Intl.DateTimeFormat(undefined, { weekday: 'long', month: 'short', day: 'numeric' }).format(date)
  return value === today ? `Today · ${label}` : label
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42 },
  day: { paddingVertical: 3 },
  entry: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingVertical: 12 },
  entryBody: { flex: 1 },
  title: { fontSize: 14, lineHeight: 19, fontWeight: '700' },
  detail: { fontSize: 12, lineHeight: 17, marginTop: 2 },
})
