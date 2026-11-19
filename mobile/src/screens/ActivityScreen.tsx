import { useEffect, useRef, useState } from 'react'
import { Alert, Modal, Pressable, RefreshControl, ScrollView, StyleSheet, Text, View } from 'react-native'
import { randomUUID } from 'expo-crypto'
import {
  activityStorageKey, identityKey, loadActivityIds, saveActivityIds,
  type ActivityActionKind, type ActivityIdentity, type ActivityItem, type ActivityOperation,
  type ActivityRequest, type ActivitySelection, type ActivityTargets, type ActivityView,
} from '../activityActions'
import { ApiError, type MonarrClient } from '../api'
import { formatBytes, formatRelative } from '../format'
import { queueBadge } from '../queueState'
import { AppScreen, Badge, Button, Chip, Header, InlineError, LoadingState, MessageState, Panel, Wordmark } from '../components/UI'
import { useTheme } from '../theme'
import { useResource } from '../useResource'

const VIEWS = [['progress', 'In progress'], ['attention', 'Needs attention'], ['history', 'History']] as const
const EMPTY_TITLES = { progress: 'Nothing in progress', attention: 'Nothing needs your attention', history: 'No retained history' }
const ACTION_TITLES = { delete: 'Deletion', retry: 'Retry', approve: 'Import approval' }
const PAGE_SIZE = 50

type Selection = {
  items: Record<string, { identity: ActivityIdentity; retryable: boolean }>
  snapshot?: ActivitySelection
  excludeIds: number[]
}

type Draft = {
  request: ActivityRequest
  total: number
  label: string
  confirm: boolean
  retryOf?: string
}

type Dialog = { kind: 'draft'; draft: Draft } | { kind: 'operation'; id: string } | { kind: 'log'; item: ActivityItem }

type ObservedOperation = { data?: ActivityOperation; error?: string; missing?: boolean }

const emptySelection = (): Selection => ({ items: {}, excludeIds: [] })
const messageOf = (error: unknown) => error instanceof Error ? error.message : String(error)
function operationStatus(operation: ActivityOperation) {
  return operation.state === 'running'
    ? `${ACTION_TITLES[operation.action]} in progress · ${operation.completed} of ${operation.total} jobs processed`
    : `${ACTION_TITLES[operation.action]} finished · ${operation.succeeded} succeeded · ${operation.blocked} blocked`
}

export function ActivityScreen({ client, onOpen }: { client: MonarrClient; onOpen: (id: number) => void }) {
  const theme = useTheme()
  const [view, setView] = useState<ActivityView>('progress')
  const [offset, setOffset] = useState(0)
  const [selection, setSelection] = useState<Selection>(emptySelection)
  const [selecting, setSelecting] = useState(false)
  const [selectionError, setSelectionError] = useState('')
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [actionError, setActionError] = useState('')
  const [storage, setStorage] = useState<{ key: string; ids: string[] } | null>(null)
  const [operations, setOperations] = useState<Record<string, ObservedOperation>>({})
  const [pollRevision, setPollRevision] = useState(0)
  const storageRef = useRef(storage)
  storageRef.current = storage
  const busyRef = useRef(false)
  const resource = useResource(async () => ({
    view, offset, activity: await client.getActivity(view, PAGE_SIZE, offset),
  }), [client, view, offset])
  const activity = resource.data?.view === view && resource.data.offset === offset ? resource.data.activity : null
  const refresh = resource.refresh

  useEffect(() => {
    let current = true
    void (async () => {
      const key = await activityStorageKey(client.connection.baseUrl)
      const ids = await loadActivityIds(key)
      if (current) setStorage({ key, ids })
    })().catch(() => {
      if (current) setStorage({ key: '', ids: [] })
    })
    return () => { current = false }
  }, [client])

  useEffect(() => {
    if (!storage?.ids.length) return
    let current = true
    let timer: ReturnType<typeof setTimeout> | undefined
    let pending = [...storage.ids]
    const poll = async () => {
      const results = await Promise.all(pending.map(async id => {
        try {
          return { id, data: await client.getActivityOperation(id) }
        } catch (error) {
          return { id, error: messageOf(error), missing: error instanceof ApiError && error.status === 404 }
        }
      }))
      if (!current) return
      const observations = Object.fromEntries(results.map(({ id, ...result }) => [id, result]))
      setOperations(previous => {
        const next = { ...previous }
        for (const [id, observation] of Object.entries(observations)) {
          next[id] = { data: previous[id]?.data, ...observation }
        }
        return next
      })
      pending = results.filter(result => result.data?.state !== 'completed' && !result.missing).map(result => result.id)
      if (pending.length > 0) timer = setTimeout(() => void poll(), 2500)
    }
    void poll()
    return () => { current = false; clearTimeout(timer) }
  }, [client, storage, pollRevision])

  const finished = Object.values(operations).flatMap(entry => entry.data?.state === 'completed' ? [entry.data.id] : []).join(',')
  useEffect(() => {
    if (finished) void refresh()
  }, [finished, refresh])

  useEffect(() => {
    if (offset > 0 && activity && offset >= activity.total) {
      setOffset(Math.max(0, Math.ceil(activity.total / PAGE_SIZE) - 1) * PAGE_SIZE)
    }
  }, [activity, offset])

  function remember(id: string, keep: boolean) {
    const previous = storageRef.current
    if (!previous) return
    const ids = previous.ids.filter(item => item !== id)
    if (keep) ids.unshift(id)
    const next = { key: previous.key, ids }
    storageRef.current = next
    setStorage(next)
    if (previous.key) void saveActivityIds(previous.key, ids)
  }

  async function submit(draft: Draft) {
    if (busyRef.current) return
    busyRef.current = true
    setSubmitting(true)
    setActionError('')
    remember(draft.request.requestId, true)
    try {
      const operation = await client.submitActivityAction(draft.request)
      setOperations(previous => ({ ...previous, [operation.id]: { data: operation } }))
      remember(operation.id, true)
      if (draft.retryOf) remember(draft.retryOf, false)
      setSelection(emptySelection())
      setDialog({ kind: 'operation', id: operation.id })
    } catch (error) {
      if (error instanceof ApiError && error.status >= 400 && error.status < 500 && error.status !== 408) {
        remember(draft.request.requestId, false)
      }
      setActionError(messageOf(error))
    } finally {
      busyRef.current = false
      setSubmitting(false)
    }
  }

  function launch(action: ActivityActionKind, targets: ActivityTargets, total: number, label: string, retryOf?: string) {
    if (busyRef.current) return
    const confirm = action === 'delete' && !retryOf
    const draft: Draft = { request: { ...targets, action, requestId: randomUUID() }, total, label, confirm, retryOf }
    setDialog({ kind: 'draft', draft })
    setActionError('')
    if (!confirm) void submit(draft)
  }

  function changeView(next: ActivityView) {
    if (selecting) return
    setOffset(0)
    setView(next)
    setSelection(emptySelection())
    setSelectionError('')
  }

  const busy = !activity || !!resource.error || resource.refreshing || !storage || submitting
  const snapshotKeys = new Set(selection.snapshot?.targets.map(identityKey))
  const selected = Object.values(selection.items)
  const selectedCount = (selection.snapshot ? selection.snapshot.total - selection.excludeIds.length : 0) + selected.length
  const canRetrySelected = (selection.snapshot?.retryable ?? 0) > 0 || selected.some(item => item.retryable)
  const selectionTargets: ActivityTargets = selection.snapshot
    ? { selection: { id: selection.snapshot.id, excludeIds: selection.excludeIds } }
    : { targets: selected.map(item => item.identity) }

  async function selectAll() {
    setSelecting(true)
    setSelectionError('')
    try {
      const snapshot = await client.createActivitySelection()
      setSelection({ items: {}, snapshot, excludeIds: [] })
    } catch (error) {
      setSelectionError(messageOf(error))
    } finally {
      setSelecting(false)
    }
  }

  function toggle(item: ActivityItem, isSelected: boolean) {
    const identity = item.identity
    if (!identity) return
    const key = identityKey(identity)
    setSelection(previous => {
      if (snapshotKeys.has(key)) return {
        ...previous,
        excludeIds: isSelected ? [...previous.excludeIds, identity.id] : previous.excludeIds.filter(id => id !== identity.id),
      }
      const next = { ...previous.items }
      if (isSelected) delete next[key]
      else next[key] = { identity, retryable: item.actions?.some(action => action.kind === 'retry' && action.enabled) ?? false }
      return { ...previous, items: next }
    })
  }

  function showOperation(id: string) {
    remember(id, true)
    setDialog({ kind: 'operation', id })
  }

  const openedOperation = dialog?.kind === 'operation' ? operations[dialog.id] : undefined
  const blocked = openedOperation?.data?.targets.filter(target => target.state === 'blocked') ?? []
  const waiting = openedOperation?.data?.targets.filter(target =>
    (target.state === 'running' || target.state === 'pending') && target.message) ?? []
  function reviewReservation(item: ActivityItem) {
    Alert.alert('Release episode reservation?',
      'The original submission may still complete. Releasing the reservation lets you search for another copy and may result in a duplicate.',
      [{ text: 'Cancel', style: 'cancel' }, {
        text: 'Release reservation', style: 'destructive', onPress: () => {
          void client.resolveQueueReservation(item.download.id).then(() => {
            Alert.alert('Episode reservation released')
            void refresh()
          }).catch(error => Alert.alert('Reservation remains', messageOf(error)))
        },
      }])
  }
  const close = () => {
    if (!busyRef.current) setDialog(null)
  }

  return (
    <AppScreen>
      <Header title="Activity" left={<Wordmark size={19} />} />
      <ScrollView contentContainerStyle={styles.content}
        refreshControl={<RefreshControl refreshing={resource.refreshing} tintColor={theme.accent} onRefresh={() => void refresh()} />}>
        <View style={styles.actions}>
          {VIEWS.map(([value, label]) => <Chip key={value} label={`${label} ${activity?.counts[value] ?? '…'}`} selected={view === value} onPress={() => changeView(value)} />)}
        </View>
        <InlineError message={resource.error} />
        {(storage?.ids ?? []).map(id => {
          const operation = operations[id]
          return <Panel key={id} style={styles.item}>
            <Text style={{ color: theme.text }}>{operation?.data ? operationStatus(operation.data) : operation?.error ?? 'Checking activity action…'}</Text>
            <View style={styles.actions}>
              <Button compact secondary label="Action results" onPress={() => showOperation(id)} />
              {(operation?.data?.state === 'completed' || operation?.missing) &&
                <Button compact secondary label="Dismiss" onPress={() => remember(id, false)} />}
            </View>
          </Panel>
        })}
        {view === 'attention' && !!(activity?.total || selectedCount || selection.snapshot) && <Panel style={styles.item}>
          <View style={styles.actions}>
            <Button compact secondary disabled={busy || selecting || !activity?.total || !!selection.snapshot}
              label={selecting ? 'Selecting…' : 'Select all matching jobs'} onPress={() => void selectAll()} />
            {(selectedCount > 0 || selection.snapshot) && <Button compact secondary label="Clear selection" onPress={() => setSelection(emptySelection())} />}
          </View>
          {selectedCount > 0 && <>
            <Text style={{ color: theme.text }}>{selectedCount} selected</Text>
            <View style={styles.actions}>
              <Button compact disabled={busy || selecting || !canRetrySelected} label="Retry selected"
                onPress={() => launch('retry', selectionTargets, selectedCount, 'Retry jobs')} />
              <Button compact secondary disabled={busy || selecting} label="Delete selected"
                onPress={() => launch('delete', selectionTargets, selectedCount, 'Delete jobs')} />
            </View>
          </>}
          <InlineError message={selectionError} />
        </Panel>}
        {resource.loading && !activity && <LoadingState label="Loading activity…" />}
        {!resource.error && activity?.items.length === 0 && <MessageState title={EMPTY_TITLES[view]} />}
        {activity?.items.map(item => {
          const key = item.identity ? identityKey(item.identity) : String(item.download.id)
          const inSnapshot = snapshotKeys.has(key)
          const selected = inSnapshot ? !selection.excludeIds.includes(item.download.id) : key in selection.items
          return <ActivityRow key={key} item={item} view={view} selected={selected}
            selectionDisabled={busy || selecting || (!!selection.snapshot && !inSnapshot)} busy={busy}
            onSelect={() => toggle(item, selected)} onOpen={() => onOpen(item.download.mediaItemId)}
            onAction={action => {
              if (item.identity) launch(action.kind, { targets: [item.identity] }, 1, action.label)
            }}
            onOperation={showOperation} onReservation={() => reviewReservation(item)} onLog={() => setDialog({ kind: 'log', item })} />
        })}
        {!!activity && (activity.hasMore || offset > 0) && <View style={styles.actions}>
          <Button compact secondary disabled={offset === 0} label="Previous" onPress={() => setOffset(Math.max(0, offset - PAGE_SIZE))} />
          <Text style={{ color: theme.muted }}>{offset + 1}–{Math.min(offset + PAGE_SIZE, activity.total)} of {activity.total}</Text>
          <Button compact secondary disabled={!activity.hasMore} label="Next" onPress={() => setOffset(offset + PAGE_SIZE)} />
        </View>}
      </ScrollView>
      {dialog && <Modal visible animationType="slide" presentationStyle="fullScreen" onRequestClose={close}>
        <AppScreen forceTopInset>
          <Header title={dialog.kind === 'log' ? 'Job log' : dialog.kind === 'draft' ? dialog.draft.label : 'Activity action'}
            right={<Button compact secondary label="Close" disabled={submitting} onPress={close} />} />
          <ScrollView contentContainerStyle={styles.content}>
            {dialog.kind === 'draft' && <>
              {dialog.draft.confirm && <Text style={[styles.title, { color: theme.text }]}>
                Delete {dialog.draft.total} {dialog.draft.total === 1 ? 'job' : 'jobs'} and files?
              </Text>}
              {dialog.draft.confirm && <Text style={{ color: theme.text }}>
                Curator will stop the selected jobs and permanently remove job-owned downloads and temporary files. Media already installed in your library and externally supplied originals stay.
              </Text>}
              {submitting && <LoadingState label="Submitting action…" />}
              <InlineError message={actionError} />
              {!submitting && <View style={styles.actions}>
                <Button secondary label="Cancel" onPress={close} />
                <Button label={dialog.draft.confirm ? 'Delete jobs and files' : 'Resubmit action'} onPress={() => void submit(dialog.draft)} />
              </View>}
            </>}
            {dialog.kind === 'operation' && <>
              {openedOperation?.error && <View style={styles.item}>
                <InlineError message={openedOperation.error} />
                <Button compact secondary label="Check again" onPress={() => setPollRevision(value => value + 1)} />
              </View>}
              {openedOperation?.data ? <>
                <Text accessibilityLiveRegion="polite" style={[styles.title, { color: theme.text }]}>{operationStatus(openedOperation.data)}</Text>
                {[...blocked, ...waiting].map(target => <Panel key={identityKey(target)} style={styles.item}>
                  <Text style={[styles.title, { color: theme.text }]}>{target.title || `Job ${target.id}`}</Text>
                  {target.message && <Text style={{ color: theme.muted }}>{target.message}</Text>}
                </Panel>)}
                {blocked.length > 0 && openedOperation.data.state === 'completed' && <Button
                  label={openedOperation.data.action === 'delete' ? 'Retry deletion' : 'Retry blocked jobs'}
                  onPress={() => {
                    const operation = openedOperation.data
                    if (!operation) return
                    const targets = blocked.map(({ id, generationKey, epoch }) => ({ id, generationKey, epoch }))
                    launch(operation.action, { targets }, targets.length, operation.action === 'delete' ? 'Retry deletion' : ACTION_TITLES[operation.action], operation.id)
                  }} />}
              </> : !openedOperation?.error && <LoadingState label="Checking action progress…" />}
            </>}
            {dialog.kind === 'log' && <JobLog client={client} item={dialog.item} />}
          </ScrollView>
        </AppScreen>
      </Modal>}
    </AppScreen>
  )
}

function ActivityRow({ item, view, selected, selectionDisabled, busy, onSelect, onOpen, onAction, onOperation, onReservation, onLog }: {
  item: ActivityItem
  view: ActivityView
  selected: boolean
  selectionDisabled: boolean
  busy: boolean
  onSelect: () => void
  onOpen: () => void
  onAction: (action: NonNullable<ActivityItem['actions']>[number]) => void
  onOperation: (id: string) => void
  onReservation: () => void
  onLog: () => void
}) {
  const theme = useTheme()
  const download = item.download
  const total = download.total ?? 0
  const measured = total > 0 && download.bytes !== undefined
  const fraction = measured ? (download.bytes ?? 0) / total
    : !download.stage && download.state === 'downloading' ? download.progress : null
  const progress = fraction === null ? null : Math.round(Math.max(0, Math.min(1, fraction)) * 100)
  const badge = queueBadge(download)
  const deletion = item.operation?.action === 'delete' ? item.operation : undefined
  const actions = item.actions ?? []
  const unavailable = actions.find(action => !action.enabled && action.reason)?.reason
  return <Panel style={styles.item}>
    <View style={styles.itemHead}>
      {view === 'attention' && item.identity && <Pressable
        accessibilityRole="checkbox" accessibilityLabel={`Select ${item.title}${download.title === item.title ? '' : ` · ${download.title}`}`} accessibilityState={{ checked: selected, disabled: selectionDisabled }}
        disabled={selectionDisabled} onPress={onSelect} style={styles.checkbox}>
        <Text style={{ color: selectionDisabled ? theme.muted : theme.accent }}>{selected ? '☑' : '☐'}</Text>
      </Pressable>}
      <Text numberOfLines={3} accessibilityRole={download.mediaItemId > 0 ? 'link' : undefined}
        onPress={download.mediaItemId > 0 ? onOpen : undefined} style={[styles.title, { flex: 1, color: theme.text }]}>{item.title}</Text>
      {view !== 'attention' && <Badge label={view === 'progress' && download.stageDetail ? download.stageDetail : badge.label} tone={badge.tone} />}
    </View>
    {download.title !== item.title && <Text numberOfLines={2} style={[styles.meta, { color: theme.muted }]}>{download.title}</Text>}
    {(item.reason || view === 'attention') && <Text style={[styles.meta, { color: theme.muted }]}>{item.reason || 'This job needs attention.'}</Text>}
    {item.prerequisite && item.prerequisite !== item.reason && <Text style={[styles.meta, { color: theme.muted }]}>{item.prerequisite}</Text>}
    {!item.prerequisite && unavailable && unavailable !== item.reason && <Text style={[styles.meta, { color: theme.muted }]}>{unavailable}</Text>}
    {deletion?.message && deletion.message !== item.reason && <Text style={[styles.meta, { color: theme.muted }]}>{deletion.message}</Text>}
    {view === 'progress' && progress !== null && <>
      <View accessibilityRole="progressbar" accessibilityValue={{ min: 0, max: 100, now: progress }}
        style={[styles.track, { backgroundColor: theme.hover }]}>
        <View style={[styles.progress, { backgroundColor: theme.accent, width: `${progress}%` }]} />
      </View>
      {measured && <Text style={[styles.meta, { color: theme.muted }]}>{formatBytes(download.bytes ?? 0)} / {formatBytes(total)}</Text>}
    </>}
    {view === 'history' && <Text style={[styles.meta, { color: theme.muted }]}>{formatRelative(item.updatedAt)}</Text>}
    <View style={styles.actions}>
      {view === 'attention' && download.reservationActionable && <Button compact secondary disabled={busy} label="Review reservation" onPress={onReservation} />}
      {view !== 'history' && item.identity && actions.filter(action => !(deletion && action.kind === 'delete')).map(action =>
        <Button key={action.kind} compact secondary disabled={busy || !action.enabled} label={action.label} onPress={() => onAction(action)} />)}
      {view !== 'history' && deletion && <Button compact secondary label="Deletion status" onPress={() => onOperation(deletion.id)} />}
      {view === 'history' && download.id > 0 && <Button compact secondary label="Job log" onPress={onLog} />}
    </View>
  </Panel>
}

function JobLog({ client, item }: { client: MonarrClient; item: ActivityItem }) {
  const theme = useTheme()
  const resource = useResource(() => client.getActivityJobLog(item.download.id), [client, item.download.id])
  return <>
    <Text style={[styles.title, { color: theme.text }]}>{item.title}</Text>
    {resource.loading && <LoadingState label="Loading job log…" />}
    <InlineError message={resource.error} />
    {!!resource.error && <Button compact secondary label="Try again" onPress={() => void resource.refresh()} />}
    {resource.data && <>
      {!!resource.data.summary && <Text style={{ color: theme.text }}>{resource.data.summary}</Text>}
      {resource.data.events.map((event, index) => <Panel key={`${event.at}/${index}`} style={styles.item}>
        <Text style={[styles.meta, { color: theme.muted }]}>{new Date(event.at).toLocaleString()}</Text>
        <Text style={[styles.title, { color: theme.text }]}>{event.step}</Text>
        {!!event.message && <Text style={{ color: theme.text }}>{event.message}</Text>}
      </Panel>)}
      {!resource.data.events.length && <Text style={{ color: theme.muted }}>No detailed events remain for this job.</Text>}
    </>}
  </>
}

const styles = StyleSheet.create({
  content: { padding: 16, gap: 12, paddingBottom: 36 },
  actions: { flexDirection: 'row', gap: 7, flexWrap: 'wrap', alignItems: 'center' },
  item: { gap: 7 },
  itemHead: { flexDirection: 'row', gap: 8, alignItems: 'flex-start' },
  title: { fontSize: 15, lineHeight: 20, fontWeight: '700' },
  meta: { fontSize: 13, lineHeight: 18 },
  checkbox: { minWidth: 34, minHeight: 30, alignItems: 'center' },
  track: { height: 5, borderRadius: 3, overflow: 'hidden' },
  progress: { height: 5, borderRadius: 3 },
})
