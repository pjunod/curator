import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { getCalendar } from '../api'
import type { CalendarEntry } from '../api'
import { useIsMobile } from '../useIsMobile'

function iso(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

const DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
const MONTHS = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
]

// A real month-grid calendar (Sonarr-style): the grid always renders;
// airing episodes and movie/book release dates land on their days.
export function CalendarPage() {
  const isMobile = useIsMobile()
  const [anchor, setAnchor] = useState(() => {
    const d = new Date()
    return new Date(d.getFullYear(), d.getMonth(), 1)
  })

  // Grid spans full weeks around the month.
  const gridStart = new Date(anchor)
  gridStart.setDate(1 - gridStart.getDay())
  const monthEnd = new Date(anchor.getFullYear(), anchor.getMonth() + 1, 0)
  const gridEnd = new Date(monthEnd)
  gridEnd.setDate(monthEnd.getDate() + (6 - monthEnd.getDay()))

  const entries = useQuery({
    queryKey: ['calendar', iso(gridStart), iso(gridEnd)],
    queryFn: () => getCalendar(iso(gridStart), iso(gridEnd)),
  })

  const byDate = new Map<string, CalendarEntry[]>()
  for (const e of entries.data ?? []) {
    const key = e.date.slice(0, 10)
    byDate.set(key, [...(byDate.get(key) ?? []), e])
  }

  const days: Date[] = []
  for (let d = new Date(gridStart); d <= gridEnd; d.setDate(d.getDate() + 1)) {
    days.push(new Date(d))
  }
  const today = iso(new Date())
  const shiftMonth = (n: number) =>
    setAnchor(new Date(anchor.getFullYear(), anchor.getMonth() + n, 1))

  return (
    <>
      <header className="page-head">
        <h1>Calendar</h1>
        <div className="head-actions">
          <button onClick={() => shiftMonth(-1)}>←</button>
          <button
            onClick={() => {
              const d = new Date()
              setAnchor(new Date(d.getFullYear(), d.getMonth(), 1))
            }}
          >
            Today
          </button>
          <button onClick={() => shiftMonth(1)}>→</button>
        </div>
      </header>
      <h2 style={{ marginBottom: 10 }}>
        {MONTHS[anchor.getMonth()]} {anchor.getFullYear()}
        {entries.isFetching && <span className="muted"> · loading…</span>}
      </h2>
      {entries.isError && (
        <div className="banner warning">{String((entries.error as Error).message)}</div>
      )}

      {isMobile ? (
        // Phones: a 7-column grid is unreadable at 390px — agenda instead,
        // only the days that have something, same month paging.
        <div className="cal-agenda">
          {days
            .filter((d) => d.getMonth() === anchor.getMonth() && (byDate.get(iso(d))?.length ?? 0) > 0)
            .map((d) => {
              const key = iso(d)
              return (
                <div key={key} className={`cal-day${key === today ? ' cal-today' : ''}`}>
                  <div className="cal-day-head">
                    {DOW[d.getDay()]} {d.getDate()}
                    {key === today && <span className="muted"> · today</span>}
                  </div>
                  {byDate.get(key)!.map((e, i) => (
                    <Link
                      key={`${e.mediaItemId}-${i}`}
                      to="/library/$id"
                      params={{ id: String(e.mediaItemId) }}
                      className={`cal-entry${e.hasFile ? ' cal-have' : ''}`}
                    >
                      <span className="cal-entry-title">{e.title}</span>
                      {e.detail && <span className="cal-entry-detail">{e.detail}</span>}
                    </Link>
                  ))}
                </div>
              )
            })}
          {(entries.data?.length ?? 0) === 0 && !entries.isFetching && (
            <p className="muted">Nothing airing or releasing this month.</p>
          )}
        </div>
      ) : (
        <div className="cal-grid">
          {DOW.map((d) => (
            <div key={d} className="cal-dow">
              {d}
            </div>
          ))}
          {days.map((d) => {
            const key = iso(d)
            const inMonth = d.getMonth() === anchor.getMonth()
            const dayEntries = byDate.get(key) ?? []
            return (
              <div
                key={key}
                className={`cal-cell${inMonth ? '' : ' cal-out'}${key === today ? ' cal-today' : ''}`}
              >
                <div className="cal-daynum">{d.getDate()}</div>
                {dayEntries.map((e, i) => (
                  <Link
                    key={`${e.mediaItemId}-${i}`}
                    to="/library/$id"
                    params={{ id: String(e.mediaItemId) }}
                    className={`cal-entry${e.hasFile ? ' cal-have' : ''}`}
                    title={`${e.title}${e.detail ? ` — ${e.detail}` : ''} (${e.kind}${e.hasFile ? ', on disk' : ''})`}
                  >
                    <span className="cal-entry-title">{e.title}</span>
                    {e.detail && <span className="cal-entry-detail">{e.detail}</span>}
                  </Link>
                ))}
              </div>
            )
          })}
        </div>
      )}
      <p className="muted" style={{ marginTop: 10 }}>
        Episode air dates and movie/book release dates for items in your library.
        Filled entries are on disk; outlined ones aren't (yet).
      </p>
    </>
  )
}
