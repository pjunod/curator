import { useEffect, useRef, useState, type RefObject, type SyntheticEvent } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useLocation } from '@tanstack/react-router'
import { ActionDialog, ActionNotice, focusActionTrigger } from '../ActionDialog'
import { ImportFileDialog } from '../ImportFileDialog'
import { PathInput } from '../PathInput'
import {
  ApiError, fmtBytes, fmtRelative, getLibrary, getLibraryItem, getManualImportDefaultPath,
  manualImport, resolveQueueReservation, scanImportPath, type QueueItem, type ScannedFile, type Stage,
} from '../api'
import {
  activityFileScope, activityIdentityKey, activityOperationChange, createActivitySelection, getActivityJobLog,
  getActivityOperation, getJobActivity, newActivityRequestId, postActivityAction,
  readActivityOperationIds, updateActivityOperationIds, type ActivityActionKind, type ActivityIdentity,
  type ActivityItem, type ActivityOperation, type ActivityOperationTarget, type ActivityRequest,
  type ActivityScope, type ActivitySelection, type ActivityTargets, type ActivityView,
} from '../activityActions'
import { fmtDuration } from '../queueTime'
import '../ImportQueue.css'
import '../Activity.css'

const STAGE_LABEL: Record<Stage, string> = {
  downloading: 'Downloading',
  par_rename: 'Renaming',
  par_verify: 'Verifying',
  par_repair: 'Repairing',
  rar_rename: 'Naming archives',
  unpack: 'Extracting',
  cleanup: 'Cleaning up',
  move: 'Moving',
  post_unpack_rename: 'Renaming',
  publication_verify: 'Verifying publication',
  script: 'Running scripts',
  importing: 'Copying to library',
  notifying: 'Notifying',
  publication_cleanup: 'Cleaning up publication',
}

// Progress the row can actually stand behind.
//
// A stage that cannot measure itself gets no bar at all, rather than one
// sitting at zero: an empty bar is the claim that nothing has happened, which
// is a different and usually false statement about a job that is midway
// through repairing a 60 GB archive.
function StageProgress({ d }: { d: QueueItem }) {
  if (d.importControl?.outcome === 'pending') return <span className="muted">Waiting for import to stop</span>

  if (d.control && (d.control.lifecycle === 'held' || d.control.version !== 1)) {
    return <p className="muted">Transfer {Math.round(d.progress * 100)}% · import pending</p>
  }
  if (d.importState === 'queued') {
    return <span className="muted">Waiting for an import worker</span>
  }
  if (d.importState === 'running' && !d.stage) {
    return <span className="muted">Starting importer…</span>
  }
  if (d.state === 'importing' && !d.stage && !d.importState) {
    return <span className="muted">Waiting for automatic import retry</span>
  }
  const known = typeof d.total === 'number' && d.total > 0 && typeof d.bytes === 'number'
  const done = d.bytes ?? 0
  const fraction = known ? Math.max(0, Math.min(1, done / (d.total as number))) : !d.stage && d.state === 'downloading' ? d.progress : null

  return (
    <div className="stage-progress">
      {fraction !== null && (
        <div className="progress-track" role="progressbar" aria-label="Measured stage progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(fraction * 100)}>
          <i style={{ width: `${Math.round(fraction * 100)}%` }} />
        </div>
      )}
      {known ? (
        <div className="muted stage-bytes">
          {fmtBytes(done)} / {fmtBytes(d.total as number)}
          {d.bytesPerSecond ? ` · ${fmtBytes(d.bytesPerSecond)}/s · ${fmtDuration(Math.max(0, ((d.total as number) - done) / d.bytesPerSecond))} remaining` : ''}
        </div>
      ) : d.stage ? (
        // Words, not digits — so this one wraps. Forcing the numeric
        // treatment on it pushed "post-processing: par repair · 4m ago"
        // straight out of the column and under the next one.
        <div className="muted stage-note">
          {d.stageDetail || 'in progress'}
          {d.stageSince ? ` · ${fmtRelative(d.stageSince)}` : ''}
        </div>
      ) : null}
    </div>
  )
}

const PAGE_SIZE = 50
const VIEWS = [['progress', 'In progress'], ['attention', 'Needs attention'], ['history', 'History']] as const
const EMPTY_TITLES = { progress: 'Nothing in progress', attention: 'Nothing needs your attention', history: 'No retained history' }

type Selection = {
  items: Record<string, { identity: ActivityIdentity; retryable: boolean }>
  snapshot?: ActivitySelection
  excludeIds: number[]
}

type ActionDraft = {
  request: ActivityRequest
  total: number
  label: string
  confirm: boolean
  retryOf?: string
}

type Dialog =
  | { kind: 'submit'; draft: ActionDraft }
  | { kind: 'operation'; id: string }
  | { kind: 'log'; item: ActivityItem }
  | { kind: 'manual' }
  | { kind: 'choose-files'; item: ActivityItem }
  | { kind: 'reservation'; item: ActivityItem }

const emptySelection = (): Selection => ({ items: {}, excludeIds: [] })
const readyToRetry = (item: ActivityItem) => item.actions?.some(action => action.kind === 'retry' && action.enabled) ?? false
const ACTION_TITLES = { delete: 'Delete jobs', retry: 'Retry jobs', approve: 'Approve import', archive: 'Clear from attention' }
const operationQueryKey = (id: string) => ['activity-operation', id]
function operationOptions(id: string) {
  return {
    queryKey: operationQueryKey(id),
    queryFn: () => getActivityOperation(id),
    retry: false as const,
    refetchInterval: (query: { state: { data?: ActivityOperation; error: Error | null } }) =>
      query.state.data?.state === 'completed' || (query.state.error instanceof ApiError && query.state.error.status === 404) ? false : 2000,
  }
}

export function ActivityPage() {
  const qc = useQueryClient()
  const hash = useLocation({ select: location => location.hash.replace(/^#/, '') })
  const [view, setView] = useState<ActivityView>(() => hash === 'history' || hash === 'attention' ? hash : 'progress')
  const [scope, setScope] = useState<ActivityScope>(() => new URLSearchParams(window.location.search).get('scope') === 'imports' ? 'imports' : 'all')
  const [linkedJob] = useState(() => new URLSearchParams(window.location.search).get('job'))
  const [search, setSearch] = useState(() => linkedJob && /^\d+$/.test(linkedJob) ? `#${linkedJob}` : '')
  const [offset, setOffset] = useState(0)
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [selection, setSelection] = useState<Selection>(emptySelection)
  const [manualQueued, setManualQueued] = useState(0)
  const [operationIds, setOperationIds] = useState(readActivityOperationIds)
  const submitting = useRef(false)
  const opener = useRef<HTMLElement | null>(null)
  const heading = useRef<HTMLHeadingElement | null>(null)
  const query = useQuery({
    queryKey: ['activity', view, scope, search, offset],
    queryFn: () => getJobActivity(view, scope, search, PAGE_SIZE, offset),
    refetchInterval: 4000,
    retry: false,
  })
  const data = query.data
  useEffect(() => {
    if (linkedJob && search === `#${linkedJob}` && data?.total === 0) {
      const found = VIEWS.find(([key]) => data.counts[key] > 0)
      if (found) setView(found[0])
    }
  }, [linkedJob, search, data])
  const operations = useQueries({ queries: operationIds.map(operationOptions) })
  const completedIds = operations.flatMap(({ data }) => data?.state === 'completed' ? [data.id] : []).join(',')
  const currentDialogOperation = dialog?.kind === 'operation'
    ? operations[operationIds.indexOf(dialog.id)]
    : undefined

  useEffect(() => {
    const sync = () => setOperationIds(readActivityOperationIds())
    window.addEventListener('storage', sync)
    window.addEventListener(activityOperationChange, sync)
    return () => {
      window.removeEventListener('storage', sync)
      window.removeEventListener(activityOperationChange, sync)
    }
  }, [])

  useEffect(() => {
    if (completedIds) void qc.invalidateQueries({ queryKey: ['activity'] })
  }, [completedIds, qc])

  useEffect(() => {
    if (hash === 'history' || hash === 'attention') {
      setView(hash)
      setOffset(0)
      setSelection(emptySelection())
    }
  }, [hash])

  useEffect(() => {
    if (data && offset > 0 && offset >= data.total) {
      setOffset(Math.max(0, Math.ceil(data.total / PAGE_SIZE) - 1) * PAGE_SIZE)
    }
  }, [data, offset])

  const invalidate = () => {
    for (const key of ['activity', 'queue', 'imports', 'queue-summary']) {
      void qc.invalidateQueries({ queryKey: [key] })
    }
  }
  const chooseAll = useMutation({
    mutationFn: () => createActivitySelection(scope, search),
    onSuccess: snapshot => setSelection({ items: {}, snapshot, excludeIds: [] }),
  })
  const submit = useMutation({
    mutationFn: ({ request }: ActionDraft) => postActivityAction(request),
    onSuccess: (operation, draft) => {
      void qc.cancelQueries({ queryKey: operationQueryKey(operation.id) })
      qc.setQueryData(operationQueryKey(operation.id), operation)
      updateActivityOperationIds(operation.id, true)
      if (draft.retryOf) updateActivityOperationIds(draft.retryOf, false)
      setDialog({ kind: 'operation', id: operation.id })
      setSelection(emptySelection())
      invalidate()
    },
    onSettled: () => { submitting.current = false },
    onError: (error, { request }) => {
      // An HTTP rejection is final for this submission. A lost response can
      // still represent an accepted command, so keep its ID and resend it verbatim.
      if (error instanceof ApiError && error.status >= 400 && error.status < 500 && error.status !== 408) {
        updateActivityOperationIds(request.requestId, false)
      }
    },
  })
  const release = useMutation({
    mutationFn: resolveQueueReservation,
    onSuccess: invalidate,
  })

  function open(event: SyntheticEvent<HTMLElement>, next: Dialog) {
    focusActionTrigger(event)
    opener.current = event.currentTarget
    setDialog(next)
  }

  function close() {
    if (submitting.current) return
    if (!opener.current?.isConnected || opener.current.matches(':disabled')) opener.current = heading.current
    setDialog(null)
  }

  function send(draft: ActionDraft) {
    if (submitting.current) return
    submitting.current = true
    // Write the lookup ID before the request so a completed submission can
    // be recovered if this tab closes before receiving its response.
    updateActivityOperationIds(draft.request.requestId, true)
    submit.mutate(draft)
  }

  function launch(event: SyntheticEvent<HTMLElement>, action: ActivityActionKind, targets: ActivityTargets, total: number, label = ACTION_TITLES[action], retryOf?: string) {
    if (submitting.current) return
    submit.reset()
    const confirm = action === 'delete' && !retryOf
    const draft = { request: { ...targets, action, requestId: newActivityRequestId() }, total, label, confirm, retryOf }
    if (retryOf) setDialog({ kind: 'submit', draft })
    else open(event, { kind: 'submit', draft })
    if (!confirm) send(draft)
  }

  function changeView(next: ActivityView) {
    setView(next)
    setOffset(0)
    setSelection(emptySelection())
    chooseAll.reset()
  }

  const items = data?.items ?? []
  const selected = Object.values(selection.items)
  const snapKeys = new Set(selection.snapshot?.targets.map(activityIdentityKey))
  const selectedCount = (selection.snapshot ? selection.snapshot.total - selection.excludeIds.length : 0) + selected.length
  const canRetrySelected = (selection.snapshot ? selection.snapshot.retryable > 0 : false) || selected.some(item => item.retryable)
  const retryableCount = selection.excludeIds.length === 0 ? (selection.snapshot?.retryable ?? 0) + selected.filter(item => item.retryable).length : null
  const selectionTargets: ActivityTargets = selection.snapshot
    ? { selection: { id: selection.snapshot.id, excludeIds: selection.excludeIds } }
    : { targets: selected.map(item => item.identity) }
  const busy = submit.isPending || query.isError || !data
  const selectionLocked = busy || chooseAll.isPending

  function toggle(item: ActivityItem, checked: boolean) {
    const identity = item.identity
    if (!identity) return
    const key = activityIdentityKey(identity)
    setSelection(previous => {
      if (snapKeys.has(key)) {
        return {
          ...previous,
          excludeIds: checked ? previous.excludeIds.filter(id => id !== identity.id) : [...previous.excludeIds, identity.id],
        }
      }
      const selectedItems = { ...previous.items }
      if (checked) selectedItems[key] = { identity, retryable: readyToRetry(item) }
      else delete selectedItems[key]
      return { ...previous, items: selectedItems }
    })
  }

  return <div className="activity-feed">
    <header className="page-head activity-page-head">
      <div><h1 ref={heading} tabIndex={-1}>Activity</h1><p className="muted">Downloads and imports.</p></div>
      <button onClick={event => open(event, { kind: 'manual' })}>Import files…</button>
    </header>
    {manualQueued > 0 && <ActionNotice revision={manualQueued}>Import queued. Follow its progress in Activity.</ActionNotice>}
    <nav className="activity-tabs" aria-label="Activity views">
      {VIEWS.map(([key, text]) => <button key={key} aria-pressed={view === key} disabled={chooseAll.isPending} onClick={() => changeView(key)}>{text} <span>{data?.counts[key] ?? '…'}</span></button>)}
    </nav>
    <div className="activity-toolbar">
      <input type="search" aria-label="Find a title or release" placeholder="Find a title or release" value={search} disabled={chooseAll.isPending}
        onChange={event => { setSearch(event.target.value); setOffset(0); setSelection(emptySelection()); chooseAll.reset() }} />
      <label>Show <select aria-label="Activity scope" value={scope} disabled={chooseAll.isPending}
        onChange={event => { setScope(event.target.value as ActivityScope); setOffset(0); setSelection(emptySelection()); chooseAll.reset() }}>
        <option value="all">All work</option><option value="imports">Imports only</option>
      </select></label>
    </div>
    {operations.map((operation, index) => {
      const id = operationIds[index]
      if (dialog?.kind === 'submit' && dialog.draft.request.requestId === id) return null
      return <div className="activity-operation panel" key={id}>
        <p className="muted">{operation.data ? operationStatus(operation.data) : operation.isError ? operation.error.message : 'Checking activity action…'}</p>
        <div className="row-actions">
          <button onClick={event => open(event, { kind: 'operation', id })}>Action results</button>
          {(operation.data?.state === 'completed' || (operation.error instanceof ApiError && operation.error.status === 404)) && <button onClick={() => updateActivityOperationIds(id, false)}>Dismiss</button>}
        </div>
      </div>
    })}
    {query.isLoading && <p role="status">Loading activity…</p>}
    {query.isError && <div className="banner warning" role="alert">
      {query.error.message} {data ? `Showing activity from ${new Date(query.dataUpdatedAt).toLocaleTimeString()}.` : 'Activity and counts are unavailable.'}{' '}
      <button onClick={() => void query.refetch()}>Refresh activity</button>
    </div>}
    {data && view === 'progress' && data.counts.attention > 0 && <div className="import-attention-banner">
      <strong>{data.counts.attention} {data.counts.attention === 1 ? 'job needs' : 'jobs need'} attention</strong>
      <button onClick={() => changeView('attention')}>View jobs</button>
    </div>}
    {view === 'attention' && !!(data?.total || selectedCount || selection.snapshot) && <div className="activity-selection">
      <div className="row-actions">
        <button disabled={selectionLocked || !data?.total || !!selection.snapshot} onClick={() => chooseAll.mutate()}>
          {chooseAll.isPending ? 'Selecting…' : 'Select all matching jobs'}
        </button>
        {(selectedCount > 0 || selection.snapshot) && <button onClick={() => setSelection(emptySelection())} disabled={chooseAll.isPending}>Clear selection</button>}
        {selectedCount > 0 && <>
          <span role="status">{selectedCount} selected{retryableCount !== null ? ` · ${retryableCount} can retry` : ''}</span>
          <button className="btn-accent" disabled={selectionLocked || !canRetrySelected} onClick={event => launch(event, 'retry', selectionTargets, selectedCount)}>Retry selected</button>
          <button disabled={selectionLocked} onClick={event => launch(event, 'archive', selectionTargets, selectedCount)}>Move selected to History (keep files)</button>
          <button className="btn-danger" disabled={selectionLocked} onClick={event => launch(event, 'delete', selectionTargets, selectedCount)}>Delete selected</button>
        </>}
      </div>
      {chooseAll.error && <p role="alert" className="error-text">{chooseAll.error.message}</p>}
    </div>}
    {data && items.length === 0 && !query.isError && <section className="activity-empty">
      <h2>{search ? 'No activity matches this search' : EMPTY_TITLES[view]}</h2>
      {search && <button onClick={() => { setSearch(''); setOffset(0); setSelection(emptySelection()); chooseAll.reset() }}>Clear search</button>}
    </section>}
    {items.map(item => {
      const d = item.download
      const identity = item.identity
      const key = identity ? activityIdentityKey(identity) : String(d.id)
      const inSnapshot = snapKeys.has(key)
      const isSelected = inSnapshot ? !selection.excludeIds.includes(d.id) : key in selection.items
      const actions = item.actions ?? []
      const unavailable = actions.find(action => !action.enabled && action.reason)?.reason
      const pendingDelete = item.operation?.action === 'delete' ? item.operation : undefined
      const status = activityStatus(item, view)
      const assignFolder = d.mediaItemId > 0 && /assign.{0,30}(library )?folder|no library folder assigned/i.test([item.prerequisite, d.error].filter(Boolean).join(' '))
      return <article className={`activity-attempt${view === 'attention' ? ' activity-attention-row' : ''}`} key={key} data-activity-id={d.id}>
        {view === 'attention' && identity && <input type="checkbox" aria-label={`Select ${item.title}${d.title === item.title ? '' : ` · ${d.title}`}`} checked={isSelected}
          disabled={selectionLocked || (!!selection.snapshot && !inSnapshot)} onChange={event => toggle(item, event.target.checked)} />}
        <div className="activity-row-content">
          <div className="activity-attempt-heading">
            <div>
              {d.mediaItemId > 0 ? <Link className="activity-title" to="/library/$id" params={{ id: String(d.mediaItemId) }}>{item.title}</Link> : <strong>{item.title}</strong>}
              {d.title !== item.title && <p className="muted small activity-release" title={d.title}>{d.title}</p>}
            </div>
            {view !== 'attention' && <span className="pill pill-neutral">{status}</span>}
          </div>
          {view === 'progress' && <StageProgress d={d} />}
          {d.publications?.filter(pub => pub.held).map(pub => <p key={pub.id}>Import held · {pub.heldReason || pub.error || 'Waiting for storage'}</p>)}
          {(item.reason || view === 'attention') && <p className="muted activity-row-message">{item.reason || (d.id > 0 ? 'This job needs attention.' : 'History is available for this import.')}</p>}
          {item.prerequisite && item.prerequisite !== item.reason && <p className="muted activity-row-message">{item.prerequisite}</p>}
          {!item.prerequisite && unavailable && unavailable !== item.reason && <p className="muted activity-row-message">{unavailable}</p>}
          {pendingDelete?.message && pendingDelete.message !== item.reason && <p className="muted activity-row-message">{pendingDelete.message}</p>}
          {view === 'history' && <p className="muted small"><time dateTime={item.updatedAt}>{new Date(item.updatedAt).toLocaleString()}</time></p>}
        </div>
        <div className="row-actions activity-row-controls">
          {assignFolder && view === 'attention' && <Link to="/library/$id" params={{ id: String(d.mediaItemId) }} search={{ assignFolder: true }}>Assign folder</Link>}
          {view === 'attention' && d.reservationActionable && <button disabled={busy} onClick={event => { release.reset(); open(event, { kind: 'reservation', item }) }}>Review reservation</button>}
          {view !== 'history' && identity && actions.filter(action => !(pendingDelete && action.kind === 'delete')).map(action => <button
            key={action.kind} className={action.kind === 'delete' ? 'btn-danger' : ''} disabled={busy || !action.enabled}
            onClick={event => {
              if (action.kind === 'choose_files') open(event, { kind: 'choose-files', item })
              else launch(event, action.kind, { targets: [identity] }, 1, action.label)
            }}>{action.label}</button>)}
          {view !== 'history' && pendingDelete && <button onClick={event => {
            updateActivityOperationIds(pendingDelete.id, true)
            open(event, { kind: 'operation', id: pendingDelete.id })
          }}>Deletion status</button>}
          {view === 'history' && pendingDelete?.state === 'blocked' && identity && <button disabled={busy} onClick={event => launch(event, 'delete', { targets: [identity] }, 1)}>Retry cleanup</button>}
          {view === 'history' && !pendingDelete && identity && actions.some(a => a.kind === 'archive') && actions.some(a => a.kind === 'retry' && a.enabled) && <button disabled={busy} onClick={event => launch(event, 'retry', { targets: [identity] }, 1)}>Retry import</button>}
          {d.id !== 0 && <button onClick={event => open(event, { kind: 'log', item })}>Job log</button>}
        </div>
      </article>
    })}
    {data && <footer className="import-footer">
      <div className="row-actions">
        <button disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>Previous</button>
        <span>{data.total ? `${offset + 1}–${Math.min(offset + PAGE_SIZE, data.total)} of ${data.total}` : '0 jobs'}</span>
        <button disabled={!data.hasMore} onClick={() => setOffset(offset + PAGE_SIZE)}>Next</button>
      </div>
      {view === 'history' && data.retentionDays > 0 && <p className="muted small">History is retained for {data.retentionDays} days.</p>}
    </footer>}
    {dialog?.kind === 'submit' && <ActionDialog
      title={dialog.draft.confirm ? `Delete ${dialog.draft.total} ${dialog.draft.total === 1 ? 'job' : 'jobs'} and files?` : dialog.draft.label}
      returnFocus={opener} onClose={close}>
      {dialog.draft.confirm && <p>Curator will stop the selected {dialog.draft.total === 1 ? 'job' : 'jobs'} and permanently remove job-owned downloads and temporary files. Media already installed in your library and externally supplied originals stay.</p>}
      {submit.isPending && <p role="status">Submitting action…</p>}
      {submit.error && <p className="error-text" role="alert">{submit.error.message}</p>}
      <div className="row-actions">
        <button disabled={submit.isPending} onClick={close}>Cancel</button>
        <button disabled={submit.isPending} className={dialog.draft.request.action === 'delete' ? 'btn-danger' : 'btn-accent'}
          onClick={() => send(dialog.draft)}>{dialog.draft.confirm ? 'Delete jobs and files' : 'Resubmit action'}</button>
      </div>
    </ActionDialog>}
    {dialog?.kind === 'operation' && <ActionDialog title={currentDialogOperation?.data ? ACTION_TITLES[currentDialogOperation.data.action] : 'Activity action'} returnFocus={opener} onClose={close}>
      {currentDialogOperation?.isError && <p role="alert" className="error-text">
        {currentDialogOperation.error.message} <button onClick={() => void currentDialogOperation.refetch()}>Check again</button>
      </p>}
      {currentDialogOperation?.data
        ? <OperationResults operation={currentDialogOperation.data} onRetry={(event, operation, targets) =>
          launch(event, operation.action, { targets: targets.map(({ id, generationKey, epoch }) => ({ id, generationKey, epoch })) }, targets.length, operation.action === 'delete' ? 'Retry deletion' : ACTION_TITLES[operation.action], operation.id)} />
        : !currentDialogOperation?.isError && <p role="status">Checking action progress…</p>}
    </ActionDialog>}
    {dialog?.kind === 'log' && <ActionDialog title={`Job log · ${dialog.item.title}`} returnFocus={opener} onClose={close}><JobLog id={dialog.item.download.id} /></ActionDialog>}
    {dialog?.kind === 'manual' && <ActionDialog title="Choose files to import" returnFocus={opener} onClose={close}>
      <ManualImportPanel onDone={() => { close(); changeView('progress'); invalidate(); setManualQueued(count => count + 1) }} />
    </ActionDialog>}
    {dialog?.kind === 'choose-files' && <ActivityFilePicker item={dialog.item} returnFocus={opener} onClose={() => { close(); invalidate() }} />}
    {dialog?.kind === 'reservation' && <ActionDialog title="Review episode reservation" returnFocus={opener} onClose={close}>
      <p>The original submission may still complete. Releasing the reservation lets you search for another copy of the episode and may result in a duplicate.</p>
      {dialog.item.download.parkedReason && <p>{dialog.item.download.parkedReason}</p>}
      {release.isSuccess ? <p role="status">Episode reservation released.</p> : <button disabled={release.isPending} onClick={() => release.mutate(dialog.item.download.id)}>Release reservation</button>}
      {release.error && <p role="alert" className="error-text">{release.error.message}</p>}
    </ActionDialog>}
  </div>
}

function activityStatus(item: ActivityItem, view: ActivityView) {
  const d = item.download
  if (view === 'history' && d.importControl?.outcome === 'cancelled') return 'Cancelled'
  if (d.importControl?.outcome === 'pending') return 'Cancelling import'
  if (d.stage && view === 'progress') return STAGE_LABEL[d.stage] ?? d.stage.replace(/_/g, ' ')
  if (d.importState === 'queued') return 'Waiting to import'
  if (d.importState === 'running') return 'Starting importer'
  if (d.state === 'importing') return 'Waiting for automatic import retry'
  if (d.state === 'downloaded') return 'Download complete'
  return d.state.replace(/_/g, ' ')
}

function operationStatus(operation: ActivityOperation) {
  const action = { delete: 'Deletion', retry: 'Retry', approve: 'Import approval', archive: 'Clear from attention' }[operation.action]
  if (operation.state === 'running') return `${action} in progress · ${operation.completed} of ${operation.total} jobs processed`
  return `${action} finished · ${operation.succeeded} succeeded · ${operation.blocked} blocked`
}

function OperationResults({ operation, onRetry }: {
  operation: ActivityOperation
  onRetry: (event: SyntheticEvent<HTMLButtonElement>, operation: ActivityOperation, targets: ActivityOperationTarget[]) => void
}) {
  const blocked = operation.targets.filter(target => target.state === 'blocked')
  const waiting = operation.targets.filter(target => (target.state === 'running' || target.state === 'pending') && target.message)
  const unresolved = [...blocked, ...waiting]
  return <>
    <p role="status">{operationStatus(operation)}</p>
    {operation.state === 'running' && operation.total > 0 && <progress aria-label="Jobs processed" max={operation.total} value={operation.completed} />}
    {unresolved.length > 0 && <ul className="activity-operation-targets">
      {unresolved.map(target => <li key={activityIdentityKey(target)}>
        <strong>{target.title || `Job ${target.id}`}</strong>
        {target.message && <p>{target.message}</p>}
      </li>)}
    </ul>}
    {blocked.length > 0 && operation.state === 'completed' && <button onClick={event => onRetry(event, operation, blocked)}>
      {operation.action === 'delete' ? 'Retry deletion' : 'Retry blocked jobs'}
    </button>}
  </>
}

function JobLog({ id }: { id: number }) {
  const log = useQuery({ queryKey: ['activity-job-log', id], queryFn: () => getActivityJobLog(id), retry: false })
  return <>
    {log.isLoading && <p role="status">Loading job log…</p>}
    {log.isError && <p role="alert" className="error-text">{log.error.message} <button onClick={() => void log.refetch()}>Try again</button></p>}
    {log.data?.summary && <p>{log.data.summary}</p>}
    {log.data && (log.data.events.length ? <ol className="activity-job-log">
      {log.data.events.map((entry, index) => <li key={`${entry.at}/${index}`}>
        <time dateTime={entry.at}>{new Date(entry.at).toLocaleString()}</time>
        <strong>{entry.step}</strong>
        {entry.message && <span>{entry.message}</span>}
      </li>)}
    </ol> : <p className="muted">No detailed events remain for this job.</p>)}
  </>
}

function ActivityFilePicker({ item, returnFocus, onClose }: {
  item: ActivityItem
  returnFocus: RefObject<HTMLElement | null>
  onClose: () => void
}) {
  const id = item.download.mediaItemId
  const library = useQuery({ queryKey: ['library', id], queryFn: () => getLibraryItem(id), enabled: id > 0, retry: false })
  if (library.data) return <ImportFileDialog item={library.data} scope={activityFileScope(item)} returnFocus={returnFocus} onClose={onClose} />
  return <ActionDialog title="Choose files to import" returnFocus={returnFocus} onClose={onClose}>
    {library.isLoading && <p role="status">Loading the library title…</p>}
    {library.error && <p className="error-text" role="alert">{library.error.message} <button onClick={() => void library.refetch()}>Try again</button></p>}
    {id <= 0 && <p>No library title is assigned to this job.</p>}
  </ActionDialog>
}

function ManualImportPanel({ onDone }: { onDone: () => void }) {
  const [path, setPath] = useState<string | null>(null)
  const [itemId, setItemId] = useState(0)
  const [copyId, setCopyId] = useState(0)
  const [scanned, setScanned] = useState<ScannedFile[] | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [scanErr, setScanErr] = useState<string>('')
  const [importErr, setImportErr] = useState<string>('')

  const library = useQuery({ queryKey: ['library'], queryFn: () => getLibrary() })
  const defaultPath = useQuery({
    queryKey: ['manual-import-default-path'],
    queryFn: getManualImportDefaultPath,
  })
  const shownPath = path ?? defaultPath.data?.path ?? ''
  const itemDetail = useQuery({
    queryKey: ['library', itemId],
    queryFn: () => getLibraryItem(itemId),
    enabled: itemId > 0,
  })

  const scan = useMutation({
    mutationFn: () => scanImportPath(shownPath),
    onSuccess: (files) => {
      setScanned(files)
      // A single-file result is unambiguous. A directory with several files
      // needs an explicit choice so one title cannot accidentally swallow a
      // neighbour from the completed folder.
      setSelected(new Set(files.length === 1 ? [files[0].path] : []))
      setScanErr('')
    },
    onError: (e: Error) => {
      setScanned(null)
      setScanErr(e.message)
    },
  })

  const run = useMutation({
    mutationFn: () =>
      manualImport({
        path: shownPath,
        paths: [...selected],
        mediaItemId: itemId,
        copyId: copyId || undefined,
      }),
    onSuccess: () => {
      setImportErr('')
      // The importer owns the rest. Close immediately so the accepted job is
      // visible in Activity and this form never becomes a progress modal for
      // a multi-gigabyte copy.
      onDone()
    },
    onError: (e: Error) => {
      setImportErr(e.message)
    },
  })

  const copies = itemDetail.data?.copies ?? []

  return (
    <section className="panel manual-import">

      <div className="form-row">
        <label htmlFor="manual-import-path">Path</label>
        <PathInput
          id="manual-import-path"
          value={shownPath}
          onChange={(next) => {
            setPath(next)
            setScanned(null)
            setSelected(new Set())
          }}
          placeholder="/pool/downloads/Some.Release.2024.1080p"
          showParent
        />
        <button onClick={() => scan.mutate()} disabled={!shownPath || scan.isPending}>
          {scan.isPending ? 'Scanning…' : 'Scan'}
        </button>
      </div>
      <p className="muted manual-import-path-help">
        Starts in Curator&apos;s completed-download folder. Type another absolute path or choose a
        folder suggestion; use Parent folder to navigate back up.
      </p>
      {scanErr && <p className="error-text">{scanErr}</p>}

      {scanned && (
        <div className="scan-result">
          {scanned.length === 0 ? (
            <p className="error-text">No media files found there. Check the path is reachable from Curator's container.</p>
          ) : (
            <div className="log-scroll">
            <table>
              <thead>
                <tr>
                  <th className="scan-select">
                    <input
                      type="checkbox"
                      aria-label="Select all files"
                      checked={scanned.length > 0 && selected.size === scanned.length}
                      onChange={(e) =>
                        setSelected(e.target.checked ? new Set(scanned.map((f) => f.path)) : new Set())
                      }
                    />
                  </th>
                  <th>File</th>
                  <th>Kind</th>
                  <th>Quality</th>
                  <th>Episode</th>
                  <th>Size</th>
                </tr>
              </thead>
              <tbody>
                {scanned.map((f) => (
                  <tr
                    key={f.path}
                    className={selected.has(f.path) ? 'scan-selected' : undefined}
                    onClick={() =>
                      setSelected((prev) => {
                        const next = new Set(prev)
                        if (next.has(f.path)) next.delete(f.path)
                        else next.add(f.path)
                        return next
                      })
                    }
                  >
                    <td className="scan-select">
                      <input
                        type="checkbox"
                        aria-label={`Select ${f.name}`}
                        checked={selected.has(f.path)}
                        onChange={() =>
                          setSelected((prev) => {
                            const next = new Set(prev)
                            if (next.has(f.path)) next.delete(f.path)
                            else next.add(f.path)
                            return next
                          })
                        }
                        onClick={(e) => e.stopPropagation()}
                      />
                    </td>
                    <td className="mono">{f.name}</td>
                    <td className="muted">{f.kind}</td>
                    <td className="muted">{f.quality || '—'}</td>
                    <td className="muted">
                      {f.season >= 0 && f.episodes.length > 0
                        ? `S${String(f.season).padStart(2, '0')}E${f.episodes
                            .map((e) => String(e).padStart(2, '0'))
                            .join('E')}`
                        : '—'}
                    </td>
                    <td className="muted">{fmtBytes(f.size)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          )}
        </div>
      )}

      <div className="form-row">
        <label>
          Import into
          <select value={itemId} onChange={(e) => { setItemId(Number(e.target.value)); setCopyId(0) }}>
            <option value={0}>Choose a title…</option>
            {(library.data ?? []).map((m) => (
              <option key={m.id} value={m.id}>
                {m.title}
                {m.year ? ` (${m.year})` : ''}
              </option>
            ))}
          </select>
        </label>
        {copies.length > 0 && (
          <label>
            Copy
            <select value={copyId} onChange={(e) => setCopyId(Number(e.target.value))}>
              <option value={0}>Primary</option>
              {copies.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name || `copy ${c.id}`}
                </option>
              ))}
            </select>
          </label>
        )}
        <button
          className="btn-accent"
          onClick={() => run.mutate()}
          disabled={!shownPath || !itemId || selected.size === 0 || run.isPending}
        >
          {run.isPending ? 'Queueing…' : `Queue selected (${selected.size})`}
        </button>
      </div>
      {scanned && scanned.length > 1 && selected.size === 0 && (
        <p className="muted">Select one or more files above to import.</p>
      )}
      {selected.size > 0 && !itemId && (
        <p className="muted">Choose the library title these selected files belong to.</p>
      )}
      {importErr && <p className="error-text">{importErr}</p>}
    </section>
  )
}
