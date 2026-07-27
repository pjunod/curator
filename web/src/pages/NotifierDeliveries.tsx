import { useQuery } from '@tanstack/react-query'
import { getDeliveries } from '../api'
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
function summary(d: Delivery): string {
  if (d.status === 'ok') return d.result || 'delivered'
  if (d.status === 'pending') {
    const when = d.nextAt ? `, retrying ${inFuture(d.nextAt)}` : ''
    return d.lastError ? `attempt ${d.attempts} failed: ${d.lastError}${when}` : 'queued'
  }
  return d.lastError || 'failed'
}

const PILL: Record<Delivery['status'], string> = {
  ok: 'pill-ok',
  pending: 'pill-neutral',
  failed: 'pill-warning',
}

export function NotifierDeliveries({ id }: { id: number }) {
  // Polled while open: a pending row is waiting on a timer nobody else is
  // going to tell us about.
  const deliveries = useQuery({
    queryKey: ['deliveries', id],
    queryFn: () => getDeliveries(id),
    refetchInterval: 5000,
  })

  if (deliveries.isPending) return <p className="muted">Loading deliveries…</p>
  if (deliveries.isError) {
    return <p className="muted">Could not read deliveries: {(deliveries.error as Error).message}</p>
  }
  const rows = deliveries.data ?? []
  if (rows.length === 0) {
    return <p className="muted">No deliveries yet — this notifier fires after an import.</p>
  }

  return (
    <table>
      <thead>
        <tr>
          <th>When</th>
          <th>Event</th>
          <th>Status</th>
          <th>Detail</th>
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
          </tr>
        ))}
      </tbody>
    </table>
  )
}
