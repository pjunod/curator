import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getDeliveries, retryDelivery, retryFailedDeliveries } from '../api'
import type { Delivery } from '../api'

// The delivery log for one notifier.
//
// It exists because a media-server notification is not an announcement of
// the work — it IS the work. "Did the Discord message arrive?" is idle
// curiosity; "did plurx get told?" is the same question as "why isn't this
// in my library?", and it needs an answer that outlives a log buffer.

function ago(ms: number): string {
  const secs = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (secs < 60) return `${secs}s ago`
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`
  return `${Math.round(secs / 86400)}d ago`
}

function inFuture(ms: number): string {
  const secs = Math.max(0, Math.round((ms - Date.now()) / 1000))
  if (secs < 60) return `in ${secs}s`
  return `in ${Math.round(secs / 60)}m`
}

// What each row actually tells you, in one line. A status word alone
// ("failed") sends someone hunting through logs; the reason belongs here.
export function summary(d: Delivery): string {
  if (d.status === 'ok') return d.result || 'delivered'
  if (d.status === 'pending') {
    const when = d.nextAt ? `, retrying ${inFuture(d.nextAt)}` : ''
    if (!d.lastError) return 'queued'
    // attempts 0 with an error is a row somebody put back on the queue: the
    // error is from before the retry, not from an attempt that has not
    // happened yet.
    if (d.attempts === 0) return `retry queued${when} — last failure: ${d.lastError}`
    return `attempt ${d.attempts} failed: ${d.lastError}${when}`
  }
  return d.lastError || 'failed'
}

const PILL: Record<Delivery['status'], string> = {
  ok: 'pill-ok',
  pending: 'pill-neutral',
  failed: 'pill-warning',
}

export function NotifierDeliveries({ id }: { id: number }) {
  const qc = useQueryClient()
  // Polled while open: a pending row is waiting on a timer nobody else is
  // going to tell us about.
  const deliveries = useQuery({
    queryKey: ['deliveries', id],
    queryFn: () => getDeliveries(id),
    refetchInterval: 5000,
  })
  // A failed row is not the end of it. The schedule gives up after two and a
  // half minutes because it is tuned for a restart; a media server that
  // refused work for a day leaves a column of failed rows that are all fine
  // to send again once it is healthy — from here, not from a database shell.
  const refresh = () => qc.invalidateQueries({ queryKey: ['deliveries', id] })
  const retryOne = useMutation({
    mutationFn: (deliveryId: number) => retryDelivery(id, deliveryId),
    onSettled: refresh,
  })
  const retryAll = useMutation({
    mutationFn: () => retryFailedDeliveries(id),
    onSettled: refresh,
  })
  const [sweep, setSweep] = useState<number | null>(null)

  if (deliveries.isPending) return <p className="muted">Loading deliveries…</p>
  if (deliveries.isError) {
    return <p className="muted">Could not read deliveries: {(deliveries.error as Error).message}</p>
  }
  const rows = deliveries.data ?? []
  if (rows.length === 0) {
    return <p className="muted">No deliveries yet — this notifier fires after an import.</p>
  }

  // The list is the newest 100 rows; the sweep is the whole history. So the
  // button is always there — a wedge that outlasted 100 imports has all its
  // failures below the fold — the count in the label is what is visible,
  // and what the server actually requeued is reported back after the click.
  const failed = rows.filter((d) => d.status === 'failed').length
  const busy = retryOne.isPending || retryAll.isPending
  const error = (retryAll.error ?? retryOne.error) as Error | null

  return (
    <>
      <div className="add-controls" style={{ marginBottom: 8 }}>
        <button
          onClick={() => {
            setSweep(null)
            retryAll.mutate(undefined, { onSuccess: (r) => setSweep(r.requeued) })
          }}
          disabled={busy}
        >
          {retryAll.isPending
            ? 'Retrying…'
            : failed > 0
              ? `Retry all failed (${failed} shown)`
              : 'Retry all failed'}
        </button>
        {sweep !== null && !error && (
          <span className="muted">
            {sweep === 0 ? 'nothing to retry' : `${sweep} requeued`}
          </span>
        )}
        {error && <span className="error-text">✗ {error.message}</span>}
      </div>
      <table>
        <thead>
          <tr>
            <th>When</th>
            <th>Event</th>
            <th>Status</th>
            <th>Detail</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {rows.map((d) => (
            <tr key={d.id}>
              <td className="muted">{ago(d.createdAt)}</td>
              <td>{d.event}</td>
              <td>
                <span className={`pill ${PILL[d.status]}`}>{d.status}</span>
                {d.attempts > 1 && <span className="muted"> ×{d.attempts}</span>}
              </td>
              <td className="muted">{summary(d)}</td>
              <td>
                {d.status === 'failed' && (
                  <button onClick={() => retryOne.mutate(d.id)} disabled={busy}>
                    Retry
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}
