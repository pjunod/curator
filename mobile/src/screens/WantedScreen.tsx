import { useEffect, useRef, useState } from 'react'
import { FlatList, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { AppScreen, Badge, Button, Header, InlineError, LoadingState, MessageState, Panel, Wordmark } from '../components/UI'
import { useTheme } from '../theme'
import type { WantedItem, WantedSearchRun, WantedSearchScope } from '../types'
import { useResource } from '../useResource'

export function WantedScreen({ client, onOpen }: { client: MonarrClient; onOpen: (id: number) => void }) {
  const theme = useTheme()
  const resource = useResource(() => client.getWanted(), [client])
  const runs = useResource(() => client.getWantedSearches(), [client])
  const [cancelling, setCancelling] = useState<Set<string>>(new Set())
  const [error, setError] = useState('')
  const previousRuns = useRef<WantedSearchRun[] | undefined>(undefined)
  const active = runs.data?.items.some(isActiveRun)

  useEffect(() => {
    let stopped = false
    let timeout: ReturnType<typeof setTimeout>
    const schedule = () => {
      timeout = setTimeout(async () => {
        await runs.refresh()
        if (!stopped) schedule()
      }, active ? 1000 : 5000)
    }
    schedule()
    return () => { stopped = true; clearTimeout(timeout) }
  }, [active, runs.refresh])

  useEffect(() => {
    if (!runs.data) return
    const previous = previousRuns.current
    previousRuns.current = runs.data.items
    if (!previous) return
    const terminal = new Set(previous.filter((run) => !isActiveRun(run)).map((run) => run.runId))
    if (runs.data.items.some((run) => !isActiveRun(run) && !terminal.has(run.runId))) {
      void resource.refresh()
    }
  }, [resource.refresh, runs.data])

  const search = async (scope: WantedSearchScope) => {
    setError('')
    try {
      await client.createWantedSearch(scope)
      await runs.refresh()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not start the search.')
    }
  }

  const cancel = async (runId: string) => {
    setCancelling((current) => new Set([...current, runId]))
    setError('')
    try {
      await client.cancelWantedSearch(runId)
      await runs.refresh()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not cancel the search.')
    } finally {
      setCancelling((current) => new Set([...current].filter((id) => id !== runId)))
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
        onRefresh={() => { void resource.refresh(); void runs.refresh() }}
        ListHeaderComponent={
          <View style={styles.intro}>
            <Text style={[styles.explainer, { color: theme.muted }]}>Everything monitored that is missing or below its quality cutoff.</Text>
            <Button label="Search all now" disabled={!resource.data?.length} onPress={() => void search({ scope: 'all' })} />
            <InlineError message={error || resource.error || runs.error} />
            {runs.data?.items.map((run) => (
              <Panel key={run.runId} style={styles.row}>
                <Text style={[styles.title, { color: theme.text }]}>{run.scopeLabel}</Text>
                <Text style={[styles.detail, { color: theme.muted }]}>
                  {run.status[0]!.toUpperCase() + run.status.slice(1)} · {run.processed} of {run.selected} processed · {run.grabbed} grabbed
                </Text>
                {run.error ? <InlineError message={run.error} /> : null}
                {isActiveRun(run) ? (
                  <Button compact secondary label={run.cancelRequestedAt ? 'Cancelling…' : 'Cancel'}
                    disabled={cancelling.has(run.runId) || !!run.cancelRequestedAt} onPress={() => void cancel(run.runId)} />
                ) : null}
              </Panel>
            ))}
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
              <Button compact secondary label="Search title" onPress={() => void search({ scope: 'group', mediaItemId: item.mediaItemId })} />
          </Panel>
        )}
      />
    </AppScreen>
  )
}

function isActiveRun(run: WantedSearchRun): boolean {
  return run.status === 'queued' || run.status === 'running'
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
