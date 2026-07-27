import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { BusEvent } from '../api'
import type { Connection } from '../api'
import type { Transfer } from '../api'
import {
  fmtBytes,
  getTransfers,
  fmtInterval,
  fmtRelative,
  getBackups,
  getConnections,
  getHealth,
  getTasks,
  runTask,
} from '../api'

const MAX_EVENTS = 50

// The Connections card (plan §5.7): one row per remote application, and the
// single screen that answers "are the three apps actually talking right
// now". Everything here comes from the last health probe plus the push
// supervisor's live state — opening four connections every few seconds to
// render a status page would make the page part of the problem.
const STATE_PILL: Record<Connection['state'], string> = {
  live: 'pill-ok',
  polling: 'pill-ok',
  degraded: 'pill-warning',
  unreachable: 'pill-warning',
  unprobed: 'pill-neutral',
  calling: 'pill-ok',
  quiet: 'pill-neutral',
}

// What the word means, for anyone who has not read the plan. `polling` is
// not a lesser `live`: it is the fallback working exactly as designed.
const STATE_TITLE: Record<Connection['state'], string> = {
  live: 'A push stream is open — completions arrive the moment they happen',
  polling: 'Answering, on the 30-second poll',
  degraded: 'Answering, but not working properly',
  unreachable: 'Not answering',
  unprobed: 'Configured, but never probed — its only test is the action itself',
  calling: 'This application has called Monarr recently',
  quiet: 'It has called since Monarr started, but not lately',
}

// Direction stated, not inferred.
//
// One application legitimately appears twice — plurx is both something Monarr
// pushes to and something that calls Monarr — and when both rows carry the
// same name, one green and one amber, the second reads as a stale duplicate of
// the first. It is not: they are two settings that fail independently, and
// conflating them sends you to fix the half that already works. So the Kind
// cell leads with the direction instead of leaving it to be deduced from
// "media server" versus "calls Monarr".
const KIND_LABEL: Record<Connection['kind'], string> = {
  downloadclient: '→ download client',
  mediaserver: '→ media server',
  inbound: '← calls Monarr',
}

const KIND_TITLE: Record<Connection['kind'], string> = {
  downloadclient: 'Outbound: Monarr polls this client for queue and history',
  mediaserver: 'Outbound: Monarr tells this server when an import finishes',
  inbound: 'Inbound: this application calls Monarr. Configured on its side, not here',
}

// The data plane, on its own card.
//
// Deliberately not merged into Connections. That panel answers "can Monarr
// still talk to these applications"; this answers "is anything actually
// moving". They were the same question once, and the result was a 20 GB
// import reported as a degraded download client while being visible nowhere
// else at all — the only evidence anywhere was the duration column of an
// unrelated scheduled task.
const STAGE_LABEL: Record<Transfer['stage'], string> = {
  downloading: 'downloading',
  importing: 'importing',
  notifying: 'notifying',
}

const STAGE_TITLE: Record<Transfer['stage'], string> = {
  downloading: 'nzbd is fetching it — Monarr is watching, not working',
  importing: 'Monarr is moving bytes into the library right now',
  notifying: 'telling plurx what landed, including the waits between retries',
}

function rate(bps?: number): string {
  if (!bps || bps <= 0) return ''
  return `${fmtBytes(bps)}/s`
}

function TransfersCard() {
  const q = useQuery({
    queryKey: ['transfers'],
    queryFn: getTransfers,
    // Faster than the other panels on purpose: this is the one that moves.
    refetchInterval: 2_000,
  })
  const rows = q.data ?? []

  return (
    <section className="panel">
      <h2>In flight</h2>
      <p className="muted">
        What is moving right now, and which seam it is at. Separate from
        Connections above: that one says whether Monarr can still talk to the
        other applications, this one says whether anything is actually moving.
      </p>
      {rows.length === 0 ? (
        <p className="muted">Nothing in flight.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Title</th>
              <th>Stage</th>
              <th>Progress</th>
              <th>Elapsed</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((t) => {
              // Absent totals are not 0% — a stage that cannot measure itself
              // must not draw an empty bar suggesting nothing has happened.
              const known = !!t.total && t.total > 0
              const pct = known ? Math.min(100, Math.round((100 * (t.bytes ?? 0)) / t.total!)) : 0
              return (
                <tr key={`${t.downloadId}:${t.stage}`}>
                  <td>
                    {t.title}
                    {t.transfer && <div className="muted mono">{t.transfer}</div>}
                  </td>
                  <td className="muted nowrap" title={STAGE_TITLE[t.stage]}>
                    {STAGE_LABEL[t.stage] ?? t.stage}
                    {t.peer ? ` · ${t.peer}` : ''}
                  </td>
                  <td>
                    {known ? (
                      <>
                        <div className="progress-track">
                          <i style={{ width: `${pct}%` }} />
                        </div>
                        <div className="muted nowrap">
                          {fmtBytes(t.bytes ?? 0)} / {fmtBytes(t.total!)}
                          {rate(t.bytesPerSecond) ? ` · ${rate(t.bytesPerSecond)}` : ''}
                        </div>
                      </>
                    ) : (
                      <span className="muted">not measurable</span>
                    )}
                  </td>
                  <td className="muted nowrap">{fmtRelative(t.startedAt)}</td>
                  <td className="muted">{t.detail ?? ''}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </section>
  )
}

function ConnectionsCard() {
  const q = useQuery({
    queryKey: ['connections'],
    queryFn: getConnections,
    refetchInterval: 10_000,
  })
  const rows = q.data?.connections ?? []

  return (
    <section className="panel">
      <h2>Connections</h2>
      <p className="muted">
        The other applications Monarr talks to (→) — and the ones that talk to it
        (←). One application can appear as both; the two directions are separate
        settings and fail separately.{' '}
        {q.data?.checkedAt
          ? `Last probed ${fmtRelative(new Date(q.data.checkedAt).toISOString())}.`
          : 'Not probed yet — the health check runs every minute.'}
      </p>
      {rows.length === 0 ? (
        <p className="muted">
          No download clients or media servers configured yet (Settings).
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>State</th>
              <th>Last contact</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((c) => (
              <tr key={`${c.kind}:${c.name}`}>
                <td>
                  {c.name}
                  <div className="muted">
                    {c.type}
                    {c.version ? ` ${c.version}` : ''}
                    {c.url ? ` · ${c.url}` : ''}
                  </div>
                </td>
                <td className="muted nowrap" title={KIND_TITLE[c.kind] ?? ''}>
                  {KIND_LABEL[c.kind] ?? c.kind}
                </td>
                <td>
                  <span className={`pill ${STATE_PILL[c.state]}`} title={STATE_TITLE[c.state]}>
                    {c.state}
                  </span>
                </td>
                <td className="muted">
                  {c.lastContact ? fmtRelative(new Date(c.lastContact).toISOString()) : '—'}
                  {c.lastEventSeq ? ` · event #${c.lastEventSeq}` : ''}
                </td>
                <td className="muted">{c.detail ?? ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function useEventStream(): { events: BusEvent[]; connected: boolean } {
  const [events, setEvents] = useState<BusEvent[]>([])
  const [connected, setConnected] = useState(false)
  const sourceRef = useRef<EventSource | null>(null)

  useEffect(() => {
    const es = new EventSource('/api/v1/events')
    sourceRef.current = es
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    es.onmessage = (msg) => {
      try {
        const e = JSON.parse(msg.data) as BusEvent
        setEvents((prev) => [e, ...prev].slice(0, MAX_EVENTS))
      } catch {
        // ignore malformed frames
      }
    }
    return () => {
      es.close()
      sourceRef.current = null
    }
  }, [])

  return { events, connected }
}

export function SystemPage() {
  const qc = useQueryClient()
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth, refetchInterval: 30_000 })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 5_000 })
  const backups = useQuery({ queryKey: ['backups'], queryFn: getBackups, refetchInterval: 60_000 })
  const { events, connected } = useEventStream()

  const trigger = useMutation({
    mutationFn: runTask,
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['tasks'] })
      void qc.invalidateQueries({ queryKey: ['health'] })
    },
  })

  return (
    <>
      <header className="page-head">
        <h1>System</h1>
      </header>

      <ConnectionsCard />

      <TransfersCard />

      <section className="panel">
        <h2>Health</h2>
        <table>
          <thead>
            <tr>
              <th>Check</th>
              <th>Status</th>
              <th>Message</th>
              <th>Checked</th>
            </tr>
          </thead>
          <tbody>
            {health.data?.checks.map((c) => (
              <tr key={c.name}>
                <td className="mono">{c.name}</td>
                <td>
                  <span className={`pill pill-${c.status}`}>{c.status}</span>
                </td>
                <td className="muted">{c.message ?? ''}</td>
                <td className="muted">{fmtRelative(c.checkedAt)}</td>
              </tr>
            ))}
            {health.isLoading && (
              <tr>
                <td colSpan={4} className="muted">
                  Running checks…
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </section>

      <section className="panel">
        <h2>Scheduled tasks</h2>
        <table>
          <thead>
            <tr>
              <th>Task</th>
              <th>Interval</th>
              <th>Last run</th>
              <th>Duration</th>
              <th>Next run</th>
              <th>Last error</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {tasks.data?.map((t) => (
              <tr key={t.name}>
                <td className="mono">
                  {t.name}
                  {t.running && <span className="dot dot-running" aria-label="running" />}
                </td>
                <td className="muted">{fmtInterval(t.intervalSeconds)}</td>
                <td className="muted">{fmtRelative(t.lastRunAt)}</td>
                <td className="muted">{t.lastDurationMs !== undefined ? `${t.lastDurationMs} ms` : '—'}</td>
                <td className="muted">{fmtRelative(t.nextRunAt)}</td>
                <td className="error-text">{t.lastError ?? ''}</td>
                <td>
                  <button
                    onClick={() => trigger.mutate(t.name)}
                    disabled={trigger.isPending || t.running}
                  >
                    Run now
                  </button>
                </td>
              </tr>
            ))}
            {tasks.isLoading && (
              <tr>
                <td colSpan={7} className="muted">
                  Loading…
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </section>

      <section className="panel">
        <h2>Backups</h2>
        <p className="muted">
          Daily online snapshots (VACUUM INTO), newest first, last 7 kept. Run one now via
          the <code>backup.run</code> task above.
        </p>
        {backups.data?.length === 0 ? (
          <p className="muted">No backups yet.</p>
        ) : (
          <table>
            <tbody>
              {backups.data?.map((b) => (
                <tr key={b.name}>
                  <td className="mono">{b.name}</td>
                  <td className="muted">{fmtBytes(b.sizeBytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="panel">
        <h2>
          Live events
          <span
            className={`dot ${connected ? 'dot-ok' : 'dot-error'}`}
            title={connected ? 'SSE connected' : 'SSE disconnected'}
          />
        </h2>
        {events.length === 0 ? (
          <p className="muted">
            Waiting for events… (try “Run now” on a task — its completion will appear here via the
            event bus → SSE)
          </p>
        ) : (
          <ul className="event-log">
            {events.map((e, i) => (
              <li key={`${e.ts}-${i}`}>
                <span className="event-type">{e.type}</span>
                <span className="muted"> {new Date(e.ts).toLocaleTimeString()} </span>
                <code>{JSON.stringify(e.payload)}</code>
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  )
}
