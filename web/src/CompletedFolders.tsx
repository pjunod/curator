import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { deleteCompletedEntry, fmtBytes, getCompletedInventory, getSettings, getTasks, runTask, updateSettings } from './api'
import type { CompletedEntry, CompletedRoot } from './api'
import { clampPage, Pager, PageSizePicker, sliceForPage } from './Pager'
import './CompletedFolders.css'

type StorageRow = { root: CompletedRoot; entry: CompletedEntry }
const rowKey = ({ root, entry }: StorageRow) => JSON.stringify([root.path, entry.path])
const canDelete = ({ root, entry }: StorageRow) => !root.error && !['active', 'awaiting_import', 'storage'].includes(entry.status)
const canImport = ({ root, entry }: StorageRow) => !root.error && !['active', 'symlink', 'retained', 'storage'].includes(entry.status)
const statusLabel = (status: string) => status.replace(/_/g, ' ')

export function CompletedFolders({ onImport }: { onImport: (path: string) => void }) {
  const qc = useQueryClient()
  const inventory = useQuery({
    queryKey: ['completed-inventory'], queryFn: getCompletedInventory, refetchInterval: 5_000,
  })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 2_000 })
  const [requestedScan, setRequestedScan] = useState<string | null>(null)
  const scanTask = tasks.data?.find((task) => task.name === 'downloads.inventory')
  const scanWaiting = requestedScan !== null && (scanTask?.lastRunAt ?? '') === requestedScan
  const scanning = scanTask?.running || scanWaiting
  const scan = useMutation({
    mutationFn: () => runTask('downloads.inventory'),
    onMutate: () => setRequestedScan(scanTask?.lastRunAt ?? ''),
    onError: () => setRequestedScan(null),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ['tasks'] }) },
  })
  const cleanup = useMutation({ mutationFn: () => runTask('downloads.cleanup') })
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState('all')
  const [sort, setSort] = useState('name')
  const [size, setSize] = useState(25)
  const [page, setPage] = useState(0)
  // Remember the reviewed fingerprint, so refreshes cannot silently select changed files.
  const [selected, setSelected] = useState<Record<string, string>>({})
  const [confirm, setConfirm] = useState<StorageRow[] | null>(null)
  const [result, setResult] = useState<{ deleted: number; errors: string[] } | null>(null)
  const [progress, setProgress] = useState(0)
  const data = inventory.data
  const entries = data?.roots.flatMap((root) => root.entries.map((entry) => ({ root, entry }))) ?? []
  const selectedRows = entries.filter((row) => canDelete(row) && selected[rowKey(row)] === row.entry.fingerprint)
  const isSelected = (row: StorageRow) => canDelete(row) && selected[rowKey(row)] === row.entry.fingerprint
  const remove = useMutation({
    mutationFn: async (rows: StorageRow[]) => {
      const errors: string[] = []
      let deleted = 0
      setResult(null)
      setProgress(0)
      // Reuse the server's per-entry ownership and fingerprint checks. Serial
      // requests avoid flooding storage and give every entry its own outcome.
      for (const [index, row] of rows.entries()) {
        try {
          await deleteCompletedEntry(row.entry.path, row.entry.fingerprint)
          deleted++
          setSelected((previous) => {
            const next = { ...previous }
            delete next[rowKey(row)]
            return next
          })
        } catch (error) {
          errors.push(`${row.entry.path}: ${error instanceof Error ? error.message : 'Deletion failed'}`)
        }
        setProgress(index + 1)
      }
      return { deleted, errors }
    },
    onSuccess: (outcome) => {
      setResult(outcome)
      setConfirm(null)
      void qc.invalidateQueries({ queryKey: ['completed-inventory'] })
    },
  })
  const needle = search.trim().toLowerCase()
  const filtered = entries.filter(({ entry }) =>
    (status === 'all' || (status === 'attention' ? !['active', 'retained', 'storage'].includes(entry.status) : entry.status === status)) &&
    (!needle || [entry.path, entry.reason, ...entry.receipts.map((r) => r.title)].some((text) => text.toLowerCase().includes(needle))),
  ).sort((a, b) => sort === 'size' ? b.entry.bytes - a.entry.bytes || a.entry.path.localeCompare(b.entry.path) : a.entry.path.localeCompare(b.entry.path))
  const currentPage = clampPage(page, filtered.length, size)
  const visible = sliceForPage(filtered, currentPage, size)
  const eligible = filtered.filter(canDelete)
  const pageEligible = visible.filter(canDelete)
  const allPageSelected = pageEligible.length > 0 && pageEligible.every(isSelected)
  const togglePage = () => setSelected((previous) => {
    const next = { ...previous }
    for (const row of pageEligible) {
      if (allPageSelected) delete next[rowKey(row)]
      else next[rowKey(row)] = row.entry.fingerprint
    }
    return next
  })
  const resetView = () => { setPage(0); setSelected({}); setConfirm(null) }
  const selectedBytes = selectedRows.reduce((sum, { entry }) => sum + entry.bytes, 0)
  const confirmBytes = confirm?.reduce((sum, { entry }) => sum + entry.bytes, 0) ?? 0
  const confirmFiles = confirm?.reduce((sum, { entry }) => sum + entry.files, 0) ?? 0
  const confirmationChanged = confirm?.some((row) => !entries.some((current) => rowKey(current) === rowKey(row) && canDelete(current) && current.entry.fingerprint === row.entry.fingerprint))

  return <section className="panel download-storage" aria-label="Download storage">
    <div className="storage-heading">
      <div><h2>Download storage</h2><p className="muted">Review leftover downloads and manage files on disk.</p></div>
      <div className="storage-actions">
        <button disabled={scan.isPending || !!scanning || remove.isPending} onClick={() => scan.mutate()}>{scanning ? 'Scanning storage…' : 'Scan download storage'}</button>
        <button disabled={cleanup.isPending || remove.isPending} onClick={() => cleanup.mutate()}>Retry imported cleanup</button>
      </div>
    </div>
    <div className="storage-stats">
      <div><strong>{data ? fmtBytes(data.bytes) : '…'}</strong><span>Logical size on disk</span></div>
      <div><strong>{entries.length}</strong><span>Entries accounted for</span></div>
      <div><strong>{data?.attention ?? '…'}</strong><span>Need attention</span></div>
    </div>
    <p className="muted storage-note">Logical file sizes; actual space freed may differ. Scanned every 5 minutes.</p>
    {scanning && <p role="status">Storage scan {scanTask?.running ? 'in progress' : 'queued'}… Previous results remain visible until it finishes.</p>}
    {!scanning && requestedScan !== null && scanTask && <p role="status" className={scanTask.lastError ? 'error-text' : 'muted'}>
      {scanTask.lastError ? `Scan failed: ${scanTask.lastError}` : 'Storage scan complete.'}
    </p>}
    {cleanup.isSuccess && <p role="status" className="muted">Imported cleanup queued. A subsequent inventory scan will show the result.</p>}
    {(inventory.error || scan.error || cleanup.error) && <p role="alert" className="error-text">{(inventory.error || scan.error || cleanup.error)?.message}</p>}
    {data?.roots.length === 0 && <p className="muted">No scan yet. Configure download storage folders in Settings if needed.</p>}
    <div className="storage-roots">{data?.roots.map((root) => <div key={root.path}>
      <div className="storage-root-line"><span className="mono">{root.path}</span><span>{fmtBytes(root.bytes)}</span><span className="muted">
        {root.lastCompleteAt.startsWith('0001') ? 'No complete scan' : `Last complete scan: ${new Date(root.lastCompleteAt).toLocaleString()}`}
      </span></div>
      {root.error && <p className="error-text" role="alert">Scan incomplete: {root.error}. Showing stale or partial results. Scan again before managing these files.</p>}
    </div>)}</div>
    {result && <div role="status" className={result.errors.length ? 'error-text' : 'muted'}>
      {result.deleted} {result.deleted === 1 ? 'entry' : 'entries'} deleted.{result.errors.length > 0 && <> {result.errors.length} failed; those files were not confirmed deleted.
        <ul>{result.errors.map((error) => <li key={error}>{error}</li>)}</ul></>}
    </div>}
    {confirm && <div className="storage-confirm" role="region" aria-label="Confirm storage deletion">
      <h3>Delete {confirm.length} {confirm.length === 1 ? 'entry' : 'entries'} permanently?</h3>
      <p>{confirmFiles} files · {fmtBytes(confirmBytes)} logical size. This cannot be undone.</p>
      <ul>{confirm.map(({ root, entry }) => <li key={`${root.path}:${entry.path}`} className="mono">{entry.path}</li>)}</ul>
      {confirmationChanged && <p className="error-text">The inventory changed. Cancel and select the entries again.</p>}
      <div className="storage-actions"><button className="btn-danger" disabled={remove.isPending || confirmationChanged} onClick={() => remove.mutate(confirm)}>
        {remove.isPending ? `Deleting ${progress} of ${confirm.length}…` : 'Confirm delete'}
      </button><button disabled={remove.isPending} onClick={() => setConfirm(null)}>Keep files</button></div>
    </div>}
    {entries.length > 0 && <>
      <div className="storage-toolbar">
        <input type="search" aria-label="Search storage" placeholder="Search paths or downloads…" value={search} disabled={remove.isPending} onChange={(e) => { setSearch(e.target.value); resetView() }} />
        <label>Status <select aria-label="Storage status" value={status} disabled={remove.isPending} onChange={(e) => { setStatus(e.target.value); resetView() }}>
          <option value="all">All statuses</option><option value="attention">Needs attention</option>
          {[...new Set(entries.map(({ entry }) => entry.status))].sort().map((value) => <option key={value} value={value}>{statusLabel(value)}</option>)}
        </select></label>
        <label>Sort <select aria-label="Sort storage" value={sort} onChange={(e) => { setSort(e.target.value); setPage(0) }}><option value="name">Name</option><option value="size">Largest first</option></select></label>
        <PageSizePicker size={size} onChange={(value) => { setSize(value); setPage(0) }} />
      </div>
      <div className="storage-selection">
        <label><input type="checkbox" aria-label="Select page" disabled={!pageEligible.length || remove.isPending} checked={allPageSelected}
          ref={(node) => { if (node) node.indeterminate = !allPageSelected && pageEligible.some(isSelected) }} onChange={togglePage} /> Select page</label>
        <span className="muted">{selectedRows.length} selected · {fmtBytes(selectedBytes)}</span>
        <button disabled={!eligible.length || remove.isPending} onClick={() => setSelected(Object.fromEntries(eligible.map((row) => [rowKey(row), row.entry.fingerprint])))}>Select all {eligible.length} eligible</button>
        <button disabled={!selectedRows.length || remove.isPending} onClick={() => { setSelected({}); setConfirm(null) }}>Clear selection</button>
        <button className="btn-danger" disabled={!selectedRows.length || remove.isPending} onClick={() => setConfirm(selectedRows)}>Delete selected…</button>
      </div>
      <p className="muted storage-note">Select all includes matching entries on every page. Active downloads, awaiting imports, storage folders, and incomplete scans cannot be selected.</p>
      <div className="storage-pagination storage-pagination-top"><Pager page={currentPage} size={size} total={filtered.length} onPage={setPage} /></div>
      <div className="storage-list" role="list" aria-label="Storage entries">
        {visible.map((row) => {
          const { root, entry } = row
          const name = entry.path.split('/').filter(Boolean).pop() ?? entry.path
          return <article className={`storage-entry${isSelected(row) ? ' is-selected' : ''}`} role="listitem" key={rowKey(row)}>
            <input type="checkbox" aria-label={`Select ${name}`} disabled={!canDelete(row) || remove.isPending} checked={isSelected(row)} onChange={() => setSelected((previous) => {
              const next = { ...previous }
              if (isSelected(row)) delete next[rowKey(row)]
              else next[rowKey(row)] = entry.fingerprint
              return next
            })} />
            <div className="storage-entry-name"><strong title={entry.path}>{name}</strong><div className="mono muted storage-path" title={entry.path}>{entry.path.slice(0, -name.length)}</div>
              {entry.receipts.length > 0 && <details><summary>{entry.receipts.length} download {entry.receipts.length === 1 ? 'record' : 'records'}</summary>
                {entry.receipts.map((r, i) => <div key={`${r.downloadId}:${i}`}>#{r.downloadId} {r.title}{!r.live && ' (history only)'}</div>)}
              </details>}
            </div>
            <div className="storage-entry-size"><strong>{fmtBytes(entry.bytes)}</strong><span className="muted">{entry.files} {entry.files === 1 ? 'file' : 'files'}</span></div>
            <div className="storage-entry-status"><span className={`pill ${entry.status === 'failed' ? 'pill-error' : ''}`}>{statusLabel(entry.status)}</span><p className="muted">{entry.reason}</p></div>
            <div className="storage-entry-actions">
              <button disabled={!canImport(row) || remove.isPending} onClick={() => onImport(entry.path)}>Review import</button>
              <button disabled={!canDelete(row) || remove.isPending} title={root.error ? 'A complete scan is required' : undefined} onClick={() => setConfirm([row])}>Delete files…</button>
            </div>
          </article>
        })}
      </div>
      {filtered.length === 0 && <p className="muted">No entries match your search or status filter.</p>}
      <div className="storage-pagination"><Pager page={currentPage} size={size} total={filtered.length} onPage={setPage} /></div>
    </>}
  </section>
}

export function CompletedFolderSettings() {
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const qc = useQueryClient()
  const [value, setValue] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => updateSettings({ completedRoots: value ?? settings.data?.completedRoots ?? '' }),
    onSuccess: () => { setValue(null); void qc.invalidateQueries({ queryKey: ['settings'] }) },
  })
  return <>
    <h3 style={{ marginTop: 18 }}>Download storage folders</h3>
    <p className="muted">Curator-owned working folders, one absolute path per line as seen by the server.
      Every file beneath these roots is accounted for, including completed downloads.
      Leave blank to use /working/monarr when mounted, otherwise discover download folders. Unknown files remain visible for review. Saving these folders also accepts their current mount identities after a mount replacement.</p>
    <textarea rows={3} className="mono" aria-label="Download storage folders"
      value={value ?? settings.data?.completedRoots ?? ''} onChange={(e) => setValue(e.target.value)}
      placeholder="/working/monarr" />
    <button disabled={settings.isPending || save.isPending} onClick={() => save.mutate()}>Save download storage</button>
    {save.isSuccess && <p className="muted">Saved — applies on the next inventory scan.</p>}
    {save.error && <p className="error-text">{save.error.message}</p>}
  </>
}
