import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ActionDialog, ActionNotice, focusActionTrigger } from '../ActionDialog'
import {
  acceptCompletedStorageChange, fmtBytes, getCompletedInventory, getRootFolders, getTasks,
  previewCompletedStorageChange, type RootFolder, type TaskState,
} from '../api'
import { getStorageObservations, type StorageObservation } from '../activityActions'
import '../Activity.css'

function observedAt(at?: string) {
  return at && !at.startsWith('0001') ? new Date(at).toLocaleString() : null
}

function capacity(root?: RootFolder & { totalBytes?: number }) {
  // The existing API also uses zero if its free-space measurement fails.
  if (!root?.accessible || root.freeBytes <= 0) return 'Available space unknown'
  return root.totalBytes && root.totalBytes >= root.freeBytes
    ? `${fmtBytes(root.freeBytes)} free of ${fmtBytes(root.totalBytes)}`
    : `${fmtBytes(root.freeBytes)} free`
}

function measuredBytes(bytes: number) {
  return bytes === 0 ? '0 B' : fmtBytes(bytes)
}

function cleanupObservation(task?: TaskState) {
  if (!task) return 'Cleanup status unavailable.'
  if (task.running) return 'Download cleanup is running.'
  const checked = observedAt(task.lastRunAt)
  if (task.lastError) return `${checked ? `Cleanup reported an error at ${checked}` : 'Cleanup reported an error'}. ${task.lastError}`
  return checked ? `Last cleanup run ${checked}.` : 'Waiting for the first cleanup run.'
}

export function SystemStorage() {
  const qc = useQueryClient()
  const opener = useRef<HTMLElement | null>(null)
  const heading = useRef<HTMLHeadingElement | null>(null)
  const [review, setReview] = useState<StorageObservation | null>(null)
  const [showKept, setShowKept] = useState(false)
  const inventory = useQuery({ queryKey: ['completed-inventory'], queryFn: getCompletedInventory, enabled: showKept, refetchInterval: showKept ? 30_000 : false })
  const kept = inventory.data?.roots.flatMap(root => root.entries.flatMap(entry => {
    const receipt = entry.receipts[0]
    return entry.status === 'retained' && entry.receipts.length === 1 && receipt.archived
      ? [{ entry, receipt, error: root.error }] : []
  }))
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders, staleTime: 30_000 })
  const downloads = useQuery({
    queryKey: ['storage-observations'],
    queryFn: getStorageObservations,
    refetchInterval: 30_000,
  })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 10_000 })
  const preview = useMutation({
    mutationFn: (root: StorageObservation) => previewCompletedStorageChange({
      root: root.path, expectedIdentity: root.storageIdentity!, observedIdentity: root.observedStorageIdentity!,
    }),
  })
  const accept = useMutation({
    mutationFn: acceptCompletedStorageChange,
    onSuccess: () => {
      opener.current = heading.current
      setReview(null)
      void qc.invalidateQueries({ queryKey: ['storage-observations'] })
      void qc.invalidateQueries({ queryKey: ['completed-inventory'] })
      void qc.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  const cleanup = tasks.data?.find(task => task.name === 'downloads.cleanup')
  const recorded = downloads.data?.cleanup
  const lastRemoval = observedAt(recorded?.lastRemovalAt)
  return <section id="storage" className="system-storage">
    <div className="activity-storage-heading">
      <div>
        <h2 ref={heading} tabIndex={-1}>Storage</h2>
        <p className="muted">Drive capacity and automatic cleanup.</p>
      </div>
      <Link to="/settings" hash="library-folders">Manage storage folders</Link>
    </div>
    <section aria-labelledby="library-storage-title">
      <h3 id="library-storage-title">Library folders</h3>
      {roots.isLoading && <p role="status">Checking library storage…</p>}
      {roots.isError && <p role="alert" className="error-text">Could not check library storage. {roots.error.message}</p>}
      {roots.data?.length === 0 && !roots.isError && <p className="muted">No library folders configured.</p>}
      {roots.data?.map(root => <article className="panel activity-storage-observation" key={root.id}>
        <div className="activity-storage-heading">
          <strong className="mono">{root.path}</strong>
          <span>{capacity(root)}</span>
        </div>
        <p className="muted small">{root.accessible ? 'Curator · Accessible' : 'Curator · Unavailable'} · Checked {new Date(roots.dataUpdatedAt).toLocaleString()}</p>
      </article>)}
    </section>
    <section aria-labelledby="download-storage-title">
      <h3 id="download-storage-title">Download folders</h3>
      {downloads.isLoading && <p role="status">Checking download storage…</p>}
      {downloads.isError && <p role="alert" className="error-text">Could not check download storage. {downloads.error.message}</p>}
      {downloads.data?.roots.length === 0 && !downloads.isError && <p className="muted">No download folders have been checked yet.</p>}
      {downloads.data?.roots.map(root => {
        const changed = root.owner === 'Curator' && root.storageIdentity && root.observedStorageIdentity && root.storageIdentity !== root.observedStorageIdentity
        const checked = observedAt(root.observedAt)
        const free = root.status === 'available' && root.freeBytes !== undefined
          ? `${measuredBytes(root.freeBytes)} free${root.totalBytes !== undefined ? ` of ${measuredBytes(root.totalBytes)}` : ''}`
          : 'Available space unknown'
        return <article className="panel activity-storage-observation" key={`${root.owner}/${root.name ?? ''}/${root.role}/${root.path}`}>
          <div className="activity-storage-heading">
            <strong className="mono">{root.path || root.name || root.role}</strong>
            <span>{free}</span>
          </div>
          <p className="muted small">{[root.owner, root.path ? root.name : null, root.role, checked ? `Reported ${checked}` : null].filter(Boolean).join(' · ')}</p>
          {(root.error || root.status !== 'available') && <p className="muted small">{root.error || 'Capacity has not been measured.'}</p>}
          {changed && <button disabled={downloads.isError} onClick={event => {
            focusActionTrigger(event)
            opener.current = event.currentTarget
            accept.reset()
            preview.reset()
            setReview(root)
            preview.mutate(root)
          }}>Review replacement storage</button>}
        </article>
      })}
    </section>
    <section aria-labelledby="kept-downloads-title">
      <h3 id="kept-downloads-title">Files kept from archived jobs</h3>
      <button aria-expanded={showKept} onClick={() => setShowKept(!showKept)}>{showKept ? 'Hide kept downloads' : 'Show kept downloads'}</button>
      {showKept && <div aria-live="polite">
        {inventory.isLoading && <p>Reading completed-folder scans…</p>}
        {inventory.isError && <p role="alert">Could not load kept downloads. {inventory.error.message}</p>}
        {kept?.length === 0 && <p>No kept downloads in the latest scans.</p>}
        {kept?.map(({ entry, receipt, error }) => <p key={entry.path}>
          <a href={`/activity?job=${receipt.downloadId}#history`}>{receipt.title} · Job #{receipt.downloadId}</a> · {measuredBytes(entry.bytes)}
          {error && <span> · Last successful scan. {error}</span>}
        </p>)}
      </div>}
    </section>
    <section aria-labelledby="storage-cleanup-title">
      <h3 id="storage-cleanup-title">Automatic cleanup</h3>
      <div className="panel activity-storage-observation">
        {tasks.isLoading ? <p role="status">Checking cleanup status…</p> : <p className="muted">{cleanupObservation(cleanup)}</p>}
        {tasks.isError && <p role="alert" className="error-text">Could not refresh cleanup status. {tasks.error.message}</p>}
        {recorded?.filesRemoved !== undefined && recorded.logicalBytesRemoved !== undefined && <p className="muted">
          Recorded cleanup · {recorded.filesRemoved.toLocaleString()} files · {measuredBytes(recorded.logicalBytesRemoved)} in logical file sizes
          {lastRemoval ? ` · Last removal ${lastRemoval}` : ''}
        </p>}
        <div className="row-actions"><Link to="/activity" hash="attention">Jobs needing attention</Link><Link to="/activity" hash="history">Job history</Link></div>
      </div>
    </section>
    {accept.isSuccess && <ActionNotice>Replacement storage accepted for new requests.</ActionNotice>}
    {review && <ActionDialog title="Review replacement storage" returnFocus={opener} onClose={() => { if (!accept.isPending) setReview(null) }}>
      <p className="mono">{review.path}</p>
      <p>Reconnect the expected drive to recover automatically. Accept replacement storage when you intentionally changed drives.</p>
      {preview.isPending && <p role="status">Checking the current drive and affected requests…</p>}
      {preview.error && <p role="alert" className="error-text">{preview.error.message}</p>}
      {preview.data && <>
        <dl>
          <dt>Expected drive</dt><dd>{preview.data.expectedDescription}</dd>
          <dt>Observed drive</dt><dd>{preview.data.observedDescription}</dd>
        </dl>
        <p>This allows new requests to use the replacement storage. Existing jobs keep their original storage requirements.</p>
        {!!preview.data.activeImports && <p role="status">Wait for {preview.data.activeImports} active imports to finish, then refresh this review.</p>}
      </>}
      {accept.error && <p role="alert" className="error-text">{accept.error.message}</p>}
      <div className="row-actions">
        <button disabled={preview.isPending || accept.isPending} onClick={() => preview.mutate(review)}>Refresh review</button>
        {preview.data && <button className="btn-accent" disabled={preview.isPending || accept.isPending || preview.data.activeImports > 0}
          onClick={() => { if (preview.data) accept.mutate(preview.data) }}>{accept.isPending ? 'Accepting…' : 'Accept replacement storage'}</button>}
      </div>
    </ActionDialog>}
  </section>
}
