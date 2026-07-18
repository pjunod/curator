import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { getCalendar } from '../api'
import type { CalendarEntry } from '../api'

function iso(d: Date): string {
  return d.toISOString().slice(0, 10)
}

function shiftDays(base: Date, days: number): Date {
  const d = new Date(base)
  d.setDate(d.getDate() + days)
  return d
}

// Agenda-style calendar: airing episodes and movie/book releases grouped by
// day over a sliding window.
export function CalendarPage() {
  const [anchor, setAnchor] = useState(() => new Date())
  const start = shiftDays(anchor, -7)
  const end = shiftDays(anchor, 30)

  const entries = useQuery({
    queryKey: ['calendar', iso(start), iso(end)],
    queryFn: () => getCalendar(iso(start), iso(end)),
  })

  const byDate = new Map<string, CalendarEntry[]>()
  for (const e of entries.data ?? []) {
    const key = e.date.slice(0, 10)
    byDate.set(key, [...(byDate.get(key) ?? []), e])
  }
  const dates = [...byDate.keys()].sort()
  const today = iso(new Date())

  return (
    <>
      <header className="page-head">
        <h1>Calendar</h1>
        <div className="head-actions">
          <button onClick={() => setAnchor(shiftDays(anchor, -30))}>← Earlier</button>
          <button onClick={() => setAnchor(new Date())}>Today</button>
          <button onClick={() => setAnchor(shiftDays(anchor, 30))}>Later →</button>
        </div>
      </header>
      <p className="muted">
        {iso(start)} → {iso(end)}
      </p>

      {entries.isLoading && <p className="muted">Loading…</p>}
      {entries.data?.length === 0 && (
        <p className="muted">Nothing airing or releasing in this window.</p>
      )}

      {dates.map((date) => (
        <section key={date} className="panel">
          <h2>
            {date}
            {date === today && <span className="pill pill-ok" style={{ marginLeft: 8 }}>today</span>}
          </h2>
          <table>
            <tbody>
              {byDate.get(date)!.map((e, i) => (
                <tr key={`${e.mediaItemId}-${e.detail}-${i}`}>
                  <td style={{ width: 90 }}>
                    <span className="pill pill-neutral">{e.kind}</span>
                  </td>
                  <td>
                    <Link to="/library/$id" params={{ id: String(e.mediaItemId) }}>
                      {e.title}
                    </Link>{' '}
                    {e.detail && <span className="muted">{e.detail}</span>}
                  </td>
                  <td style={{ width: 60 }}>
                    {e.hasFile ? (
                      <span className="pill pill-ok">✓</span>
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      ))}
    </>
  )
}
