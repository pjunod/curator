import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ActionDialog, ActionNotice } from './ActionDialog'
import {
  ApiError,
  OFFLOAD_RUNNING,
  cancelOffload,
  fmtBytes,
  getLibraryOffload,
  getOffload,
  previewOffload,
  startOffload,
  type OffloadOperation,
  type OffloadPreview,
  type OffloadRequest,
} from './api'
import './RematchDialog.css'

/** What the dialog says when a volume's free space is not known. */
export const freeText = (n: number) => (n < 0 ? 'unknown' : fmtBytes(n))

/**
 * Offload… (ADR 0028): free the space, keep the title. The dialog says
 * exactly what goes and how much space comes back, before anything is
 * deleted: every file with sidecars, the bytes freed, the library volume's free
 * space before and after, what blocks it, and what Cinema will do (advisory).
 * Progress and Stop stay in the dialog until it is done.
 */
export function OffloadDialog({ itemId, title, request, onClose }: {
  itemId: number
  title: string
  request: OffloadRequest
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [operation, setOperation] = useState<OffloadOperation | null>(null)
  const preview = useQuery({
    queryKey: ['offload-preview', itemId, request],
    queryFn: () => previewOffload(itemId, request),
    enabled: !operation,
    retry: false,
  })
  const run = useMutation({
    // One request id per preview: a retry of the same click is the same
    // request, and the server answers with the operation it already started.
    mutationFn: ({ p, requestId }: { p: OffloadPreview; requestId: string }) => startOffload(itemId, { ...request, requestId, fingerprint: p.fingerprint }),
    onSuccess: (op) => setOperation(op),
    onError: (e) => {
      // Blocked or stale: the preview says what changed.
      if (e instanceof ApiError && e.status === 409) void preview.refetch()
    },
  })
  const status = useQuery({
    queryKey: ['offload', operation?.operationId],
    queryFn: () => getOffload(operation!.operationId),
    enabled: Boolean(operation),
    refetchInterval: (q) => (OFFLOAD_RUNNING.has(q.state.data?.state ?? 'deleting') ? 1000 : false),
  })
  const op = status.data ?? operation
  const over = op != null && !OFFLOAD_RUNNING.has(op.state)
  useEffect(() => {
    if (over) {
      void qc.invalidateQueries({ queryKey: ['library-item'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
      void qc.invalidateQueries({ queryKey: ['item-status'] })
    }
  }, [over, qc])
  const stop = useMutation({ mutationFn: () => cancelOffload(op!.operationId), onSuccess: () => void status.refetch() })
  // An Offload already running for this title (the dialog was closed while it
  // deleted) is followed here: progress and Stop, not a second preview.
  const open = useQuery({ queryKey: ['library-offload', itemId], queryFn: () => getLibraryOffload(itemId), enabled: !operation, retry: false })
  useEffect(() => {
    if (open.data && OFFLOAD_RUNNING.has(open.data.state) && !operation) setOperation(open.data)
  }, [open.data, operation])

  const p = preview.data
  const fingerprint = p?.fingerprint ?? ''
  const requestId = useMemo(() => (fingerprint ? `offload-${itemId}-${fingerprint}-${Math.random().toString(36).slice(2, 10)}` : ''), [itemId, fingerprint])
  const [sure, setSure] = useState(false)

  return (
    <ActionDialog title={`Offload ${title}`} className="rmd" onClose={onClose}>
      {op ? (
        <OffloadProgress op={op} stopping={stop.isPending} onStop={() => stop.mutate()} stopError={stop.error as Error | null} onClose={onClose} />
      ) : (
        <>
          {preview.isFetching && !p && <span className="muted">Working out what goes…</span>}
          {preview.isError && <div className="banner warning" role="alert">{(preview.error as Error).message}</div>}
          {p && (
            <>
              <p className="rmd-lead">
                Deletes {p.files.length} file{p.files.length === 1 ? '' : 's'} of <b>{p.label}</b> and keeps the title:
                its identity, profile, history and blocklist stay, and nothing searches for it again until you Re-acquire it.
              </p>
              {p.widened && p.widened.length > 0 && (
                <div className="banner warning" role="status">
                  A file covers more than you chose, so the Offload widens to {p.widened.join(', ')} as well.
                </div>
              )}
              <b className="rmd-h">What will be deleted</b>
              <ul className="rmd-plan" aria-label="Files">
                {p.files.map((f) => (
                  <li key={f.path} className="rmd-line rmd-file">
                    <span className="mono">{f.path}</span>
                    <span className="rmd-to">
                      <span className="muted">{f.kind === 'sidecar' ? 'sidecar' : 'video'} · {fmtBytes(f.bytes)}{f.episodes && f.episodes.length ? ` · ${f.episodes.join(', ')}` : ''}</span>
                      {f.shared && <span className="muted">another hard link (a seeding copy) keeps these bytes until it goes too</span>}
                    </span>
                  </li>
                ))}
                {p.files.length === 0 && <li className="muted">There are no files on disk. The title is only marked offloaded.</li>}
              </ul>
              <div className="item-numbers" data-testid="offload-space">
                <span>{fmtBytes(p.freed)} freed{p.freed < p.bytes ? ` (${fmtBytes(p.bytes)} deleted)` : ''}</span>
                <span>free space {freeText(p.freeBefore)} → {freeText(p.freeAfter)}{p.root ? <> on <span className="mono">{p.root}</span></> : null}</span>
              </div>
              {p.cinema && <span className={p.cinemaWarning ? 'warning-text' : 'muted'} data-testid="offload-cinema">{p.cinema}.</span>}
              {p.blocked && (
                <div className="banner warning" role="alert">
                  {p.blocked.message}.{' '}
                  <button type="button" onClick={() => void preview.refetch()}>Wait for it</button>
                </div>
              )}
              {run.isError && <div className="banner warning" role="alert">{(run.error as Error).message}</div>}
              <label className="rmd-check">
                <input type="checkbox" checked={sure} onChange={(e) => setSure(e.target.checked)} />
                I understand the files are deleted, not moved to a trash
              </label>
              <div className="rmd-foot">
                <span className="muted">{p.summary}</span>
                <div className="row-actions">
                  <button type="button" onClick={onClose}>Cancel</button>
                  <button type="button" className="btn-danger" disabled={Boolean(p.blocked) || !sure || run.isPending || preview.isFetching}
                    onClick={() => run.mutate({ p, requestId })}>
                    {run.isPending ? 'Starting…' : 'Offload'}
                  </button>
                </div>
              </div>
            </>
          )}
        </>
      )}
    </ActionDialog>
  )
}

/** The line a finished or running operation reads as. */
export function offloadText(op: OffloadOperation): string {
  switch (op.state) {
    case 'complete': return `Offloaded ${op.label ?? ''}: ${op.filesDone} file${op.filesDone === 1 ? '' : 's'} deleted · ${fmtBytes(op.bytesFreed)} freed.`.replace('  ', ' ')
    case 'cancelled': return `Stopped. ${op.filesDone} of ${op.filesTotal} files were deleted; the rest stay on disk.`
    case 'failed': return `The Offload failed: ${op.lastError ?? 'unknown error'}.`
    case 'waiting': return `Waiting: ${op.lastError ?? 'the library folder is not readable'}. Nothing was deleted while it waits.`
    case 'retrying': return `${op.failed?.length ?? 0} file(s) could not be deleted; retrying. ${op.lastError ?? ''}`.trim()
    case 'stopping': return `Stopping… ${op.filesDone} of ${op.filesTotal} files were deleted; the rest stay on disk.`
    default: return `Deleting… ${op.filesDone} of ${op.filesTotal} files.`
  }
}

function OffloadProgress({ op, stopping, onStop, stopError, onClose }: { op: OffloadOperation; stopping: boolean; onStop: () => void; stopError: Error | null; onClose: () => void }) {
  const running = OFFLOAD_RUNNING.has(op.state)
  return (
    <div className="rmd-progress">
      <ActionNotice warning={op.state === 'failed' || op.state === 'retrying' || op.state === 'waiting'}>{offloadText(op)}</ActionNotice>
      {op.filesTotal > 0 && running && <progress max={op.filesTotal} value={op.filesDone} aria-label="Files deleted" />}
      {(op.failed ?? []).map((f) => <p key={f} className="warning-text small mono">{f}</p>)}
      {op.cinemaLine && <p className="muted">{op.cinemaLine}</p>}
      {stopError && <p className="error-text">{stopError.message}</p>}
      <div className="rmd-foot">
        <span className="muted">Operation {op.operationId}</span>
        <div className="row-actions">
          {op.stoppable && <button type="button" className="btn-danger" disabled={stopping} onClick={onStop}>{stopping ? 'Stopping…' : 'Stop'}</button>}
          <button type="button" className="btn-accent" onClick={onClose}>{running ? 'Close (keeps running)' : 'Done'}</button>
        </div>
      </div>
    </div>
  )
}
