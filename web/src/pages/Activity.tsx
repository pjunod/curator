import { ActionDialog, focusActionTrigger } from '../ActionDialog'
import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  type ManualImportRequest,
  type QueueItem,
  type PublicationFileAction,
  type QueuePublication,
  type ScannedFile,
  type Stage,
  blocklistQueueItem,
  cancelQueueImport,
  getImportReview,
  resolveImportReview,
  fmtBytes,
  fmtRelative,
  getLibrary,
  getLibraryItem,
  getManualImportDefaultPath,
  getActivity,
  type ImportActivityItem,
  importQueueItem,
  resumeQueueItem,
  manualImport,
  removeQueueItem,
  resolvePublicationFile,
  setPublicationCleanup,
  resolveQueueReservation,
  scanImportPath,
  publicationHoldText,
} from '../api'
import { PathInput } from '../PathInput'
import '../LegacyReview.css'
import '../ImportQueue.css'
import '../Activity.css'
import { canRetryImport, importActivityLabel, importNeedsReview } from '../importActivity'
import { awaitingText, fmtDuration } from '../queueTime'

const STATE_PILL: Record<string, string> = {
  imported: 'pill-ok',
  failed: 'pill-error',
  awaiting_import: 'pill-warning',
  downloaded: 'pill-neutral',
  downloading: 'pill-neutral',
  importing: 'pill-neutral',
  grabbed: 'pill-neutral',
  submitting: 'pill-neutral',
  held: 'pill-warning',
  notified: 'pill-ok',
  planned: 'pill-neutral',
}

// Friendlier labels for the state pill and the trace steps.
const STATE_LABEL: Record<string, string> = {
  awaiting_import: 'awaiting approval',
  downloaded: 'completed',
  held: 'held',
}

const STEP_LABEL: Record<string, string> = {
  grabbed: 'Grabbed',
  downloading: 'Downloading',
  downloaded: 'Downloaded',
  awaiting_import: 'Awaiting approval',
  import_queued: 'Queued for import',
  importing: 'Importing',
  import_cancelled: 'Import cancelled',
  import_recovered: 'Import recovered',
  imported: 'Imported',
  failed: 'Failed',
  held: 'Held by Runner',
  reopened: 'Re-opened',
  salvage_published: 'Salvage published',
}

function label(map: Record<string, string>, key: string): string {
  return map[key] ?? key.replace(/_/g, ' ')
}

// One job, one row, and the stage word changes as it moves.
//
// Fetching, repairing, extracting and copying into the library are parts of
// one job, not different kinds of work that belong on different screens. They
// are separate SECTIONS because lumping them into one pile answers "how many
// things are busy" and not "what is this one doing" — and the second question
// is the one anybody actually opens this page with.
//
// The keys are the download client's own stage names. Translating them here,
// at the edge, is deliberate: nothing between nzbd and this line has to agree
// on English.
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
  if(d.importControl?.outcome==='pending') return <span role="status">{d.importControl.pendingPublications>0&&!d.importState ? 'Cancellation pending · publication review required' : 'Cancelling · waiting for actual writer exit'}</span>
  if(d.importControl?.retainedPublications) return <span className="warning-text">Cancelled · retained publication requires Library review</span>
  if(d.importControl?.outcome==='cancelled') return <span className="muted">Cancelled · files retained; explicit retry available</span>

  if (d.control && (d.control.lifecycle === 'held' || d.control.version !== 1)) {
    return <div><span className="pill pill-neutral">Held</span><div className="muted">{d.control.message}</div><div className="muted">Transfer {Math.round(d.progress * 100)}% · import pending</div></div>
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

function clock(ms: number): string {
  const d = new Date(ms)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

const ACTIVITY_PAGE_SIZE = 50
const ACTIVITY_VIEWS = [['progress', 'In progress'], ['attention', 'Needs attention'], ['history', 'History']] as const

function attemptKey(d: QueueItem): string { return JSON.stringify([d.id, d.importControl?.generationKey, d.importControl?.epoch]) }

export function ActivityPage() {
  const qc = useQueryClient()
  const [view, setView] = useState<'progress' | 'attention' | 'history'>('progress')
  const [scope, setScope] = useState<'all' | 'imports'>(() => new URLSearchParams(window.location.search).get('scope') === 'imports' ? 'imports' : 'all')
  const [search, setSearch] = useState('')
  const [offset, setOffset] = useState(0)
  const [detail, setDetail] = useState<ImportActivityItem | null>(null)
  const [manual, setManual] = useState<Partial<ManualImportRequest> | null>(null)
  const [review, setReview] = useState<number | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const opener = useRef<HTMLElement | null>(null)
  const heading = useRef<HTMLHeadingElement | null>(null)
  const query = useQuery({ queryKey: ['activity', view, scope, search, offset], queryFn: () => getActivity(view, scope, search, ACTIVITY_PAGE_SIZE, offset), refetchInterval: 4000, retry: false })
  const data = query.data
  useEffect(() => {
    if (data && offset > 0 && offset >= data.total) {
      setOffset(Math.max(0, Math.ceil(data.total / ACTIVITY_PAGE_SIZE) - 1) * ACTIVITY_PAGE_SIZE)
      setSelected(new Set())
    }
  }, [data, offset])
  const invalidate = () => {
    for (const key of ['activity', 'queue', 'imports', 'queue-summary']) void qc.invalidateQueries({ queryKey: [key] })
  }
  const remember = () => { opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : heading.current }
  const close = () => {
    if (!opener.current?.isConnected || opener.current.matches(':disabled')) opener.current = heading.current
    setDetail(null); setManual(null); setReview(null); invalidate()
  }
  const [result, setResult] = useState<string[] | null>(null)
  const action = useMutation({
    mutationFn: async ({ rows, kind }: { rows: QueueItem[]; kind: 'import' | 'cancel' | 'remove' | 'blocklist' }) => {
      const results: string[] = []
      for (const snapshot of rows) {
        const cached = qc.getQueryData<NonNullable<typeof data>>(['activity', view, scope, search, offset])
        const d = cached?.items.find(i => i.download.id === snapshot.id)?.download
        try {
          if (query.isError || qc.getQueryState(['activity', view, scope, search, offset])?.status === 'error' || !d || d.id < 1 || attemptKey(d) !== attemptKey(snapshot)) throw new Error('This import attempt changed. Refresh and select it again.')
          if (kind === 'import') {
            if (d.control?.lifecycle === 'held') await resumeQueueItem(d.id)
            else await importQueueItem(d.id)
          } else if (kind === 'cancel') {
            if (!d.importControl) throw new Error('Refresh before cancelling this import.')
            await cancelQueueImport(snapshot.id, snapshot.importControl!)
          } else if (kind === 'remove') await removeQueueItem(d.id, false)
          else await blocklistQueueItem(d.id)
          results.push(`${d.title}: ${kind === 'cancel' ? 'Cancellation requested. Files stay protected until the writer stops.' : kind === 'import' ? 'Import accepted. Follow its status in Activity.' : kind === 'remove' ? 'Eligible download record removed. Library files were left in place.' : 'Blocklist request completed.'}`)
        } catch (e) { results.push(`${snapshot.title}: ${e instanceof Error ? e.message : String(e)}`) }
        setResult([...results])
      }
      return results
    },
    onError: (error: Error) => setResult([error.message]),
    onSettled: () => { setSelected(new Set()); invalidate() },
  })
  const run = (rows: QueueItem[], kind: 'import' | 'cancel' | 'remove' | 'blocklist') => { if (!detail) remember(); setDetail(null); setResult([]); action.mutate({ rows, kind }) }
  const changeView = (next: typeof view) => { setView(next); setOffset(0); setSelected(new Set()) }
  const items = data?.items ?? []
  const retryable = items.filter(canRetryImport)
  const selectedItems = retryable.filter(i => selected.has(attemptKey(i.download)))
  const liveDetail = detail && items.find(i => i.download.id === detail.download.id)
  const staleDetail = !!detail && (!liveDetail || attemptKey(liveDetail.download) !== attemptKey(detail.download))
  const currentDetail = detail && (!staleDetail && liveDetail ? liveDetail : detail)
  const busy = action.isPending || query.isError || !data
  return <div className="activity-feed">
    <header className="page-head"><div><h1 ref={heading} tabIndex={-1}>Activity</h1><p className="muted">Downloads and imports, in one place.</p></div><span className="muted small">{query.dataUpdatedAt ? `Updated ${new Date(query.dataUpdatedAt).toLocaleTimeString()}` : 'Waiting for activity'}</span></header>
    <nav className="activity-tabs" aria-label="Activity views">{ACTIVITY_VIEWS.map(([key, text]) => <button key={key} aria-pressed={view === key} onClick={() => changeView(key)}>{text} <span>{data?.counts[key] ?? '…'}</span></button>)}</nav>
    <div className="activity-toolbar"><input type="search" aria-label="Find a title or release" placeholder="Find a title or release" value={search} onChange={e => { setSearch(e.target.value); setOffset(0); setSelected(new Set()) }} /><label>Show <select aria-label="Activity scope" value={scope} onChange={e => { setScope(e.target.value as typeof scope); setOffset(0); setSelected(new Set()) }}><option value="all">All work</option><option value="imports">Imports only</option></select></label></div>
    {query.isLoading && <p role="status">Loading activity…</p>}
    {query.isError && <div className="banner warning" role="alert">Could not refresh activity: {query.error.message}. {data ? 'Showing the last successful snapshot; actions are disabled.' : 'Activity and counts are unavailable.'} <button onClick={() => void query.refetch()}>Try again</button></div>}
    {data && view === 'progress' && data.counts.attention > 0 && <div className="import-attention-banner"><div><strong>{data.counts.attention} item{data.counts.attention === 1 ? '' : 's'} need{data.counts.attention === 1 ? 's' : ''} a decision</strong><p className="muted small">Other downloads and imports continue automatically.</p></div><button onClick={() => changeView('attention')}>Review</button></div>}
    {view === 'attention' && retryable.length > 0 && <div className="row-actions"><label><input type="checkbox" aria-label="Select retryable imports on this page" checked={selectedItems.length === retryable.length} onChange={e => setSelected(new Set(e.target.checked ? retryable.map(i => attemptKey(i.download)) : []))} /> Select page</label><button disabled={busy || selectedItems.length === 0} onClick={event => { focusActionTrigger(event); run(selectedItems.map(i => i.download), 'import') }}>Retry selected ({selectedItems.length})</button></div>}
    {data && items.length === 0 && !query.isError && <section className="activity-empty"><h2>{search ? 'No activity matches this search' : view === 'progress' ? 'Nothing in progress' : view === 'attention' ? 'Nothing needs your attention' : 'No retained history'}</h2><p className="muted">{view === 'progress' ? 'New downloads and imports appear here. Automatic retries continue without a separate recovery queue.' : 'Only recorded attempts in this view and scope appear here.'}</p>{search && <button onClick={() => { setSearch(''); setOffset(0) }}>Clear search</button>}</section>}
    {items.map(item => {
      const d = item.download
      const stage = d.importControl?.outcome === 'pending' ? 'Cancelling import' : d.stage ? STAGE_LABEL[d.stage] : d.importState === 'queued' ? 'Waiting to import' : d.importState === 'running' ? 'Starting importer' : d.state === 'importing' ? 'Waiting for automatic import retry' : item.category === 'attention' ? importActivityLabel(item) : label(STATE_LABEL, d.state)
      return <article className="activity-attempt" key={d.id} data-activity-id={d.id}>
        <div className="activity-attempt-heading"><div>{canRetryImport(item) && <input type="checkbox" aria-label={`Select ${item.title}`} checked={selected.has(attemptKey(d))} onChange={e => setSelected(prev => { const next = new Set(prev); if (e.target.checked) next.add(attemptKey(d)); else next.delete(attemptKey(d)); return next })} />}{d.mediaItemId > 0 ? <Link className="activity-title" to="/library/$id" params={{ id: String(d.mediaItemId) }}>{item.title}</Link> : <strong>{item.title}</strong>}<p className="muted small activity-release" title={d.title}>{d.title}</p><p className="muted small">{[item.kind, d.quality, d.id < 0 ? 'Historical import · read only' : d.protocol === 'manual' ? 'Manual import' : null].filter(Boolean).join(' · ')}</p></div><span className={`pill ${view === 'attention' ? 'pill-warning' : 'pill-neutral'}`}>{stage}</span></div>
        {view === 'progress' && <StageProgress d={d} />}
        {d.state === 'held' && <HeldLine d={d} />}
        {d.error && <p className="muted small">{d.error}</p>}
        {d.importBlockedSince && <p className="muted small">Waiting on the environment · {d.importRetryAt ? `next retry ${fmtRelative(d.importRetryAt)}` : 'automatic retry pending'}</p>}
        <div className="activity-attempt-footer"><span className="muted small">Updated {fmtRelative(item.updatedAt)}</span><div className="row-actions">
          {d.id > 0 && importNeedsReview(item) && <button disabled={busy} onClick={event => { focusActionTrigger(event); remember(); setReview(d.id) }}>Review retained files</button>}
          {d.state === 'awaiting_import' && canRetryImport(item) && <button disabled={busy} onClick={event => { focusActionTrigger(event); run([d], 'import') }}>Approve import</button>}
          <button onClick={event => { focusActionTrigger(event); remember(); setDetail(item) }}>Details</button>
        </div></div>
      </article>
    })}
    {data && <footer className="import-footer"><div className="row-actions"><button disabled={offset === 0} onClick={() => { setOffset(Math.max(0, offset - ACTIVITY_PAGE_SIZE)); setSelected(new Set()) }}>Previous</button><span>{data.total ? `${offset + 1}–${Math.min(offset + ACTIVITY_PAGE_SIZE, data.total)} of ${data.total}` : '0 attempts'}</span><button disabled={!data.hasMore} onClick={() => { setOffset(offset + ACTIVITY_PAGE_SIZE); setSelected(new Set()) }}>Next</button></div><p className="muted small">{data.retentionDays === 0 ? 'Automatic history expiry is disabled.' : `History is retained for ${data.retentionDays} days.`} Storage tools are under <Link to="/system" hash="storage">System → Storage</Link>.</p></footer>}
    {currentDetail && <ActionDialog title={currentDetail.title} returnFocus={opener} onClose={close} className="activity-detail-dialog"><p className="muted">{currentDetail.download.title}</p>{currentDetail.destination && <p>Library destination <span className="path-chip">{currentDetail.destination}</span></p>}{staleDetail && <p role="status">This attempt moved out of the current view. Refresh the view before changing it.</p>}{currentDetail.download.id < 0 ? <PublicationList d={currentDetail.download} readOnly /> : <table className="queue-table"><tbody><RowGroup d={currentDetail.download} open busy={busy || staleDetail} onToggle={close} onImport={() => run([currentDetail.download], 'import')} onCancelImport={() => run([currentDetail.download], 'cancel')} onBlocklist={() => run([currentDetail.download], 'blocklist')} onRemove={() => run([currentDetail.download], 'remove')} onManual={() => { setDetail(null); setManual({ path: currentDetail.download.importPath || currentDetail.download.savePath, downloadId: currentDetail.download.id, mediaItemId: currentDetail.download.mediaItemId, copyId: currentDetail.download.copyId }) }} /></tbody></table>}</ActionDialog>}
    {manual && <ActionDialog title="Choose files to import" returnFocus={opener} onClose={close}><ManualImportPanel prefill={manual} onDone={close} /></ActionDialog>}
    {review !== null && <ActionDialog title="Review retained files" returnFocus={opener} onClose={close}><ImportReviewPanel id={review} readOnly={busy} /></ActionDialog>}
    {result && <ActionDialog title="Activity action results" returnFocus={opener} onClose={() => { if (!action.isPending) { if (!opener.current?.isConnected || opener.current.matches(':disabled')) opener.current = heading.current; setResult(null) } }}><div aria-live="polite">{action.isPending && <p role="status">Submitting actions…</p>}{result.map((message, i) => <p key={i}>{message}</p>)}</div></ActionDialog>}
  </div>
}

function RowGroup(props: {
  d: QueueItem
  open: boolean
  busy: boolean
  onToggle: () => void
  onImport: () => void
  onCancelImport: () => void
  onBlocklist: () => void
  onRemove: () => void
  onManual: () => void
}) {
  const { d, open, busy } = props
 const [whyOpen,setWhyOpen]=useState(false)
  const [reviewOpen,setReviewOpen]=useState(false)
  const [acknowledged,setAcknowledged]=useState(false)
  const qc=useQueryClient()
  const resolve=useMutation({mutationFn:()=>resolveQueueReservation(d.id),onSuccess:()=>{setReviewOpen(false);void qc.invalidateQueries({queryKey:['activity']});void qc.invalidateQueries({queryKey:['queue']});void qc.invalidateQueries({queryKey:['imports']});void qc.invalidateQueries({queryKey:['queue-summary']})}})
  const [importReviewOpen,setImportReviewOpen]=useState(false)
  const cancellationPending=d.importControl?.outcome==='pending'
  const publicationReview=!!d.importControl?.retainedPublications||!!d.importControl?.pendingPublications
  const retryBlocked=cancellationPending||publicationReview
  const queuedImport = d.importState === 'queued'
  const startingImport = d.importState === 'running' && !d.stage
  const canImport = (d.state === 'awaiting_import' || d.state === 'downloaded') && !d.importState && !retryBlocked
  const canRetry = d.state === 'failed' && d.submissionPhase !== 'uncertain' && !retryBlocked
  const interruptedImport = d.state === 'importing' && !d.stage && !d.importState && !retryBlocked
  const activeImport = d.stage === 'importing'
  const importerOwned = queuedImport || startingImport || activeImport
  const held = !!d.control && (d.control.lifecycle === 'held' || d.control.version !== 1)
  const canBlocklist = d.state !== 'imported' && !importerOwned && !held

  return (
    <>
      <tr>
        <td>
          <button
            className="row-toggle"
            aria-label={open ? 'Collapse handoff' : 'Show handoff'}
            aria-expanded={open}
            onClick={props.onToggle}
          >
            {open ? '▾' : '▸'}
          </button>
        </td>
        <td className="mono">
          <span className="queue-title" title={d.title}>{d.title}</span>
          {/* One line, elided: the release column is what is left after the
              fixed columns, and a four-line match sentence under every title
              turned each row into a paragraph. The full reason is the title. */}
          {d.match?.reason && <div className="muted queue-match" title={d.match.reason}>Match: {d.match.reason}</div>}
          {d.error && <div className="error-text queue-row-detail" title={d.error}>{d.error}</div>}
          {d.importBlockedSince && <div className={`queue-row-detail ${Date.now() - new Date(d.importBlockedSince).getTime() >= 30 * 60 * 1000 ? 'warning-text' : 'muted'}`} title={`Import blocked ${fmtRelative(d.importBlockedSince)} · ${d.importRetryAttempts ?? 0} retries · ${d.importRetryAt && new Date(d.importRetryAt).getTime() > Date.now() ? `next retry ${fmtRelative(d.importRetryAt)}` : 'retry due'}`}>
            Import blocked {fmtRelative(d.importBlockedSince)} · {d.importRetryAttempts ?? 0} retries · {d.importRetryAt && new Date(d.importRetryAt).getTime() > Date.now() ? `next retry ${fmtRelative(d.importRetryAt)}` : 'retry due'}
          </div>}
          {d.state === 'held' && <HeldLine d={d} />}
          {d.state === 'awaiting_import' && <AwaitingLine d={d} />}
          {d.provisional && <div className="muted queue-row-detail" title="Curator's own conclusion; any later fact from Runner re-opens this row">Provisional: {d.runnerReason ?? 'failed'} — re-opens if Runner reports this transfer</div>}
          {d.parkedReason && <div className="muted queue-row-detail" title={`Reserved: ${d.parkedReason}`}>Reserved: {d.parkedReason}</div>}
          {reviewOpen && <ActionDialog title="Review episode reservation" onClose={()=>setReviewOpen(false)}>
            <p>The original submission may still exist or complete later. Releasing custody permits a deliberate Search to acquire another copy. The original row and a persistent possible-duplicate warning remain.</p>
            <p>{d.parkedReason || 'Submission acceptance is uncertain.'}</p>
            <label><input type="checkbox" checked={acknowledged} onChange={(e)=>setAcknowledged(e.target.checked)}/>I acknowledge the possible duplicate and have reviewed the client.</label>
            {resolve.error && <p role="alert">{resolve.error.message}</p>}
            <button disabled={busy || !acknowledged || resolve.isPending} onClick={()=>resolve.mutate()}>Release reservation</button>
          </ActionDialog>}
          {whyOpen && <ActionDialog title="Why chosen" onClose={()=>setWhyOpen(false)}>
            {d.selection ? <>
              <p>{d.selection.reason}</p>
              <p>Target: {d.selection.target}. {d.selection.partial ? 'Partial discovery; pack comparison remains deferred.' : 'Completed bounded discovery.'}</p>
              <p>Plan {d.planId}: {d.planState}. Submission: {d.submissionPhase}.</p>
              <p>{d.selection.plan.Cost.UnknownSizes>0 ? `${d.selection.plan.Cost.UnknownSizes} transfers have unknown size; known subtotal ${fmtBytes(d.selection.plan.Cost.KnownBytes)}.` : `Full advertised transfer cost: ${fmtBytes(d.selection.plan.Cost.KnownBytes)}.`}</p>
              {d.selection.plan.Cost.ZeroSeedTorrents>0 && <p className="warning-text">Selected torrents include reported zero-seeder supply; higher attainable outcome takes priority over health.</p>}
              <ul>{d.selection.plan.Releases.map(key=><li key={key}>{d.selection!.titles[key]} — permitted episode IDs: {d.selection!.importAllowlists[key]?.join(', ') || 'none'}</li>)}</ul>
              {d.selection.plan.Unserved.length>0 && <p>Unserved episode IDs: {d.selection.plan.Unserved.join(', ')}. No admissible supply in this pool.</p>}
            </> : <p>Selection reason was not recorded for this legacy or explicit manual grab.</p>}
            {d.match?.reason && <p>Identity match: {d.match.reason}</p>}
          </ActionDialog>}

        </td>
        <td className="muted">{d.quality}</td>
        <td>
          {/*
            While a job is in flight the section heading already says which
            stage it is at, so repeating it here is noise — and worse than
            noise: a row sitting under "Repairing" showed a "downloading" pill,
            because `state` is the coarse lifecycle and cannot tell the two
            apart. That is the same wrong claim the backend was making, left
            on the screen.

            So a moving row answers the question the section does not: WHICH
            APPLICATION is doing this. Curator's own name appears exactly once,
            on the one stage Curator performs itself.
          */}
          {cancellationPending ? <span className="pill pill-warning">cancelling</span> : publicationReview ? <span className="pill pill-warning">review required</span> : queuedImport ? (
            <span className="pill pill-neutral">queued</span>
          ) : startingImport ? (
            <span className="muted stage-owner">Curator</span>
          ) : interruptedImport ? (
            <span className="pill pill-warning">interrupted</span>
          ) : d.stage ? (
            <span className="muted stage-owner">{d.stagePeer || 'Curator'}</span>
          ) : (
            <span className={`pill ${STATE_PILL[d.state] ?? 'pill-neutral'}`}>
              {label(STATE_LABEL, d.state)}
            </span>
          )}
        </td>
        <td>
          <StageProgress d={d} />
        </td>
        <td className="muted nowrap" title={d.addedAt}>
          {fmtRelative(d.addedAt)}
        </td>
        <td>
          <div className="row-actions">
            <button onClick={(event)=>{focusActionTrigger(event);setWhyOpen(true)}}>Why chosen</button>
            {(d.importControl?.outcome==='pending'||d.importControl?.outcome==='cancelled'||publicationReview||!!d.importControl?.heldStages)&&<button onClick={(event)=>{focusActionTrigger(event);setImportReviewOpen(true)}}>Review retained import</button>}
            {importReviewOpen&&<ActionDialog title="Review retained import" onClose={()=>setImportReviewOpen(false)}><ImportReviewPanel id={d.id} readOnly={busy}/></ActionDialog>}

            {d.reservationActionable && <button disabled={busy} onClick={(event)=>{focusActionTrigger(event);setAcknowledged(false);resolve.reset();setReviewOpen(true)}}>Review reservation</button>}
            {held && d.control?.retry_policy === 'resume_same_job' && <button onClick={(event)=>{focusActionTrigger(event);props.onImport()}} disabled={busy}>Resume same job</button>}
            {canImport && !held && (
              <button onClick={(event)=>{focusActionTrigger(event);props.onImport()}} disabled={busy}>
                Import now
              </button>
            )}
            {canRetry && !held && (
              <>
                <button onClick={(event)=>{focusActionTrigger(event);props.onImport()}} disabled={busy}>
                  Retry
                </button>
                <button onClick={props.onManual} disabled={busy}>
                  Manual import
                </button>
              </>
            )}
            {interruptedImport && (
              <button onClick={(event)=>{focusActionTrigger(event);props.onImport()}} disabled={busy}>
                Restart import
              </button>
            )}
            {(importerOwned || interruptedImport) && !cancellationPending && (
              <button className="btn-danger" onClick={(event)=>{focusActionTrigger(event);props.onCancelImport()}} disabled={busy}>
                Cancel import
              </button>
            )}
            {d.error?.includes('no library folder assigned') && (
              <Link
                to="/library/$id"
                params={{ id: String(d.mediaItemId) }}
                search={{ assignFolder: true }}
              >
                Assign folder
              </Link>
            )}
            {canBlocklist && (
              <button className="btn-danger" onClick={props.onBlocklist} disabled={busy}>
                Blocklist
              </button>
            )}
            {!importerOwned && !interruptedImport && (
              <button onClick={props.onRemove} disabled={busy}>
                Remove
              </button>
            )}
          </div>
        </td>
      </tr>
      {open && (
        <tr className="handoff-row">
          <td></td>
          <td colSpan={6}>
            <HandoffDetail d={d} readOnly={busy} />
          </td>
        </tr>
      )}
    </>
  )
}

// A held row says what Runner is waiting on and how long it has left; past
// its deadline it is "held (stale)": Runner has not reported since, and
// Curator is reconciling, never failing it on its own (contract 3.2).
function HeldLine({ d }: { d: QueueItem }) {
  const deadline = d.deadlineAt ? new Date(d.deadlineAt).getTime() : 0
  const left = deadline ? deadline - Date.now() : 0
  const text = d.stale
    ? `Held (stale) — ${d.heldReason || 'Runner has not reported since the hold began'}`
    : `Held: ${d.heldReason || d.control?.message || 'waiting on Runner'}${deadline ? ` · Runner ends it in ${fmtDuration(left)}` : ''}`
  return <div className={`queue-row-detail ${d.stale ? 'warning-text' : 'muted'}`} title={text}>{text}</div>
}

function AwaitingLine({ d }: { d: QueueItem }) {
  const text = awaitingText(d.stateEnteredAt)
  return <div className="queue-row-detail muted" title={text}>{text}</div>
}


const OUTCOME_PILL: Record<string, string> = {
  placed: 'pill-ok',
  discarded: 'pill-neutral',
  unresolved: 'pill-warning',
  pending: 'pill-neutral',
}

// Publications are the import unit: one per Runner generation, each file with
// what became of it. An unresolved file is retained and decided here.
export function PublicationList({ d, readOnly = false }: { d: QueueItem; readOnly?: boolean }) {
  const qc = useQueryClient()
  const [done, setDone] = useState<Record<string, string>>({})
  // The decided file's buttons go away with its outcome, so the result
  // takes keyboard focus (CLAUDE.md section 5) instead of leaving it on a
  // removed button.
  const [focusKey, setFocusKey] = useState<string | null>(null)
  const results = useRef<Record<string, HTMLSpanElement | null>>({})
  useEffect(() => {
    if (focusKey) results.current[focusKey]?.focus()
  }, [focusKey, done])
  const act = useMutation({
    mutationFn: (v: { pub: number; path: string; action: PublicationFileAction }) => resolvePublicationFile(d.id, v.pub, v.path, v.action),
    onSuccess: (_r, v) => {
      const key = `${v.pub}/${v.path}`
      setDone((prev) => ({ ...prev, [key]: v.action === 'place' ? 'queued for import' : v.action === 'discard' ? 'Marked for cleanup' : 'kept' }))
      setFocusKey(key)
      void qc.invalidateQueries({ queryKey: ['activity'] }); void qc.invalidateQueries({ queryKey: ['queue'] })
      void qc.invalidateQueries({ queryKey: ['imports'] })
    },
    onError: (e, v) => {
      const key = `${v.pub}/${v.path}`
      setDone((prev) => ({ ...prev, [key]: e instanceof Error ? e.message : String(e) }))
      setFocusKey(key)
    },
  })
  const pubs = d.publications ?? []
  if (pubs.length === 0) return null
  return (
    <div className="handoff-publications">
      {pubs.map((p) => (
        <div key={p.id} className="handoff-publication" data-publication={p.id}>
          <p>
            <b>{p.kind === 'salvage' ? 'Salvage publication' : 'Publication'}</b>{' '}
            <span className={`pill ${p.state === 'import_failed' ? 'pill-error' : p.state === 'imported' || p.state === 'notified' ? 'pill-ok' : 'pill-neutral'}`}>{p.state.replace(/_/g, ' ')}</span>{' '}
            {p.cohort === 'historical' && <span className="pill pill-neutral" title="Frozen at the pipeline v2 cutover: never deleted automatically until its review batch is approved">{p.cleanupApprovedAt ? 'historical · cleanup approved' : 'historical'}</span>}{' '}
            <span className="path-chip">{p.path}</span>
          </p>
          {p.held ? (
            <p className="muted" data-held>
              <span className="pill pill-warning">held</span> {publicationHoldText(p)} — it imports by itself once that clears.
            </p>
          ) : (
            p.error && <p className="error-text">{p.error}</p>
          )}
          {!readOnly && <PublicationCleanupLine pub={p} />}
          <ul className="publication-files">
            {p.files.map((f) => {
              const key = `${p.id}/${f.path}`
              return (
                <li key={f.path} data-file={f.path}>
                  <span className={`pill ${OUTCOME_PILL[f.outcome] ?? 'pill-neutral'}`}>{f.outcome}{f.discardReason ? ` · ${f.discardReason}` : ''}</span>{' '}
                  <span className="mono">{f.path}</span>
                  {f.libraryPath && <span className="muted"> → {f.libraryPath}</span>}
                  {f.reason && <span className="muted"> — {f.reason}</span>}
                  {f.removed && <span className="muted"> · removed from the publication</span>}
                  {!readOnly && (f.outcome === 'unresolved' || f.outcome === 'pending') && (
                    <span className="row-actions publication-file-actions">
                      {(['place', 'discard', 'keep'] as PublicationFileAction[]).map((a) => (
                        <button key={a} disabled={act.isPending} onClick={() => act.mutate({ pub: p.id, path: f.path, action: a })}>
                          {a === 'place' ? 'Place' : a === 'discard' ? 'Discard' : 'Keep'}
                        </button>
                      ))}
                    </span>
                  )}
                  {done[key] && <span role="status" tabIndex={-1} ref={(el) => { results.current[key] = el }} className="muted publication-file-result"> {done[key]}</span>}
                </li>
              )
            })}
          </ul>
        </div>
      ))}
    </div>
  )
}

// What the per-file cleanup is doing to a publication, why, and a way to
// stop it (Paul's rule for anything that spends disk time on his server).
function PublicationCleanupLine({ pub }: { pub: QueuePublication }) {
  const qc = useQueryClient()
  const [note, setNote] = useState('')
  const act = useMutation({
    mutationFn: (action: 'pause' | 'resume') => setPublicationCleanup(pub.id, action),
    onSuccess: (_r, action) => { setNote(action === 'pause' ? 'Cleanup paused' : 'Cleanup resumed and queued'); void qc.invalidateQueries({ queryKey: ['activity'] }); void qc.invalidateQueries({ queryKey: ['queue'] })
      void qc.invalidateQueries({ queryKey: ['imports'] }) },
    onError: (e) => setNote(e instanceof Error ? e.message : String(e)),
  })
  if (pub.state === 'removed') return null
  const paused = !!pub.cleanupPausedAt
  const running = pub.cleanupState
  if (!paused && !running) return null
  return (
    <p className="muted publication-cleanup" data-cleanup={paused ? 'paused' : running}>
      {paused ? 'Automatic cleanup paused.' : `Cleanup ${running === 'running' ? 'running' : 'queued'}${pub.cleanupReason ? ` (${pub.cleanupReason})` : ''}: verifying files against the manifest before removing them.`}{' '}
      <button disabled={act.isPending} onClick={() => act.mutate(paused ? 'resume' : 'pause')}>{paused ? 'Resume cleanup' : 'Pause cleanup'}</button>
      {note && <span role="status"> {note}</span>}
    </p>
  )
}

function HandoffDetail({ d, readOnly = false }: { d: QueueItem; readOnly?: boolean }) {
  const steps = d.handoff ?? []
  return (
    <div className="handoff">
      <PublicationList d={d} readOnly={readOnly} />

      {d.error && <p className="error-text"><strong>Latest error:</strong> {d.error}</p>}
      {(d.savePath || d.importPath) && (
        <div className="handoff-paths">
          {d.savePath && (
            <div>
              <span className="muted">Client reported</span>{' '}
              <span className="path-chip">{d.savePath}</span>
            </div>
          )}
          {d.importPath && d.importPath !== d.savePath && (
            <div>
              <span className="muted">Curator looked in</span>{' '}
              <span className="path-chip">{d.importPath}</span>
            </div>
          )}
        </div>
      )}
      {steps.length === 0 ? (
        <p className="muted">No handoff steps recorded yet.</p>
      ) : (
        <ol className="handoff-trace">
          {steps.map((h, i) => (
            <li key={i} className="handoff-step" data-step={h.step}>
              <span className="handoff-dot" />
              <span className="handoff-when mono">{clock(h.at)}</span>
              <span className="handoff-what">
                <b>{label(STEP_LABEL, h.step)}</b>
                {h.detail ? ` — ${h.detail}` : ''}
              </span>
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

export function ManualImportPanel({
  prefill,
  onDone,
}: {
  prefill: Partial<ManualImportRequest>
  onDone: () => void
}) {
  // The API learns the completed-download folder from path mappings or recent
  // rows. Queue-row manual imports still win with their exact recorded path.
  const [path, setPath] = useState(prefill.path ?? '')
  const [itemId, setItemId] = useState<number>(prefill.mediaItemId ?? 0)
  const [copyId, setCopyId] = useState<number>(prefill.copyId ?? 0)
  const [scanned, setScanned] = useState<ScannedFile[] | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [scanErr, setScanErr] = useState<string>('')
  const [importErr, setImportErr] = useState<string>('')

  const library = useQuery({ queryKey: ['library'], queryFn: () => getLibrary() })
  const defaultPath = useQuery({
    queryKey: ['manual-import-default-path'],
    queryFn: getManualImportDefaultPath,
    enabled: !prefill.path,
  })
  const shownPath = path || defaultPath.data?.path || ''
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
        downloadId: prefill.downloadId,
        reviewLegacy: prefill.reviewLegacy,
        reviewLegacyFingerprint: prefill.reviewLegacy ? prefill.reviewLegacyFingerprint : undefined,
        reviewLegacyFiles: prefill.reviewLegacy ? (scanned ?? []).filter(file => selected.has(file.path)).map(({ path, snapshotIdentity }) => ({ path, snapshotIdentity: snapshotIdentity ?? '' })) : undefined,
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
      <h2>Manual import</h2>
      <p className="muted" style={{ fontSize: 13 }}>
        Point Curator at a folder or file, choose where it belongs, and import it — for a download
        Curator couldn't place automatically, or files you moved into place yourself.
      </p>
      {prefill.reviewLegacy && <p className="warning-text">Legacy review approval is tied to this inventory snapshot, destination, and exact selected files. If the files or payload change, refresh the backlog and review again.</p>}

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

export function ImportReviewPanel({id,readOnly=false}:{id:number;readOnly?:boolean}) {
  const qc=useQueryClient()
  const [ackSnapshot,setAckSnapshot]=useState<string|null>(null)
  const review=useQuery({queryKey:['import-review',id],queryFn:()=>getImportReview(id),refetchInterval:4000})
  const snapshot=review.data ? JSON.stringify([review.data.attempt,review.data.publications,review.data.stages]) : ''
  const ack=!!snapshot && ackSnapshot===snapshot
  const action=useMutation({mutationFn:({action,ids}:{action:string;ids:string[]})=>{if(readOnly||!ack||review.isError||review.data?.writerActive)throw new Error('Review and acknowledge the current exact records before acting.');return resolveImportReview(id,review.data!.attempt,action,ids,ack)},onSuccess:()=>{setAckSnapshot(null);void qc.invalidateQueries({queryKey:['import-review',id]});void qc.invalidateQueries({queryKey:['activity']});void qc.invalidateQueries({queryKey:['queue']});void qc.invalidateQueries({queryKey:['imports']})}})
  const data=review.data
  if(review.isLoading)return <p role="status">Loading durable import custody…</p>
  if(review.error)return <p role="alert">{review.error.message}</p>
  if(!data)return null
  const pending=data.publications.filter(p=>p.outcome==='pending')
  const retained=data.publications.filter(p=>p.resolution==='retained_for_review'||p.resolution==='operator_adopted')
  const stages=data.stages??[]
  const disabled=readOnly||!ack||action.isPending||data.writerActive
  return <>
    <p>Attempt {data.attempt.epoch}: {data.attempt.outcome||'active'}. {data.writerActive?'An importer still owns this work. Wait for its actual exit.':'No local importer ticket is active.'}</p>
    <p>These records describe retained custody. They do not prove whether a target was published. Review the source, stage and Library before disposition.</p>
    <ul>{data.publications.filter(p=>p.outcome==='pending'||p.resolution).map(p=><li key={p.id}><strong>{p.outcome} {p.resolution}</strong><p className="path-chip">{p.source} → {p.target}</p></li>)}{stages.map(p=><li key={p.id}>Retained stage <span className="path-chip">{p.stage}</span><p>{p.source} → {p.target}</p></li>)}</ul>
    <label><input type="checkbox" checked={ack} onChange={e=>setAckSnapshot(e.target.checked?snapshot:null)}/>I have fenced every external writer and reviewed these exact records. For disposition, I have independently handled all listed bytes; for adoption, I have verified the current Library ownership. Curator will retain files and perform no deletion.</label>
    {action.error&&<p role="alert">{action.error.message}</p>}
    {action.isSuccess&&<p role="status">Review recorded. Retained filesystem bytes were left in place.</p>}
    {pending.length>0&&<button disabled={disabled} onClick={()=>action.mutate({action:'retain_uncertain',ids:pending.map(p=>p.id)})}>Finalize cancellation and retain uncertain bytes</button>}
    {retained.length>0&&<button disabled={disabled} onClick={()=>action.mutate({action:'operator_adopted',ids:retained.map(p=>p.id)})}>Record verified Library adoption</button>}
    {(retained.length>0||stages.length>0)&&<button disabled={disabled} onClick={()=>action.mutate({action:'operator_disposed',ids:[...retained.map(p=>p.id),...stages.map(p=>p.id)]})}>Attest all listed bytes independently handled</button>}
  </>
}
