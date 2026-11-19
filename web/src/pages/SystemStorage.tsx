import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ActionDialog, ActionNotice, focusActionTrigger } from '../ActionDialog'
import {
  acceptCompletedStorageChange, fmtBytes, getCompletedInventory, getRootFolders, getTasks,
  previewCompletedStorageChange, type CompletedRoot, type RootFolder, type TaskState,
} from '../api'
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
  const [review, setReview] = useState<CompletedRoot | null>(null)
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders, staleTime: 30_000 })
  const downloads = useQuery({
    queryKey: ['completed-inventory'],
    queryFn: getCompletedInventory,
    select: inventory => inventory.roots,
    staleTime: 30_000,
  })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 10_000 })
  const preview = useMutation({
    mutationFn: (root: CompletedRoot) => previewCompletedStorageChange({
      root: root.path, expectedIdentity: root.storageIdentity!, observedIdentity: root.observedStorageIdentity!,
    }),
  })
  const accept = useMutation({
    mutationFn: acceptCompletedStorageChange,
    onSuccess: () => {
      opener.current = heading.current
      setReview(null)
      void qc.invalidateQueries({ queryKey: ['completed-inventory'] })
      void qc.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  const cleanup = tasks.data?.find(task => task.name === 'downloads.cleanup')
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
        <p className="muted small">{root.accessible ? 'Accessible' : 'Unavailable'} · Checked {new Date(roots.dataUpdatedAt).toLocaleString()}</p>
      </article>)}
    </section>
    <section aria-labelledby="download-storage-title">
      <h3 id="download-storage-title">Download folders</h3>
      {downloads.isLoading && <p role="status">Checking download storage…</p>}
      {downloads.isError && <p role="alert" className="error-text">Could not check download storage. {downloads.error.message}</p>}
      {downloads.data?.length === 0 && !downloads.isError && <p className="muted">No download folders have been checked yet.</p>}
      {downloads.data?.map(root => {
        const changed = root.storageIdentity && root.observedStorageIdentity && root.storageIdentity !== root.observedStorageIdentity
        const checked = observedAt(root.checkedAt)
        const matchingLibraryRoot = root.error ? undefined : roots.data?.find(volume => volume.path === root.path)
        return <article className="panel activity-storage-observation" key={root.path}>
          <div className="activity-storage-heading">
            <strong className="mono">{root.path}</strong>
            <span>{root.freeBytes !== undefined && !root.error ? `${fmtBytes(root.freeBytes)} free${root.totalBytes !== undefined ? ` of ${fmtBytes(root.totalBytes)}` : ''}` : capacity(matchingLibraryRoot)}</span>
          </div>
          <p className="muted small">{root.error || (changed ? 'Storage has changed.' : 'Accessible at last check')}{checked ? ` · Checked ${checked}` : ''}</p>
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
    <section aria-labelledby="storage-cleanup-title">
      <h3 id="storage-cleanup-title">Automatic cleanup</h3>
      <div className="panel activity-storage-observation">
        {tasks.isLoading ? <p role="status">Checking cleanup status…</p> : <p className="muted">{cleanupObservation(cleanup)}</p>}
        {tasks.isError && <p role="alert" className="error-text">Could not refresh cleanup status. {tasks.error.message}</p>}
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
