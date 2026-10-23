import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  getDeliveryLifecycle,
  retryCuratorDeliveryCleanup,
  retryDeliveryHandoff,
  retryRunnerDeliveryCleanup,
  queueDeliveryImport,
  type DeliveryLifecycleReceipt,
} from './api'
import { fmtBytes, fmtRelative } from './api'

const elapsed = (value?: string) => value ? fmtRelative(value) : 'not recorded'
const pending = (value?: string) => !!value && !value.startsWith('0001') && new Date(value).getTime() > Date.now()
const runnerTime = (value: unknown) => typeof value === 'number' && value > 0 ? fmtRelative(new Date(value * 1000).toISOString()) : 'not recorded'
const runnerMillisTime = (value: unknown) => typeof value === 'number' && value > 0 ? fmtRelative(new Date(value).toISOString()) : 'not recorded'
const span = (milliseconds: number) => {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return 'unknown'
  const seconds = Math.floor(milliseconds / 1000)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  return hours ? `${hours}h ${minutes}m ${remainder}s` : minutes ? `${minutes}m ${remainder}s` : `${remainder}s`
}
const knownTimestamp = (value?: string) => !!value && !value.startsWith('0001') && Number.isFinite(new Date(value).getTime())
const completedPath = (receipt: DeliveryLifecycleReceipt) => receipt.mappedPath || receipt.publicationPath || ''
export const deliveryLifecyclePollingInterval = (open: boolean) => open ? 5_000 : false
const completedFolder = (receipt: DeliveryLifecycleReceipt) => {
  const path = completedPath(receipt).replace(/[\\/]+$/, '')
  return path.split(/[\\/]/).at(-1) || 'folder unknown'
}
const cleanupStatus = (status: string, error?: string, blocked?: string, next?: string) => {
  if (status === 'removed' || status === 'succeeded') return 'Complete'
  if (status === 'retry' || error) return `Retry pending · blocked ${elapsed(blocked)}${pending(next) ? ` · next ${fmtRelative(next!)}` : ''}`
  return status.replaceAll('_', ' ')
}

export function DeliveryLifecycleTrace({ receipt }: { receipt: DeliveryLifecycleReceipt }) {
  const qc = useQueryClient()
  const [selectedPaths, setSelectedPaths] = useState<string[]>([])
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['delivery-lifecycle'] })
    void qc.invalidateQueries({ queryKey: ['completed-inventory'] })
    void qc.invalidateQueries({ queryKey: ['queue'] })
  }
  const handoff = useMutation({ mutationFn: () => retryDeliveryHandoff(receipt.receiptId), onSettled: invalidate })
  const runnerCleanup = useMutation({ mutationFn: () => retryRunnerDeliveryCleanup(receipt.receiptId), onSettled: invalidate })
  const curatorCleanup = useMutation({ mutationFn: () => retryCuratorDeliveryCleanup(receipt.receiptId), onSettled: invalidate })
  const importSelected = useMutation({ mutationFn: () => queueDeliveryImport(receipt.receiptId, selectedPaths), onSuccess: () => { setSelectedPaths([]); invalidate() } })
  const blocked = [receipt.blockedSince, receipt.sourceCleanupBlockedSince, receipt.directoryCleanupBlockedSince,
    ...receipt.curatorFiles.map((file) => file.cleanupBlockedSince)].filter((value) => value && !value.startsWith('0001')) as string[]
  const longestBlock = blocked.map((value) => new Date(value).getTime()).filter(Number.isFinite).sort()[0]
  const warning = longestBlock !== undefined && Date.now() - longestBlock >= 30 * 60 * 1000
  const busy = handoff.isPending || runnerCleanup.isPending || curatorCleanup.isPending || importSelected.isPending
  const runnerSource = receipt.sourceCleanup ?? {}
  const runnerSourceState = typeof runnerSource.status === 'string' ? runnerSource.status : 'unknown'
  const acknowledgement = receipt.acceptance.replaceAll('_', ' ')

  return <details className="delivery-lifecycle">
    <summary>Runner handoff · {completedFolder(receipt)} · {receipt.state.replaceAll('_', ' ')}</summary>
    <div className="delivery-lifecycle-body">
      <p><strong>Completed folder:</strong> <span className="mono">{completedPath(receipt) || 'path unknown'}</span></p>
      <p><strong>Publication:</strong> {receipt.terminalOutcome || 'outcome not reported'} · {receipt.state.replaceAll('_', ' ')} · acknowledgement: {acknowledgement} · ready {elapsed(receipt.readyAt)}</p>
      <p className="muted">Resolved library target: item {receipt.mediaItemId ?? 'unknown'} · copy {receipt.copyId ?? 0} · download {receipt.downloadId}</p>
      <p><strong>Attempts:</strong> {receipt.attempts} · blocked {elapsed(receipt.blockedSince)}{pending(receipt.nextAttemptAt) ? ` · next handoff ${fmtRelative(receipt.nextAttemptAt!)}` : ''}</p>
      <details className="delivery-attempt-history"><summary>Curator attempt history ({receipt.attemptHistory?.length ?? 0})</summary>
        {receipt.attemptHistory?.length ? <ol>{receipt.attemptHistory.map((event, index) => {
          const previous = [...receipt.attemptHistory.slice(0, index)].reverse().find((item) => item.stage === event.stage && knownTimestamp(item.finishedAt))
          const waiting = previous && knownTimestamp(event.startedAt) ? Math.max(0, new Date(event.startedAt).getTime() - new Date(previous.finishedAt!).getTime()) : undefined
          const finished = knownTimestamp(event.finishedAt)
          const work = finished ? Math.max(0, new Date(event.finishedAt!).getTime() - new Date(event.startedAt).getTime()) : undefined
          const stage = event.stage.startsWith('cleanup_file:') ? `completed file cleanup · ${event.stage.slice('cleanup_file:'.length)}` : event.stage.replaceAll('_', ' ')
          return <li key={event.id}><strong>{stage}</strong> · attempt {event.attempt} · {event.result}
            <div>Started {knownTimestamp(event.startedAt) ? fmtRelative(event.startedAt) : 'time unavailable'}{finished ? ` · finished ${fmtRelative(event.finishedAt)} · work ${span(work!)}` : ` · completion not recorded; outcome unknown (elapsed since start ${knownTimestamp(event.startedAt) ? span(Date.now() - new Date(event.startedAt).getTime()) : 'unknown'})`}{waiting !== undefined ? ` · wait before attempt ${span(waiting)}` : ''}</div>
            {event.error && <div className="error-text">{event.error}</div>}
          </li>
        })}</ol> : <p className="muted">No Curator attempt history was recorded before this version. Historical durations are unknown.</p>}
      </details>
      {receipt.error && <p className="error-text">{receipt.error}</p>}
      {warning && <p className="warning-text" role="status">This lifecycle has been blocked for at least 30 minutes. Review the cause and retry when the underlying issue is corrected.</p>}
      {receipt.failureStage && <p><strong>Runner stage:</strong> {receipt.failureStage}{receipt.failureCause ? ` · ${receipt.failureCause}` : ''} {receipt.keepSourceOnFailure ? '· source kept on failure' : ''}</p>}
      <p><strong>Runner source cleanup:</strong> {cleanupStatus(runnerSourceState, receipt.sourceCleanupError, receipt.sourceCleanupBlockedSince, receipt.sourceCleanupNextAttemptAt)} · {Number(runnerSource.attempts ?? receipt.sourceCleanupAttempts)} attempts
        {runnerSource.startedAt ? ` · started ${runnerTime(runnerSource.startedAt)}` : ''}{runnerSource.lastAttemptAt ? ` · last attempt ${runnerTime(runnerSource.lastAttemptAt)}` : ''}{runnerSource.completedAt ? ` · completed ${runnerTime(runnerSource.completedAt)}` : ''}{runnerSource.nextRetryAt ? ` · Runner next ${runnerTime(runnerSource.nextRetryAt)}` : ''}
      </p>
      {receipt.sourceCleanupAttempts > 0 && <p className="muted">Curator status refresh: {receipt.sourceCleanupAttempts} attempts{receipt.sourceCleanupBlockedSince ? ` · blocked ${elapsed(receipt.sourceCleanupBlockedSince)}` : ''}{pending(receipt.sourceCleanupNextAttemptAt) ? ` · next ${fmtRelative(receipt.sourceCleanupNextAttemptAt!)}` : ''}</p>}
      {receipt.sourceCleanupError && <p className="error-text">{receipt.sourceCleanupError}</p>}
      <details><summary>Runner source cleanup attempts ({runnerSource.attemptHistory?.length ?? 0})</summary>
        {runnerSource.attemptHistory?.length ? <ol>{runnerSource.attemptHistory.map((event) => <li key={event.attempt}>
          Attempt {event.attempt} · {event.outcome} · started {runnerMillisTime(event.startedAtMs)} · wait {span(event.waitMs)}
          {typeof event.workMs === 'number' ? ` · work ${span(event.workMs)}` : ' · work duration not recorded'}
          {event.endedAtMs ? ` · ended ${runnerMillisTime(event.endedAtMs)}` : ' · outcome still running'}
          {event.error && <div className="error-text">{event.error}</div>}
        </li>)}</ol> : <p className="muted">Runner did not provide historical attempt timing for this receipt. Earlier durations are unknown.</p>}
      </details>
      {typeof runnerSource.error === 'string' && runnerSource.error && <p className="error-text">{runnerSource.error}</p>}
      {typeof runnerSource.warning === 'string' && runnerSource.warning && <p className="warning-text">{runnerSource.warning}</p>}
      {runnerSource.warning === true && <p className="warning-text">Runner reports that source cleanup needs attention.</p>}
      <p><strong>Curator publication cleanup:</strong> {cleanupStatus(receipt.directoryCleanupState, receipt.directoryCleanupError, receipt.directoryCleanupBlockedSince, receipt.directoryCleanupNextAttemptAt)} · {receipt.directoryCleanupAttempts} attempts</p>
      {receipt.directoryCleanupError && <p className="error-text">{receipt.directoryCleanupError}</p>}
      <div className="row-actions">
        {receipt.state !== 'conflict' && <button disabled={busy} onClick={() => handoff.mutate()}>Retry handoff</button>}
        <button disabled={busy} onClick={() => runnerCleanup.mutate()}>Retry Runner source cleanup</button>
        <button disabled={busy || receipt.state === 'conflict'} onClick={() => curatorCleanup.mutate()}>Retry Curator cleanup</button>
      </div>
      {(handoff.error || runnerCleanup.error || curatorCleanup.error) && <p role="alert" className="error-text">{(handoff.error || runnerCleanup.error || curatorCleanup.error)?.message}</p>}
      {(handoff.isSuccess || runnerCleanup.isSuccess || curatorCleanup.isSuccess) && <p role="status" className="muted">Retry queued. The durable receipt remains visible until its obligation is complete.</p>}
      <details><summary>Transfer and publication identity</summary>
        <dl><dt>Delivery</dt><dd className="mono">{receipt.deliveryId}</dd><dt>Runner instance</dt><dd className="mono">{receipt.runnerInstanceId || 'unknown'}</dd>
          <dt>Publication generation</dt><dd className="mono">{receipt.publicationGeneration}</dd><dt>Manifest digest</dt><dd className="mono">{receipt.manifestDigest}</dd>
          <dt>Request token</dt><dd className="mono">{receipt.requestToken || 'unknown'}</dd><dt>Source path</dt><dd className="mono">{receipt.publicationPath || 'unknown'}</dd>
          <dt>Curator path</dt><dd className="mono">{receipt.mappedPath || 'unknown'}</dd><dt>Download generation</dt><dd className="mono">{receipt.generationKey || 'unknown'}</dd></dl>
      </details>
      <details><summary>Per-file outcomes ({receipt.curatorFiles.length} publication files)</summary>
        <p className="muted">Select exact paths from the accepted manifest to queue a retry. Each file is reverified before import; other files are untouched.</p>
        <ul>{receipt.manifestFiles.map((file) => {
          const row = receipt.curatorFiles.find((item) => item.path === file.path)
          const source = receipt.sourceFileOutcomes?.find((item) => item.publicationPath === file.path)
          const resolved = row && ['placed', 'redundant', 'disposable_selected_scope'].includes(row.disposition)
          const removed = row?.cleanupState === 'removed'
          return <li key={file.path}>
            {!resolved && !removed && <label><input type="checkbox" aria-label={`Select ${file.path} for import`} checked={selectedPaths.includes(file.path)} onChange={(event) => setSelectedPaths((previous) => event.target.checked ? [...previous, file.path] : previous.filter((path) => path !== file.path))} /> </label>}
            <strong className="mono">{file.path}</strong> · {fmtBytes(file.sizeBytes)}
            <div>Curator: {row ? `${row.disposition.replaceAll('_', ' ')} · ${cleanupStatus(row.cleanupState, row.cleanupError, row.cleanupBlockedSince, row.cleanupNextAttemptAt)} · ${row.cleanupAttempts} attempts` : 'not yet resolved'}</div>
            {row?.reason && <div className="muted">{row.reason}</div>}{row?.cleanupError && <div className="error-text">{row.cleanupError}</div>}
            {source && <div className="muted">Runner source: {source.classification} · {source.outcome} · cleanup {source.sourceCleanupStatus}{source.reason ? ` · ${source.reason}` : ''}</div>}
          </li>
        })}</ul>
        <button disabled={busy || selectedPaths.length === 0 || receipt.state === 'conflict'} onClick={() => importSelected.mutate()}>Queue import selected ({selectedPaths.length})</button>
        {importSelected.error && <p role="alert" className="error-text">{importSelected.error.message}</p>}
        {importSelected.isSuccess && <p role="status" className="muted">Selected manifest files queued for verification and import.</p>}
      </details>
      {receipt.sourceCleanup && <details><summary>Runner cleanup trace</summary><pre>{JSON.stringify(receipt.sourceCleanup, null, 2)}</pre></details>}
    </div>
  </details>
}

export function DeliveryLifecycleForDownload({ downloadId }: { downloadId: number }) {
  const [open, setOpen] = useState(false)
  const [cursorStack, setCursorStack] = useState<number[]>([0])
  const cursor = cursorStack[cursorStack.length - 1]
  const query = useQuery({
    queryKey: ['delivery-lifecycle', 'download', downloadId, cursor],
    queryFn: () => getDeliveryLifecycle(cursor || undefined, 100, downloadId),
    enabled: open,
    refetchInterval: deliveryLifecyclePollingInterval(open),
  })
  const nextBeforeId = query.data?.nextBeforeId
  return <details className="delivery-lifecycle-lookup" onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary>Handoff and cleanup trace</summary>
    {query.isLoading && <p className="muted">Loading durable lifecycle…</p>}
    {query.error && <p role="alert" className="error-text">{query.error.message}</p>}
    {cursorStack.length > 1 && <button onClick={() => setCursorStack((stack) => stack.slice(0, -1))}>Newer receipts</button>}
    {query.data?.items.length === 0 && <>
      <p className="muted">No Runner delivery receipt is associated with this download. Browse the durable receipt archive to resolve a delivery.</p>
      <DeliveryLifecycleArchive initiallyOpen active={open} />
    </>}
    {query.data?.items.map((receipt) => <DeliveryLifecycleTrace key={receipt.receiptId} receipt={receipt} />)}
    {!!nextBeforeId && <button onClick={() => setCursorStack((stack) => [...stack, nextBeforeId])}>Older receipts</button>}
  </details>
}

export function DeliveryLifecycleArchive({ initiallyOpen = false, active = true }: { initiallyOpen?: boolean; active?: boolean }) {
  const [open, setOpen] = useState(initiallyOpen)
  const [cursorStack, setCursorStack] = useState<number[]>([0])
  const cursor = cursorStack[cursorStack.length - 1]
  const query = useQuery({
    queryKey: ['delivery-lifecycle', 'archive', cursor],
    queryFn: () => getDeliveryLifecycle(cursor || undefined, 50),
    enabled: open && active,
    refetchInterval: deliveryLifecyclePollingInterval(open && active),
  })
  const nextBeforeId = query.data?.nextBeforeId
  return <details className="delivery-lifecycle-archive" open={initiallyOpen ? open : undefined} onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary>Runner lifecycle history</summary>
    <p className="muted">Choose a receipt below to inspect its exact manifest files and resolve selected files. Durable handoff and cleanup receipts remain here after Activity rows are dismissed or completed files are removed.</p>
    {query.isLoading && <p className="muted">Loading lifecycle receipts…</p>}
    {query.error && <p role="alert" className="error-text">{query.error.message}</p>}
    {query.data?.items.length === 0 && <p className="muted">No Runner delivery receipts are recorded.</p>}
    {query.data?.items.map((receipt) => <DeliveryLifecycleTrace key={receipt.receiptId} receipt={receipt} />)}
    <div className="row-actions">
      {cursorStack.length > 1 && <button onClick={() => setCursorStack((stack) => stack.slice(0, -1))}>Newer receipts</button>}
      {!!nextBeforeId && <button onClick={() => setCursorStack((stack) => [...stack, nextBeforeId])}>Older receipts</button>}
    </div>
  </details>
}
