import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { fmtRelative, getTasks, getWanted, runTask } from '../api'

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
                  </td>
                  <td className="muted mono">{w.wantableId.split(':')[0]}</td>
                  <td>
                    {w.missing ? (
                      <span className="pill pill-warning">missing</span>
                    ) : (
                      <span className="pill pill-neutral">upgrade from {w.current}</span>
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
