import { useCallback, useEffect, useMemo, useState } from 'react'
import { Alert, FlatList, Linking, Pressable, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { AppScreen, Badge, Button, Chip, Field, Header, IconButton, InlineError, LoadingState, MessageState, Panel } from '../components/UI'
import { formatBytes } from '../format'
import { acceptedCount, candidateKey, filterCandidates, grabbedMessage, scopeLabel, type ReleaseFilter } from '../releaseSearch'
import { useTheme } from '../theme'
import type { MediaItemDetail, ReleaseCandidate, ReleaseSearchResponse, ReleaseSearchScope } from '../types'
import { useResource } from '../useResource'

// The native interactive search. It is the web panel's contract on a phone:
// every candidate the indexers returned is listed, a rejected release keeps
// its reasons instead of disappearing, and Grab sends any row to the client —
// the "would be grabbed" filter narrows the view and never blocks a grab.
//
// On a series the scope is chosen here rather than on the detail page. The
// phone detail page lists seasons, not episodes, so the season chips pick a
// pack and the episode chips narrow to one episode; both re-run the search.

export function ReleaseSearchScreen({ client, id, initialScope, onBack }: { client: MonarrClient; id: number; initialScope: ReleaseSearchScope; onBack: () => void }) {
  const resource = useResource(() => client.getLibraryItem(id), [client, id])
  const back = <IconButton label="Back" glyph="‹" onPress={onBack} />
  if (resource.loading) {
    return <AppScreen><Header title="Interactive search" left={back} /><LoadingState /></AppScreen>
  }
  if (!resource.data) {
    return <AppScreen><Header title="Interactive search" left={back} /><MessageState title="Title unavailable" message={resource.error} retry={() => void resource.refresh()} /></AppScreen>
  }
  return <ReleaseSearchBody client={client} item={resource.data} initialScope={initialScope} onBack={onBack} />
}

function ReleaseSearchBody({
  client,
  item,
  initialScope,
  onBack,
}: {
  client: MonarrClient
  item: MediaItemDetail
  initialScope: ReleaseSearchScope
  onBack: () => void
}) {
  const theme = useTheme()
  const [scope, setScope] = useState<ReleaseSearchScope>(initialScope)
  const [result, setResult] = useState<ReleaseSearchResponse | null>(null)
  const [error, setError] = useState('')
  const [searching, setSearching] = useState(true)
  const [filter, setFilter] = useState<ReleaseFilter>('all')
  const [query, setQuery] = useState('')
  const [grabbing, setGrabbing] = useState<string | null>(null)
  const [grabError, setGrabError] = useState('')
  const [grabbed, setGrabbed] = useState('')

  const search = useCallback(async (target: ReleaseSearchScope) => {
    setSearching(true)
    setError('')
    setResult(null)
    try {
      setResult(await client.searchReleases(item.id, target))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'The search failed.')
    } finally {
      setSearching(false)
    }
  }, [client, item.id])

  useEffect(() => {
    void search(scope)
  }, [search, scope])

  const changeScope = (next: ReleaseSearchScope) => {
    setGrabbed('')
    setGrabError('')
    setScope(next)
  }

  const grab = async (candidate: ReleaseCandidate) => {
    const key = candidateKey(candidate)
    setGrabbing(key)
    setGrabError('')
    setGrabbed('')
    try {
      await client.grabRelease({
        mediaItemId: item.id,
        copyId: scope.copyId,
        season: scope.season,
        episode: scope.episode,
        title: candidate.title,
        downloadUrl: candidate.downloadUrl,
        indexer: candidate.indexer,
        protocol: candidate.protocol,
        size: candidate.size,
        candidateToken: candidate.candidateToken,
      })
      setGrabbed(grabbedMessage(candidate.title))
    } catch (cause) {
      setGrabError(cause instanceof Error ? cause.message : 'Could not grab this release.')
    } finally {
      setGrabbing(null)
    }
  }

  const confirmGrab = (candidate: ReleaseCandidate) => {
    if (candidate.accepted) {
      void grab(candidate)
      return
    }
    // A rejected release can still be grabbed by hand — the confirmation
    // exists because a thumb on a phone is less deliberate than a click, not
    // because the grab is gated.
    Alert.alert(
      'Grab anyway?',
      `Curator would not pick this release on its own:\n${candidate.rejections.map((rejection) => `• ${rejection.reason}`).join('\n')}`,
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Grab anyway', onPress: () => void grab(candidate) },
      ],
    )
  }

  const all = useMemo(() => result?.candidates ?? [], [result])
  const accepted = useMemo(() => acceptedCount(all), [all])
  const visible = useMemo(() => filterCandidates(all, filter, query), [all, filter, query])
  const season = scope.season !== undefined ? item.seasons.find((candidate) => candidate.number === scope.season) : undefined
  const label = scopeLabel(scope, scope.copyId ? (item.copies ?? []).find((copy) => copy.id === scope.copyId)?.name || 'edition' : item.kind === 'series' ? 'series' : item.kind)

  const header = (
    <View style={styles.headerBlock}>
      {item.kind === 'series' ? (
        <Panel style={styles.scopePanel}>
          <Text style={[styles.scopeLabel, { color: theme.text }]}>Season</Text>
          <View style={styles.chips}>
            {item.seasons.map((candidate) => (
              <Chip
                key={candidate.number}
                label={candidate.number === 0 ? 'Specials' : `S${candidate.number}`}
                selected={candidate.number === scope.season}
                onPress={searching ? undefined : () => changeScope({ copyId: scope.copyId, season: candidate.number })}
              />
            ))}
          </View>
          {season ? (
            <>
              <Text style={[styles.scopeLabel, { color: theme.text }]}>Episode</Text>
              <View style={styles.chips}>
                <Chip label="Whole pack" selected={scope.episode === undefined} onPress={searching ? undefined : () => changeScope({ copyId: scope.copyId, season: season.number })} />
                {season.episodes.map((episode) => (
                  <Chip
                    key={episode.id}
                    label={`E${episode.episodeNumber}${episode.hasFile ? ' ✓' : ''}`}
                    selected={scope.episode === episode.episodeNumber}
                    onPress={searching ? undefined : () => changeScope({ copyId: scope.copyId, season: season.number, episode: episode.episodeNumber })}
                  />
                ))}
              </View>
            </>
          ) : null}
        </Panel>
      ) : null}

      {searching ? <LoadingState label="Searching indexers…" /> : null}
      {result?.partial ? (
        <Text style={[styles.banner, { color: theme.warning, borderColor: theme.warning }]}>Results are incomplete{result.reason ? `: ${result.reason}` : '.'}</Text>
      ) : null}
      <InlineError message={error} />
      <InlineError message={grabError} />
      {grabbed ? <Text style={[styles.success, { color: theme.ok }]}>{grabbed}</Text> : null}

      {!searching && !error && all.length === 0 ? (
        <Text style={[styles.hint, { color: theme.muted }]}>No releases found on any enabled indexer.</Text>
      ) : null}

      {all.length > 0 ? (
        <View style={styles.toolbar}>
          <Field placeholder="Filter by name…" value={query} onChangeText={setQuery} autoCapitalize="none" autoCorrect={false} clearButtonMode="while-editing" />
          <View style={styles.chips}>
            <Chip label={`Everything (${all.length})`} selected={filter === 'all'} onPress={() => setFilter('all')} />
            <Chip label={`Would be grabbed (${accepted})`} selected={filter === 'accepted'} onPress={() => setFilter('accepted')} />
          </View>
          {visible.length === 0 ? (
            <Text style={[styles.hint, { color: theme.muted }]}>
              Nothing matches that filter.{accepted === 0 ? ' No release here passes the profile — switch to "Everything" to see why each was declined.' : ''}
            </Text>
          ) : null}
        </View>
      ) : null}
    </View>
  )

  return (
    <AppScreen>
      <Header title="Interactive search" subtitle={`${item.title} · ${label}`} left={<IconButton label="Back" glyph="‹" onPress={onBack} />} />
      <FlatList
        data={visible}
        keyExtractor={candidateKey}
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={styles.content}
        ListHeaderComponent={header}
        renderItem={({ item: candidate }) => (
          <ReleaseRow candidate={candidate} busy={grabbing !== null} grabbing={grabbing === candidateKey(candidate)} onGrab={() => confirmGrab(candidate)} />
        )}
      />
    </AppScreen>
  )
}

/**
 * One release. The first rejection shows and the rest expand on tap, so the
 * reason a search "found nothing useful" is one glance away without turning
 * every row into a paragraph.
 */
export function ReleaseRow({ candidate, busy, grabbing, onGrab }: { candidate: ReleaseCandidate; busy: boolean; grabbing: boolean; onGrab: () => void }) {
  const theme = useTheme()
  const [expanded, setExpanded] = useState(false)
  const [first, ...rest] = candidate.rejections

  return (
    <Panel style={candidate.accepted ? styles.row : styles.rowRejected}>
      <Pressable
        accessibilityRole={candidate.infoUrl ? 'link' : 'text'}
        disabled={!candidate.infoUrl}
        onPress={candidate.infoUrl ? () => void Linking.openURL(candidate.infoUrl!) : undefined}
      >
        <Text style={[styles.title, { color: candidate.infoUrl ? theme.accent : theme.text }]}>{candidate.title}</Text>
      </Pressable>
      <View style={styles.badges}>
        <Badge label={candidate.quality} />
        {candidate.score !== 0 ? <Badge label={`score ${candidate.score > 0 ? '+' : ''}${candidate.score}`} /> : null}
        {candidate.isUpgrade ? <Badge label="upgrade" tone="ok" /> : null}
        {candidate.accepted ? <Badge label="Would grab" tone="ok" /> : <Badge label="Rejected" tone="warning" />}
      </View>
      <Text style={[styles.meta, { color: theme.muted }]}>
        {[formatBytes(candidate.size), candidate.age || '', candidate.protocol === 'torrent' ? `${candidate.seeders} seeders` : candidate.protocol, candidate.indexer].filter(Boolean).join(' · ')}
      </Text>
      {candidate.match.matched ? <Text style={[styles.meta, { color: theme.ok }]}>✓ {candidate.match.reason}</Text> : null}
      {candidate.warning ? <Text style={[styles.meta, { color: theme.warning }]}>⚠ {candidate.warning}</Text> : null}
      {first ? (
        <Pressable accessibilityRole="button" disabled={rest.length === 0} onPress={() => setExpanded((value) => !value)}>
          <Text style={[styles.meta, { color: theme.error }]}>
            ✕ {first.reason}
            {rest.length > 0 ? <Text style={{ color: theme.accent }}>{expanded ? '  less' : `  +${rest.length} more`}</Text> : null}
          </Text>
        </Pressable>
      ) : null}
      {expanded
        ? rest.map((rejection) => (
            <Text key={rejection.code} style={[styles.meta, { color: theme.error }]}>✕ {rejection.reason}</Text>
          ))
        : null}
      <View style={styles.actions}>
        <Button compact secondary={!candidate.accepted} label={grabbing ? 'Grabbing…' : candidate.accepted ? 'Grab' : 'Grab anyway'} disabled={busy} onPress={onGrab} />
      </View>
    </Panel>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42, gap: 10 },
  headerBlock: { gap: 12, marginBottom: 2 },
  scopePanel: { gap: 8 },
  scopeLabel: { fontSize: 13, fontWeight: '700' },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  toolbar: { gap: 10 },
  banner: { borderWidth: 1, borderRadius: 12, padding: 12, fontSize: 13, lineHeight: 19 },
  success: { fontSize: 13, lineHeight: 19 },
  hint: { fontSize: 13, lineHeight: 19 },
  row: { gap: 8 },
  rowRejected: { gap: 8, opacity: 0.82 },
  title: { fontSize: 13, lineHeight: 18, fontWeight: '700' },
  badges: { flexDirection: 'row', flexWrap: 'wrap', gap: 5 },
  meta: { fontSize: 12, lineHeight: 17 },
  actions: { flexDirection: 'row', justifyContent: 'flex-end' },
})
