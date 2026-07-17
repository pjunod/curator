import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { fmtRelative, getQueue, removeQueueItem } from '../api'

const STATE_PILL: Record<string, string> = {
  imported: 'pill-ok',
  failed: 'pill-error',
  downloading: 'pill-neutral',
  importing: 'pill-neutral',
  grabbed: 'pill-neutral',
  completed: 'pill-neutral',
}

export function ActivityPage() {
  const qc = useQueryClient()
  const queue = useQuery({ queryKey: ['queue'], queryFn: getQueue, refetchInterval: 4_000 })
  const remove = useMutation({
    mutationFn: (id: number) => removeQueueItem(id, false),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['queue'] }),
  })

  return (
    <>
      <header className="page-head">
        <h1>Activity</h1>
      </header>
      <section className="panel">
        {queue.data?.length === 0 && (
          <p className="muted">Nothing in the queue. Grab something from a title's search.</p>
        )}
        {queue.data && queue.data.length > 0 && (
          <table>
            <thead>
              <tr>
                <th>Release</th>
                <th>Quality</th>
                <th>State</th>
                <th>Progress</th>
                <th>Added</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {queue.data.map((d) => (
                <tr key={d.id}>
                  <td className="mono">
                    {d.title}
                    {d.error && <div className="error-text">{d.error}</div>}
                  </td>
                  <td className="muted">{d.quality}</td>
                  <td>
                    <span className={`pill ${STATE_PILL[d.state] ?? 'pill-neutral'}`}>{d.state}</span>
                  </td>
                  <td style={{ minWidth: 120 }}>
                    <div className="progress-track">
                      <i style={{ width: `${Math.round(d.progress * 100)}%` }} />
                    </div>
                  </td>
                  <td className="muted">{fmtRelative(d.addedAt)}</td>
                  <td>
                    <button onClick={() => remove.mutate(d.id)} disabled={remove.isPending}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
      <p className="muted" style={{ fontSize: 13 }}>
        The queue refreshes every 30s from the download clients (task{' '}
        <span className="mono">queue.refresh</span>); completed downloads import automatically —
        renamed files land in the library and appear on each title's page.
      </p>
    </>
  )
}
