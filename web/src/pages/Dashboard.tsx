import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fmtDuration, getHealth, getStatus } from '../api'

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
  const now = useNow()

  const s = status.data
  const uptime = s ? (now.getTime() - new Date(s.startedAt).getTime()) / 1000 : undefined

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
        <div className="card error-card">Could not reach the Monarr API: {String(status.error)}</div>
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

      <section className="card prose">
        <h2>Walking skeleton</h2>
        <p>
          This is <strong>Phase 0</strong> of Monarr — a unified rewrite of Sonarr + Radarr as one
          Go binary. The scaffold you are looking at already runs the real spine: embedded React
          shell, spec-first <code>/api/v1</code>, SQLite with migrations, a typed event bus
          streaming to this UI over SSE, a persistent task scheduler, and health checks.
        </p>
        <p>
          Next up — <em>Phase 1: Library</em> — TMDB metadata, adding movies &amp; series, root
          folders, and disk reconcile. See <code>docs/architecture.md</code> in the repo for the
          full blueprint.
        </p>
      </section>
    </>
  )
}
