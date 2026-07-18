import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { autoSearchItem, fmtRelative, getTasks, getWanted, runTask } from '../api'

// WantedPage makes the automation visible: everything missing or below
// cutoff, plus when the loops that hunt for it last ran / run next.
export function WantedPage() {
  const qc = useQueryClient()
  const wanted = useQuery({ queryKey: ['wanted'], queryFn: getWanted, refetchInterval: 30_000 })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 15_000 })

  const backlog = useMutation({
    mutationFn: () => runTask('backlog.search'),
    onSuccess: () => {
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: ['wanted'] })
        void qc.invalidateQueries({ queryKey: ['tasks'] })
      }, 2000)
    },
  })

  // Per-item automatic search; remembers which items were kicked off.
  const [kicked, setKicked] = useState<Set<number>>(new Set())
  const one = useMutation({
    mutationFn: (itemId: number) => autoSearchItem(itemId),
    onSuccess: (_data, itemId) => {
      setKicked((prev) => new Set(prev).add(itemId))
      setTimeout(() => void qc.invalidateQueries({ queryKey: ['wanted'] }), 4000)
    },
  })

  const loop = (name: string) => tasks.data?.find((t) => t.name === name)
  const rss = loop('rss.sync')
  const bl = loop('backlog.search')
  const now = new Date()

  return (
    <>
      <header className="page-head">
        <h1>Wanted</h1>
        <div className="head-actions">
          <button onClick={() => backlog.mutate()} disabled={backlog.isPending}>
            Search all now
          </button>
        </div>
      </header>

      <p className="muted">
        Everything monitored that's missing or below its quality cutoff. Two loops work this
        list automatically: RSS sync
        {rss && <> (last {fmtRelative(rss.lastRunAt, now)}, next {fmtRelative(rss.nextRunAt, now)})</>}
        {' '}grabs new releases as indexers publish them, and the backlog search
        {bl && <> (last {fmtRelative(bl.lastRunAt, now)})</>} actively hunts for the rest.
      </p>

      {wanted.data?.length === 0 && (
        <div className="banner">
          Nothing wanted — everything monitored is on disk at or above its cutoff.
        </div>
      )}

      {(wanted.data?.length ?? 0) > 0 && (
        <section className="panel">
          <table>
            <thead>
              <tr>
                <th>Title</th>
                <th></th>
                <th>Status</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {wanted.data!.map((w) => (
                <tr key={w.wantableId}>
                  <td>
                    <Link to="/library/$id" params={{ id: String(w.mediaItemId) }}>
                      {w.title}
                    </Link>{' '}
                    <span className="muted">{w.detail}</span>
                    {w.copy && (
                      <span className="pill pill-info" title="This entry is for an additional quality copy">
                        {w.copy}
                      </span>
                    )}
                  </td>
                  <td className="muted mono">{w.wantableId.split(':')[0]}</td>
                  <td>
                    {w.missing ? (
                      <span className="pill pill-warning">missing</span>
                    ) : (
                      <span className="pill pill-neutral">upgrade from {w.current}</span>
                    )}
                  </td>
                  <td>
                    {kicked.has(w.mediaItemId) ? (
                      <span className="muted">searching…</span>
                    ) : (
                      <button
                        title="Automatic search for this item — best accepted release is grabbed"
                        disabled={one.isPending}
                        onClick={() => one.mutate(w.mediaItemId)}
                      >
                        Search
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </>
  )
}
