import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { fmtDuration, getHealth, getLibrary, getQueue, getStatus, getWanted } from '../api'

function useNow(intervalMs = 1000): Date {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), intervalMs)
    return () => clearInterval(id)
  }, [intervalMs])
  return now
}

export function Dashboard() {
  const status = useQuery({ queryKey: ['status'], queryFn: getStatus, refetchInterval: 15_000 })
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth, refetchInterval: 30_000 })
  const library = useQuery({ queryKey: ['library', 'all'], queryFn: () => getLibrary(), refetchInterval: 60_000 })
  const wanted = useQuery({ queryKey: ['wanted'], queryFn: getWanted, refetchInterval: 60_000 })
  const queue = useQuery({ queryKey: ['queue'], queryFn: getQueue, refetchInterval: 30_000 })
  const now = useNow()

  const s = status.data
  const uptime = s ? (now.getTime() - new Date(s.startedAt).getTime()) / 1000 : undefined
  const countKind = (k: string) => library.data?.filter((m) => m.kind === k).length ?? 0
  const activeDownloads =
    queue.data?.filter((q) => ['grabbed', 'downloading', 'importing'].includes(q.state)).length ?? 0

  return (
    <>
      <header className="page-head">
        <h1>Dashboard</h1>
        {health.data && (
          <span className={`pill pill-${health.data.overall}`}>
            {health.data.overall === 'ok' ? 'healthy' : health.data.overall}
          </span>
        )}
      </header>

      {status.isError && (
        <div className="card error-card">Could not reach the Curator API: {String(status.error)}</div>
      )}

      <section className="cards">
        <div className="card">
          <div className="card-label">Version</div>
          <div className="card-value">{s?.version ?? '…'}</div>
          <div className="card-sub">commit {s?.commit ?? '…'}</div>
        </div>
        <div className="card">
          <div className="card-label">Uptime</div>
          <div className="card-value">{uptime !== undefined ? fmtDuration(uptime) : '…'}</div>
          <div className="card-sub">
            since {s ? new Date(s.startedAt).toLocaleString() : '…'}
          </div>
        </div>
        <div className="card">
          <div className="card-label">Runtime</div>
          <div className="card-value">{s ? `${s.os}/${s.arch}` : '…'}</div>
          <div className="card-sub">{s?.goVersion ?? '…'}</div>
        </div>
        <div className="card">
          <div className="card-label">Database</div>
          <div className="card-value">schema v{s?.dbSchemaVersion ?? '…'}</div>
          <div className="card-sub">SQLite · {s?.dataDir ?? '…'}</div>
        </div>
      </section>

      <section className="cards">
        <Link to="/" className="card" style={{ textDecoration: 'none', color: 'inherit' }}>
          <div className="card-label">Library</div>
          <div className="card-value">{library.data?.length ?? '…'}</div>
          <div className="card-sub">
            {countKind('movie')} movies · {countKind('series')} series · {countKind('book')} books
          </div>
        </Link>
        <Link to="/wanted" className="card" style={{ textDecoration: 'none', color: 'inherit' }}>
          <div className="card-label">Wanted</div>
          <div className="card-value">{wanted.data?.length ?? '…'}</div>
          <div className="card-sub">missing or below cutoff — the loops hunt these</div>
        </Link>
        <Link to="/activity" className="card" style={{ textDecoration: 'none', color: 'inherit' }}>
          <div className="card-label">Downloads</div>
          <div className="card-value">{activeDownloads}</div>
          <div className="card-sub">in flight (grabbed / downloading / importing)</div>
        </Link>
      </section>
    </>
  )
}
