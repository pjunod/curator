import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { BusEvent } from '../api'
import { fmtInterval, fmtRelative, getHealth, getTasks, runTask } from '../api'

const MAX_EVENTS = 50

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
