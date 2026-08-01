import { useState } from 'react'
import { ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { formatBytes } from '../format'
import { Poster } from '../components/Media'
import { AppScreen, Badge, Button, Header, IconButton, InlineError, LoadingState, MessageState, Panel, SectionTitle } from '../components/UI'
import { useTheme } from '../theme'
import { useResource } from '../useResource'

export function MediaDetailScreen({ client, id, onBack }: { client: MonarrClient; id: number; onBack: () => void }) {
  const theme = useTheme()
  const resource = useResource(() => client.getLibraryItem(id), [client, id])
  const [actionError, setActionError] = useState('')
  const [saving, setSaving] = useState(false)
  const [searching, setSearching] = useState(false)
  const item = resource.data

  const setMonitored = async (monitored: boolean) => {
    setSaving(true)
    setActionError('')
    try {
      await client.updateLibraryItem(id, { monitored })
      await resource.refresh()
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not update monitoring.')
    } finally {
      setSaving(false)
    }
  }

  const searchNow = async () => {
    setSearching(true)
    setActionError('')
    try {
      await client.autoSearchItem(id)
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not start a search.')
    } finally {
      setSearching(false)
    }
  }

  if (resource.loading && !item) {
    return <AppScreen><Header title="Details" left={<IconButton label="Back" glyph="‹" onPress={onBack} />} /><LoadingState /></AppScreen>
  }
  if (!item) {
    return <AppScreen><Header title="Details" left={<IconButton label="Back" glyph="‹" onPress={onBack} />} /><MessageState title="Title unavailable" message={resource.error} retry={() => void resource.refresh()} /></AppScreen>
  }

  const episodes = item.seasons.flatMap((season) => season.episodes)
  const aired = episodes.filter((episode) => !episode.airDate || new Date(episode.airDate) <= new Date())
  const onDisk = aired.filter((episode) => episode.hasFile).length

  return (
    <AppScreen>
      <Header title={item.title} subtitle={item.kind === 'series' ? 'TV series' : item.kind} left={<IconButton label="Back" glyph="‹" onPress={onBack} />} />
      <ScrollView contentContainerStyle={styles.content}>
        <View style={styles.hero}>
          <Poster path={item.posterPath} title={item.title} width={126} />
          <View style={styles.heroBody}>
            <Text style={[styles.title, { color: theme.text }]}>{item.title}</Text>
            <Text style={[styles.meta, { color: theme.muted }]}>{[item.author, item.year, item.status, item.runtime ? `${item.runtime} min` : ''].filter(Boolean).join(' · ')}</Text>
            <View style={styles.badges}>
              <Badge label={item.monitored ? 'Monitored' : 'Unmonitored'} tone={item.monitored ? 'ok' : 'neutral'} />
              {item.quality ? <Badge label={item.quality} tone={item.qualityVerified ? 'ok' : 'neutral'} /> : null}
              {item.upgrade ? <Badge label={item.upgrade} tone={item.upgrade === 'met' ? 'ok' : 'warning'} /> : null}
            </View>
          </View>
        </View>

        {item.overview ? <Text style={[styles.overview, { color: theme.text }]}>{item.overview}</Text> : null}
        <InlineError message={actionError || resource.error} />

        <Panel style={styles.actions}>
          <View style={styles.monitorRow}>
            <View style={styles.flex}>
              <Text style={[styles.actionTitle, { color: theme.text }]}>Monitor</Text>
              <Text style={[styles.actionHint, { color: theme.muted }]}>Let automation keep this title complete.</Text>
            </View>
            <Switch disabled={saving} value={item.monitored} onValueChange={(value) => void setMonitored(value)} trackColor={{ true: theme.accent }} />
          </View>
          <Button label={searching ? 'Starting search…' : 'Search now'} disabled={searching} onPress={() => void searchNow()} />
        </Panel>

        {item.kind === 'series' ? (
          <>
            <SectionTitle>Episodes</SectionTitle>
            <Panel>
              <Text style={[styles.stat, { color: theme.text }]}>{onDisk} of {aired.length} aired episodes on disk</Text>
              {item.seasons.map((season) => {
                const present = season.episodes.filter((episode) => episode.hasFile).length
                return (
                  <View key={season.number} style={[styles.row, { borderTopColor: theme.border }]}>
                    <Text style={[styles.rowTitle, { color: theme.text }]}>Season {season.number}</Text>
                    <Text style={[styles.meta, { color: theme.muted }]}>{present}/{season.episodes.length} · {season.monitored ? 'monitored' : 'off'}</Text>
                  </View>
                )
              })}
            </Panel>
          </>
        ) : null}

        <SectionTitle>Files</SectionTitle>
        {item.files.length === 0 ? (
          <Panel><Text style={[styles.meta, { color: theme.muted }]}>Nothing on disk yet.</Text></Panel>
        ) : item.files.map((file) => (
          <Panel key={file.id} style={styles.file}>
            <View style={styles.rowBetween}>
              <Text numberOfLines={1} style={[styles.fileName, { color: theme.text }]}>{file.path.split('/').pop()}</Text>
              <Text style={[styles.meta, { color: theme.muted }]}>{formatBytes(file.size)}</Text>
            </View>
            <Text style={[styles.meta, { color: theme.muted }]}>{file.facts || file.quality || 'Quality unknown'}</Text>
            {file.implausible ? <Badge label="Implausible metadata" tone="error" /> : file.provenanceLabel ? <Badge label={file.provenanceLabel} tone={file.verified ? 'ok' : 'neutral'} /> : null}
          </Panel>
        ))}

        {item.path ? (
          <>
            <SectionTitle>Location</SectionTitle>
            <Text selectable style={[styles.path, { color: theme.muted, backgroundColor: theme.raised, borderColor: theme.border }]}>{item.path}</Text>
          </>
        ) : null}
      </ScrollView>
    </AppScreen>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42 },
  hero: { flexDirection: 'row', gap: 16 },
  heroBody: { flex: 1, paddingTop: 4, gap: 7 },
  title: { fontSize: 24, lineHeight: 29, fontWeight: '800' },
  meta: { fontSize: 12, lineHeight: 18 },
  badges: { flexDirection: 'row', flexWrap: 'wrap', gap: 5 },
  overview: { marginTop: 18, fontSize: 14, lineHeight: 22 },
  actions: { gap: 14, marginTop: 18 },
  monitorRow: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  flex: { flex: 1 },
  actionTitle: { fontSize: 15, fontWeight: '700' },
  actionHint: { fontSize: 12, lineHeight: 17, marginTop: 2 },
  stat: { fontSize: 14, fontWeight: '700', marginBottom: 5 },
  row: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 10, marginTop: 10, flexDirection: 'row', justifyContent: 'space-between', gap: 12 },
  rowTitle: { fontSize: 13, fontWeight: '600' },
  file: { gap: 7, marginBottom: 9 },
  rowBetween: { flexDirection: 'row', justifyContent: 'space-between', gap: 10 },
  fileName: { flex: 1, fontSize: 13, fontWeight: '700' },
  path: { borderWidth: 1, borderRadius: 12, padding: 12, fontSize: 12, lineHeight: 18 },
})
