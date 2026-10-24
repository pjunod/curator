import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { QueryClient } from '@tanstack/react-query'
import { deleteCompletedEntry, fmtBytes, getCompletedInventory, getSettings, getTasks, runTask, updateSettings } from './api'
import type { CompletedEntry, CompletedRoot, CompletedReviewFile } from './api'
import { clampPage, Pager, PageSizePicker, sliceForPage } from './Pager'
import { DeliveryLifecycleForDownload } from './DeliveryLifecycle'
import './CompletedFolders.css'

type StorageRow = { root: CompletedRoot; entry: CompletedEntry }
const rowKey = ({ root, entry }: StorageRow) => JSON.stringify([root.path, entry.path])
const isLegacyReview = ({ entry }: StorageRow) => entry.legacyReview || entry.receipts.some((receipt) => receipt.legacyReview)
const isDeliveryManaged = ({ entry }: StorageRow) => !!entry.deliveryManaged || entry.receipts.some((receipt) => receipt.deliveryManaged)
const canDelete = (row: StorageRow) => !row.root.error && !isLegacyReview(row) && !isDeliveryManaged(row) && !['active', 'awaiting_import', 'storage'].includes(row.entry.status)
const canImport = ({ root, entry }: StorageRow) => !root.error && !['active', 'symlink', 'retained', 'storage'].includes(entry.status)
const statusLabel = (status: string) => status.replace(/_/g, ' ')

const deletionKey = ['completed-deletion'] as const
type DeletionState = { running: boolean; total: number; progress: number; deletedKeys: string[]; errors: string[] }

// QueryClient outlives route and responsive-shell remounts. Keep both the
// operation lock and its outcome there, so returning to Activity cannot start
// another batch while an earlier one is still removing files.
async function deleteStorageRows(qc: QueryClient, rows: StorageRow[], filesByKey: Record<string, CompletedReviewFile[]> = {}, legacyReviewKeys: string[] = []) {
  if (qc.getQueryData<DeletionState | null>(deletionKey)?.running) return
  const state: DeletionState = { running: true, total: rows.length, progress: 0, deletedKeys: [], errors: [] }
  const publish = () => qc.setQueryData(deletionKey, { ...state, deletedKeys: [...state.deletedKeys], errors: [...state.errors] })
  publish()
  for (const row of rows) {
    try {
      await deleteCompletedEntry(row.entry.path, row.entry.fingerprint, filesByKey[rowKey(row)], legacyReviewKeys.includes(rowKey(row)))
      state.deletedKeys.push(rowKey(row))
    } catch (error) {
      state.errors.push(`${row.entry.path}: ${error instanceof Error ? error.message : 'Deletion failed'}`)
    }
    state.progress++
    publish()
  }
  state.running = false
  publish()
  void qc.invalidateQueries({ queryKey: ['completed-inventory'] })
}

export function CompletedFolders({ onImport }: { onImport: (path: string, mode?: 'legacy' | 'delivery', downloadId?: number) => void }) {
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
  const [deliveryFileSelection, setDeliveryFileSelection] = useState<Record<string, string>>({})
  const [legacyFileSelection, setLegacyFileSelection] = useState<Record<string, string>>({})
  const [confirm, setConfirm] = useState<StorageRow[] | null>(null)
  const deletionQuery = useQuery<DeletionState | null>({
    queryKey: deletionKey, queryFn: () => null, initialData: null, enabled: false, gcTime: Infinity,
  })
  const deletion = deletionQuery.data
  const busy = deletion?.running ?? false
  const result = deletion && !deletion.running ? { deleted: deletion.deletedKeys.length, errors: deletion.errors } : null
  const dialog = useRef<HTMLDialogElement>(null)
  const keepFiles = useRef<HTMLButtonElement>(null)
  const confirmTrigger = useRef<HTMLButtonElement | null>(null)
  const openConfirmation = (rows: StorageRow[], trigger: HTMLButtonElement) => {
    confirmTrigger.current = trigger
    setConfirm(rows)
  }
  useEffect(() => {
    if (confirm) {
      dialog.current?.showModal()
      keepFiles.current?.focus()
    }
    else {
      dialog.current?.close()
      if (confirmTrigger.current?.isConnected) confirmTrigger.current.focus({ preventScroll: true })
    }
  }, [confirm])
  useEffect(() => {
    if (!deletion) return
    if (!deletion.running) setConfirm(null)
    setSelected((previous) => {
      if (!deletion.deletedKeys.some((key) => key in previous)) return previous
      const next = { ...previous }
      for (const key of deletion.deletedKeys) delete next[key]
      return next
    })
  }, [deletion])
  const data = inventory.data
  const entries = data?.roots.flatMap((root) => root.entries.map((entry) => ({ root, entry }))) ?? []
  const selectedRows = entries.filter((row) => canDelete(row) && selected[rowKey(row)] === row.entry.fingerprint)
  const isSelected = (row: StorageRow) => canDelete(row) && selected[rowKey(row)] === row.entry.fingerprint
  const remove = useMutation({ mutationFn: ({ rows, filesByKey, legacyReviewKeys }: { rows: StorageRow[]; filesByKey?: Record<string, CompletedReviewFile[]>; legacyReviewKeys?: string[] }) => deleteStorageRows(qc, rows, filesByKey, legacyReviewKeys) })
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
  const deliveryConfirm = confirm?.length === 1 && isDeliveryManaged(confirm[0]) ? confirm[0] : null
  const legacyConfirm = confirm?.length === 1 && isLegacyReview(confirm[0]) ? confirm[0] : null
  const selectedFileEntry = deliveryConfirm ?? legacyConfirm
  const selectedDeliveryFiles = deliveryConfirm?.entry.reviewFiles?.filter((file) => deliveryFileSelection[file.path] === file.snapshotIdentity) ?? []
  const selectedLegacyFiles = legacyConfirm?.entry.reviewFiles?.filter((file) => legacyFileSelection[file.path] === file.snapshotIdentity) ?? []
  const selectedExplicitFiles = deliveryConfirm ? selectedDeliveryFiles : selectedLegacyFiles
  const confirmationChanged = !busy && confirm?.some((row) => !entries.some((current) => rowKey(current) === rowKey(row) &&
    (isDeliveryManaged(row) ? isDeliveryManaged(current) && !current.root.error : isLegacyReview(row) ? isLegacyReview(current) && !current.root.error : canDelete(current)) && current.entry.fingerprint === row.entry.fingerprint))

  return <section className="panel download-storage" aria-label="Download storage">
    <div className="storage-heading">
      <div><h2>Download storage</h2><p className="muted">Review leftover downloads and manage files on disk.</p></div>
      <div className="storage-actions">
        <button disabled={scan.isPending || !!scanning || busy} onClick={() => scan.mutate()}>{scanning ? 'Scanning storage…' : 'Scan download storage'}</button>
        <button disabled={cleanup.isPending || busy} onClick={() => cleanup.mutate()}>Retry imported cleanup</button>
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
    {busy && <p role="status">Deleting {deletion?.progress} of {deletion?.total} entries… You can navigate within the app. Keep this tab open until deletion finishes.</p>}
    {result && <div role="status" className={result.errors.length ? 'error-text' : 'muted'}>
      {result.deleted} {result.deleted === 1 ? 'entry' : 'entries'} deleted.{result.errors.length > 0 && <> {result.errors.length} failed; those files were not confirmed deleted.
        <ul>{result.errors.map((error) => <li key={error}>{error}</li>)}</ul></>}
    </div>}
    <dialog ref={dialog} className="storage-confirm" aria-labelledby="storage-confirm-title" onCancel={(event) => {
      event.preventDefault()
      setConfirm(null)
    }}>
    {confirm && <>
      <h3 id="storage-confirm-title">{deliveryConfirm ? 'Delete selected delivery files permanently?' : `Delete ${confirm.length} ${confirm.length === 1 ? 'entry' : 'entries'} permanently?`}</h3>
      {selectedFileEntry ? <>
        <p>{deliveryConfirm ? 'Choose exact files from this accepted Runner manifest.' : 'Choose exact current files to discard from this legacy review entry. No manifest or ownership is inferred.'} Unselected siblings and directories remain untouched.</p>
        <p className="mono">{selectedFileEntry.entry.path}</p>
        <div className="storage-file-selection">{selectedFileEntry.entry.reviewFiles?.map((file) => {
          const selection = deliveryConfirm ? deliveryFileSelection : legacyFileSelection
          const setSelection = deliveryConfirm ? setDeliveryFileSelection : setLegacyFileSelection
          return <label key={file.path}>
            <input type="checkbox" checked={selection[file.path] === file.snapshotIdentity} onChange={(event) => setSelection((previous) => {
              const next = { ...previous }
              if (event.target.checked) next[file.path] = file.snapshotIdentity
              else delete next[file.path]
              return next
            })} />
            <span className="mono">{file.path}</span> · {fmtBytes(file.sizeBytes)} · {new Date(file.modifiedAt).toLocaleString()}
          </label>
        })}</div>
        {!selectedFileEntry.entry.reviewFiles?.length && <p className="muted">Refresh the completed-storage inventory to list current files.</p>}
        <p>{selectedExplicitFiles.length} selected · {fmtBytes(selectedExplicitFiles.reduce((sum, file) => sum + file.sizeBytes, 0))}</p>
      </> : <>
        <p>{confirmFiles} files · {fmtBytes(confirmBytes)} logical size. This cannot be undone.</p>
        <ul>{confirm.map(({ root, entry }) => <li key={`${root.path}:${entry.path}`} className="mono">{entry.path}</li>)}</ul>
      </>}
      {confirmationChanged && <p className="error-text">The inventory changed. Cancel and select the entries again.</p>}
      <div className="storage-actions"><button className="btn-danger" disabled={busy || confirmationChanged || (selectedFileEntry ? selectedExplicitFiles.length === 0 : false)} onClick={() => remove.mutate({ rows: confirm, ...(selectedFileEntry ? { filesByKey: { [rowKey(selectedFileEntry)]: selectedExplicitFiles } } : {}), ...(legacyConfirm ? { legacyReviewKeys: [rowKey(legacyConfirm)] } : {}) })}>
        {busy ? `Deleting ${deletion?.progress ?? 0} of ${confirm.length}…` : selectedFileEntry ? 'Delete selected files' : 'Confirm delete'}
      </button><button ref={keepFiles} onClick={() => setConfirm(null)}>{busy ? 'Close' : 'Keep files'}</button></div>
    </>}
    </dialog>
    {entries.length > 0 && <details className="storage-items">
      <summary>Storage entries ({entries.length})</summary>
      <div className="storage-toolbar">
        <input type="search" aria-label="Search storage" placeholder="Search paths or downloads…" value={search} disabled={busy} onChange={(e) => { setSearch(e.target.value); resetView() }} />
        <label>Status <select aria-label="Storage status" value={status} disabled={busy} onChange={(e) => { setStatus(e.target.value); resetView() }}>
          <option value="all">All statuses</option><option value="attention">Needs attention</option>
          {[...new Set(entries.map(({ entry }) => entry.status))].sort().map((value) => <option key={value} value={value}>{statusLabel(value)}</option>)}
        </select></label>
        <label>Sort <select aria-label="Sort storage" value={sort} onChange={(e) => { setSort(e.target.value); setPage(0) }}><option value="name">Name</option><option value="size">Largest first</option></select></label>
        <PageSizePicker size={size} onChange={(value) => { setSize(value); setPage(0) }} />
      </div>
      <div className="storage-selection">
        <label><input type="checkbox" aria-label="Select page" disabled={!pageEligible.length || busy} checked={allPageSelected}
          ref={(node) => { if (node) node.indeterminate = !allPageSelected && pageEligible.some(isSelected) }} onChange={togglePage} /> Select page</label>
        <span className="muted">{selectedRows.length} selected · {fmtBytes(selectedBytes)}</span>
        <button disabled={!eligible.length || busy} onClick={() => setSelected(Object.fromEntries(eligible.map((row) => [rowKey(row), row.entry.fingerprint])))}>Select all {eligible.length} eligible</button>
        <button disabled={!selectedRows.length || busy} onClick={() => { setSelected({}); setConfirm(null) }}>Clear selection</button>
        <button className="btn-danger" disabled={!selectedRows.length || busy} onClick={(event) => openConfirmation(selectedRows, event.currentTarget)}>Delete selected…</button>
      </div>
      <p className="muted storage-note">Select all includes matching entries on every page. Active downloads, awaiting imports, storage folders, and incomplete scans cannot be selected.</p>
      <div className="storage-pagination storage-pagination-top"><Pager page={currentPage} size={size} total={filtered.length} onPage={setPage} /></div>
      <div className="storage-list" role="list" aria-label="Storage entries">
        {visible.map((row) => {
          const { root, entry } = row
          const name = entry.path.split('/').filter(Boolean).pop() ?? entry.path
          return <article className={`storage-entry${isSelected(row) ? ' is-selected' : ''}`} role="listitem" key={rowKey(row)}>
            <input type="checkbox" aria-label={`Select ${name}`} disabled={!canDelete(row) || busy} checked={isSelected(row)} onChange={() => setSelected((previous) => {
              const next = { ...previous }
              if (isSelected(row)) delete next[rowKey(row)]
              else next[rowKey(row)] = entry.fingerprint
              return next
            })} />
            <div className="storage-entry-name"><strong title={entry.path}>{name}</strong><div className="mono muted storage-path" title={entry.path}>{entry.path.slice(0, -name.length)}</div>
              {entry.receipts.length > 0 && <details><summary>{entry.receipts.length} download {entry.receipts.length === 1 ? 'record' : 'records'}</summary>
                {entry.receipts.map((r, i) => <div key={`${r.downloadId}:${i}`}>#{r.downloadId} {r.title}{!r.live && ' (history only)'}
                  {r.deliveryManaged && <DeliveryLifecycleForDownload downloadId={r.downloadId} />}
                </div>)}
              </details>}
            </div>
            <div className="storage-entry-size"><strong>{fmtBytes(entry.bytes)}</strong><span className="muted">{entry.files} {entry.files === 1 ? 'file' : 'files'}</span></div>
            <div className="storage-entry-status"><span className={`pill ${entry.status === 'failed' ? 'pill-error' : ''}`}>{statusLabel(entry.status)}</span><p className="muted">{entry.reason}</p></div>
            <div className="storage-entry-actions">
              <button disabled={!canImport(row) || busy} onClick={(event) => { event.currentTarget.focus({ preventScroll: true }); onImport(entry.path, isLegacyReview(row) ? 'legacy' : isDeliveryManaged(row) ? 'delivery' : undefined, entry.receipts.find((receipt) => receipt.deliveryManaged)?.downloadId) }}>{isLegacyReview(row) ? 'Review legacy batch' : isDeliveryManaged(row) ? 'Resolve handoff' : 'Resolve files'}</button>
              {isDeliveryManaged(row) ? <button disabled={!!root.error || !entry.reviewFiles?.length || busy} title={root.error ? 'A complete scan is required' : undefined} onClick={(event) => { setDeliveryFileSelection({}); openConfirmation([row], event.currentTarget) }}>Delete selected files…</button>
                : isLegacyReview(row) ? <button disabled={!!root.error || !entry.reviewFiles?.length || busy} title={root.error ? 'A complete scan is required' : undefined} onClick={(event) => { setLegacyFileSelection({}); openConfirmation([row], event.currentTarget) }}>Delete selected files…</button>
                : <button disabled={!canDelete(row) || busy} title={root.error ? 'A complete scan is required' : undefined} onClick={(event) => openConfirmation([row], event.currentTarget)}>Delete files…</button>}
            </div>
          </article>
        })}
      </div>
      {filtered.length === 0 && <p className="muted">No entries match your search or status filter.</p>}
      <div className="storage-pagination"><Pager page={currentPage} size={size} total={filtered.length} onPage={setPage} /></div>
    </details>}
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
      Leave blank to use /working/monarr when mounted, otherwise discover download folders. Unknown files remain visible for review. Ordinary remounts are verified automatically; saving settings does not change storage identity or authorize cleanup on a different mount.</p>
    <textarea rows={3} className="mono" aria-label="Download storage folders"
      value={value ?? settings.data?.completedRoots ?? ''} onChange={(e) => setValue(e.target.value)}
      placeholder="/working/monarr" />
    <button disabled={settings.isPending || save.isPending} onClick={() => save.mutate()}>Save download storage</button>
    {save.isSuccess && <p className="muted">Saved — applies on the next inventory scan.</p>}
    {save.error && <p className="error-text">{save.error.message}</p>}
  </>
}
