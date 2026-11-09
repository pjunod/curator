import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ActionDialog, focusActionTrigger } from '../ActionDialog'
import { cancelQueueImport, fmtBytes, fmtRelative, getImportActivity, importQueueItem, type ImportActivityItem, type ImportCategory } from '../api'
import { canRetryImport, importActivityLabel, importNeedsReview, importProgress } from '../importActivity'
import { ImportReviewPanel, PublicationList, ResolvePanel } from './Activity'
import '../ImportQueue.css'

const FILTERS = [
  ['all', 'All'], ['active', 'Importing'], ['queued', 'Queued'],
  ['attention', 'Needs attention'], ['imported', 'Imported'],
] as const
const GROUPS = [ ['active', 'Importing now'], ['queued', 'Waiting to import'],
  ['attention', 'Needs attention'], ['imported', 'Imported'] ] as const
const PAGE = 50

type ActionResult = { id: number; title: string; message: string; failed: boolean }

export function ImportQueuePage() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState<'all' | ImportCategory>('all')
  const [search, setSearch] = useState('')
  const [offset, setOffset] = useState(0)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [review, setReview] = useState<ImportActivityItem | null>(null)
  const [resolve, setResolve] = useState<ImportActivityItem | null>(null)
  const [results, setResults] = useState<ActionResult[] | null>(null)
  const [busy, setBusy] = useState(false)
  const opener = useRef<HTMLElement | null>(null)
  const query = useQuery({ queryKey: ['imports', filter, search, offset],
    queryFn: () => getImportActivity(filter, search, PAGE, offset), refetchInterval: 4000 })
  useEffect(() => {
    if (query.data && offset > 0 && offset >= query.data.total) {
      setOffset(Math.max(0, Math.ceil(query.data.total / PAGE) - 1) * PAGE)
      setSelected(new Set())
    }
  }, [query.data, offset])
  const data = query.data
  const items = data?.items ?? []
  const retryable = items.filter(canRetryImport)
  const selectedItems = retryable.filter(i => selected.has(i.download.id))
  const selectFilter = (f: typeof filter) => { setFilter(f); setOffset(0); setSelected(new Set()) }
  const remember = (event: React.SyntheticEvent<HTMLElement>) => {
    focusActionTrigger(event); opener.current = event.currentTarget
  }
  const invalidate = async () => {
    await Promise.all([qc.invalidateQueries({ queryKey: ['imports'] }),
      qc.invalidateQueries({ queryKey: ['queue'] }), qc.invalidateQueries({ queryKey: ['queue-summary'] })])
  }
  const act = async (rows: ImportActivityItem[], action: 'retry' | 'cancel') => {
    setBusy(true); setResults([])
    const out: ActionResult[] = []
    // Bounded, serial retries preserve one outcome per selected job and avoid
    // flooding the two import workers or starting replacement downloads.
    for (const item of rows) {
      const d = item.download
      try {
        if (action === 'cancel') {
          if (!d.importControl) throw new Error('Refresh before cancelling this import.')
          await cancelQueueImport(d.id, d.importControl)
        } else await importQueueItem(d.id)
        out.push({ id: d.id, title: item.title, failed: false,
          message: action === 'cancel' ? 'Cancellation requested. Files remain protected until the importer has stopped.' : 'Import retry accepted. Follow its current status in the queue.' })
      } catch (e) {
        out.push({ id: d.id, title: item.title, failed: true, message: e instanceof Error ? e.message : String(e) })
      }
      setResults([...out])
    }
    setSelected(new Set()); setBusy(false); await invalidate()
  }

  return <div className="import-queue">
    <header className="page-head"><div><h1>Import queue</h1><p className="muted">Every recorded import, from completed download to library.</p></div><Link to="/activity">Download activity</Link></header>
    {data && data.counts.attention > 0 && <div className="import-attention-banner"><div><strong>{data.counts.attention} import{data.counts.attention === 1 ? '' : 's'} need attention</strong><p className="muted">Review failed, interrupted, and unresolved work before retrying.</p></div><button onClick={() => selectFilter('attention')}>Review imports</button></div>}
    <nav className="import-filters" aria-label="Import status filters">{FILTERS.map(([key, label]) => <button key={key} aria-pressed={filter === key} onClick={() => selectFilter(key)}>{label} <span>{data?.counts[key] ?? '…'}</span></button>)}</nav>
    <div className="import-toolbar"><label>Filter imports<input type="search" value={search} onChange={e => { setSearch(e.target.value); setOffset(0); setSelected(new Set()) }} placeholder="Title or release name" /></label><span className="muted small">{query.isFetching ? 'Updating…' : query.dataUpdatedAt ? `Updated ${new Date(query.dataUpdatedAt).toLocaleTimeString()}` : ''}</span></div>
    {query.isLoading && <p role="status">Loading imports…</p>}
    {query.error && <p role="alert" className="error-text">Could not refresh imports: {query.error.message} <button onClick={() => void query.refetch()}>Try again</button></p>}
    {data && data.total === 0 && !query.error && <p className="empty">{search ? 'No imports match this search.' : filter === 'all' ? 'No import activity recorded yet. Completed downloads and manual imports will appear here.' : 'No imports in this view.'}</p>}
    {GROUPS.map(([key, label]) => {
      const rows = items.filter(i => i.category === key)
      if (!rows.length) return null
      return <section className="import-section" key={key} aria-label={label}>
        <div className="import-section-head"><h2>{label} <span className="muted">{data?.counts[key]}</span></h2>{key === 'attention' && retryable.length > 0 && <div className="row-actions"><label className="import-selection"><input type="checkbox" aria-label="Select retryable imports on this page" checked={retryable.length > 0 && selectedItems.length === retryable.length} onChange={e => setSelected(new Set(e.target.checked ? retryable.map(i => i.download.id) : []))} />Select page</label><button disabled={busy || selectedItems.length === 0} onClick={e => { remember(e); void act(selectedItems, 'retry') }}>Retry selected ({selectedItems.length})</button></div>}</div>
        {rows.map(item => <ImportRow key={item.download.id} item={item} busy={busy} selected={selected.has(item.download.id)} onSelect={checked => setSelected(prev => { const next = new Set(prev); if (checked) next.add(item.download.id); else next.delete(item.download.id); return next })}
          onRetry={e => { remember(e); void act([item], 'retry') }} onCancel={e => { remember(e); void act([item], 'cancel') }}
          onReview={e => { remember(e); setReview(item) }} onResolve={e => { remember(e); setResolve(item) }} />)}
      </section>
    })}
    {data && <footer className="import-footer"><div className="row-actions"><button disabled={offset === 0} onClick={() => { setOffset(Math.max(0, offset - PAGE)); setSelected(new Set()) }}>Previous</button><span>{data.total ? `${offset + 1}–${Math.min(offset + PAGE, data.total)} of ${data.total}` : '0 imports'}</span><button disabled={!data.hasMore} onClick={() => { setOffset(offset + PAGE); setSelected(new Set()) }}>Next</button></div><p className="muted small">{data.retentionDays === 0 ? 'All retained import history · automatic expiry disabled.' : `History follows Activity retention: ${data.retentionDays} days. Expired records cannot be reconstructed.`} <Link to="/settings" hash="library">Retention settings</Link></p></footer>}
    {results && <ActionDialog title="Import action results" returnFocus={opener} onClose={() => { if (!busy) setResults(null) }}><div aria-live="polite">{busy && <p role="status">Submitting imports… {results.length} completed</p>}{results.map(r => <div key={r.id} className="import-action-result"><strong>{r.title}</strong><p className={r.failed ? 'error-text' : 'muted'}>{r.message}</p></div>)}</div></ActionDialog>}
    {review && <ActionDialog title="Review retained import" returnFocus={opener} onClose={() => { setReview(null); void invalidate() }}><ImportReviewPanel id={review.download.id} /></ActionDialog>}
    {resolve && <ActionDialog title="Resolve import files" returnFocus={opener} onClose={() => { setResolve(null); void invalidate() }}><ResolvePanel embedded prefill={{ downloadId: resolve.download.id, mediaItemId: resolve.download.mediaItemId, copyId: resolve.download.copyId, path: resolve.download.importPath || resolve.download.savePath }} onDone={() => { setResolve(null); void invalidate() }} /></ActionDialog>}
  </div>
}

function ImportRow({ item, busy, selected, onSelect, onRetry, onCancel, onReview, onResolve }: {
  item: ImportActivityItem; busy: boolean; selected: boolean; onSelect: (checked: boolean) => void
  onRetry: (e: React.SyntheticEvent<HTMLElement>) => void; onCancel: (e: React.SyntheticEvent<HTMLElement>) => void
  onReview: (e: React.SyntheticEvent<HTMLElement>) => void; onResolve: (e: React.SyntheticEvent<HTMLElement>) => void
}) {
  const d = item.download
  const progress = importProgress(item)
  const files = (d.publications ?? []).flatMap(p => p.files)
  const placed = files.filter(f => f.outcome === 'placed').length
  const unresolved = files.filter(f => f.outcome === 'unresolved').length
  const label = importActivityLabel(item)
  const custody = importNeedsReview(item)
  const canCancel = !!d.importState && !!d.importControl && d.importControl.outcome === ''
  const ended = [...(d.handoff ?? [])].reverse().find(h => h.step === 'imported')
  return <article className="import-row" data-import-id={d.id}>
    <div className="import-row-top"><div className="import-row-title">{canRetryImport(item) && <input type="checkbox" aria-label={`Select ${item.title}`} checked={selected} onChange={e => onSelect(e.target.checked)} />}<div><h3>{d.mediaItemId > 0 ? <Link to="/library/$id" params={{ id: String(d.mediaItemId) }}>{item.title}</Link> : item.title}</h3><p className="import-release muted small">{d.title}</p><p className="muted small">{[item.kind, d.quality, d.id < 0 ? 'Historical import · read only' : d.protocol === 'manual' ? 'Manual import' : 'Automatic import'].filter(Boolean).join(' · ')}</p></div></div><span className={`pill ${item.category === 'imported' ? 'pill-ok' : item.category === 'attention' ? 'pill-warning' : 'pill-neutral'}`}>{item.category === 'active' ? 'Importing' : item.category === 'queued' ? 'Waiting' : item.category === 'imported' ? 'Imported' : 'Needs attention'}</span></div>
    <p className={item.category === 'attention' ? 'error-text small' : 'muted small'}>{label}</p>
    {progress && <><div className="progress-track" role="progressbar" aria-label={`${item.title} import progress`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress.percent}><div className="progress-fill" style={{ width: `${progress.percent}%` }} /></div><div className="import-measurements small"><span>{fmtBytes(progress.bytes)} / {fmtBytes(progress.total)} · {progress.percent}%</span><span className="muted">{progress.rate ? `${fmtBytes(progress.rate)}/s` : 'Rate not available'}{progress.secondsLeft !== null ? ` · about ${Math.max(1, Math.ceil(progress.secondsLeft / 60))}m left` : ''}</span></div></>}
    {item.category === 'active' && !progress && <p className="muted small">{d.stageSince ? `Started ${fmtRelative(d.stageSince)} · ` : ''}Byte progress is not available for this step.</p>}
    <div className="import-row-bottom"><span className="muted small">{files.length > 0 ? `${placed} placed · ${files.length} recorded files${unresolved ? ` · ${unresolved} unresolved` : ''} · ` : ''}{item.category === 'imported' && ended ? `Imported ${new Date(ended.at).toLocaleString()}` : `Updated ${fmtRelative(item.updatedAt)}`}</span><div className="row-actions">
      {canRetryImport(item) && <button disabled={busy} onClick={onRetry}>{d.state === 'awaiting_import' ? 'Approve import' : 'Retry import'}</button>}
      {canCancel && <button disabled={busy} onClick={onCancel}>Cancel import</button>}
      {(custody || d.importControl?.outcome === 'pending' || d.importControl?.outcome === 'cancelled') && <button disabled={busy} onClick={onReview}>Review retained import</button>}
      {item.category === 'attention' && d.id > 0 && !d.importState && <button disabled={busy} onClick={onResolve}>Resolve files</button>}
    </div></div>
    <details><summary>Files &amp; import details</summary><div className="import-details"><dl><dt>Release</dt><dd>{d.title}</dd><dt>Source</dt><dd>{d.importPath || d.savePath || 'Not recorded'}</dd><dt>Destination</dt><dd>{item.destination || 'Not configured'}</dd><dt>Import attempt</dt><dd>{d.importControl ? `${d.importControl.epoch} · ${d.importControl.outcome || 'open'}` : 'Not recorded'}</dd></dl><PublicationList d={d} readOnly={d.id < 0} />{(d.handoff?.length ?? 0) > 0 && <details><summary>Import history</summary><ol className="import-timeline">{d.handoff?.map((h, i) => <li key={`${h.at}-${i}`}><time>{new Date(h.at).toLocaleString()}</time> <strong>{h.step.replace(/_/g, ' ')}</strong>{h.detail && <p>{h.detail}</p>}</li>)}</ol></details>}</div></details>
  </article>
}
