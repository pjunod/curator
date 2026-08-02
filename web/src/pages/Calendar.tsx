import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { getCalendar, posterUrl } from '../api'
import type { CalendarEntry } from '../api'
import { useIsMobile } from '../useIsMobile'
import { useInView } from '../useInView'
import {
  AGENDA_STEP_DAYS,
  MONTH_CELL_CHIPS,
  MONTHS,
  compareEntries,
  dayLabel,
  entryState,
  entrySubtitle,
  extendBackward,
  extendForward,
  formatTime,
  groupByDay,
  initialWindow,
  isUnmonitored,
  iso,
  monthGrid,
  parseISODate,
  windowLabel,
} from '../calendar'
import type { DayGroup, EntryState } from '../calendar'

const DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
const VIEW_KEY = 'monarr-cal-view'

type View = 'month' | 'agenda'

const STATE_CLASS: Record<EntryState, string> = {
  have: 'cal-have',
  missing: 'cal-missing',
  upcoming: 'cal-upcoming',
}

const STATE_PILL: Record<EntryState, { cls: string; label: string }> = {
  have: { cls: 'pill pill-ok', label: 'On disk' },
  missing: { cls: 'pill pill-error', label: 'Missing' },
  upcoming: { cls: 'pill pill-outline', label: 'Upcoming' },
}

// Kind is a glyph, never a color: color is spoken for by state, and two
// meanings on one channel is how a legend stops being readable. Episodes —
// the common case — get nothing at all, so the grid stays quiet.
const KIND_GLYPH: Record<string, string> = { movie: '▸', book: '▪' }

function storedView(): View {
  try {
    return localStorage.getItem(VIEW_KEY) === 'agenda' ? 'agenda' : 'month'
  } catch {
    // Private mode, or a browser with storage disabled. The toggle still
    // works for this visit; it just does not survive a reload.
    return 'month'
  }
}

// ---- the shared row: agenda, day panel, and the model for the native app ----

function EntryRow({ e, showDate = false }: { e: CalendarEntry; showDate?: boolean }) {
  const state = entryState(e)
  const pill = STATE_PILL[state]
  const poster = e.posterPath ? posterUrl(e.posterPath, 'w185') : ''
  const time = formatTime(e.airDateUtc)
  const dimmed = isUnmonitored(e)
  return (
    <Link
      to="/library/$id"
      params={{ id: String(e.mediaItemId) }}
      className={`cal-row${dimmed ? ' cal-unmonitored' : ''}`}
    >
      {poster ? (
        <img className="cal-row-poster" src={poster} alt="" loading="lazy" />
      ) : (
        <div className="cal-row-poster poster-fallback small">{e.title.slice(0, 1)}</div>
      )}
      <div className="cal-row-main">
        <div className="cal-row-title">
          {KIND_GLYPH[e.kind] && <span className="cal-kind">{KIND_GLYPH[e.kind]}</span>}
          {e.title}
        </div>
        <div className="cal-row-sub">
          {entrySubtitle(e)}
          {dimmed && <span className="cal-row-note"> · Not monitored</span>}
        </div>
      </div>
      <div className="cal-row-rail">
        <div className="cal-row-when">{showDate ? e.date.slice(0, 10) : time || '—'}</div>
        {e.network && <div className="cal-row-network">{e.network}</div>}
        <span className={pill.cls}>{pill.label}</span>
      </div>
    </Link>
  )
}

// ---- month view ----

function DayPanel({ group, onClose }: { group: DayGroup; onClose: () => void }) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal cal-day-modal" onClick={(ev) => ev.stopPropagation()}>
        <header className="modal-head">
          <h2>{dayLabel(group.key)}</h2>
          <button onClick={onClose}>Close</button>
        </header>
        <div className="modal-body cal-rows">
          {group.entries.map((e, i) => (
            <EntryRow key={`${e.mediaItemId}-${i}`} e={e} />
          ))}
        </div>
      </div>
    </div>
  )
}

function MonthView({
  anchor,
  entries,
  onShiftMonth,
}: {
  anchor: Date
  entries: CalendarEntry[]
  onShiftMonth: (n: number) => void
}) {
  const [openDay, setOpenDay] = useState<string | null>(null)
  const { days } = monthGrid(anchor)
  const byDate = useMemo(() => {
    const m = new Map<string, CalendarEntry[]>()
    for (const e of entries) {
      const key = e.date.slice(0, 10)
      const list = m.get(key)
      if (list) list.push(e)
      else m.set(key, [e])
    }
    for (const list of m.values()) list.sort(compareEntries)
    return m
  }, [entries])
  const today = iso(new Date())
  const open = openDay ? { key: openDay, date: parseISODate(openDay), entries: byDate.get(openDay) ?? [] } : null

  return (
    <>
      <h2 className="cal-month-title">
        <button className="cal-nav" onClick={() => onShiftMonth(-1)} aria-label="Previous month">
          ←
        </button>
        {MONTHS[anchor.getMonth()]} {anchor.getFullYear()}
        <button className="cal-nav" onClick={() => onShiftMonth(1)} aria-label="Next month">
          →
        </button>
      </h2>
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
          const shown = dayEntries.slice(0, MONTH_CELL_CHIPS)
          const rest = dayEntries.length - shown.length
          return (
            <div
              key={key}
              className={`cal-cell${inMonth ? '' : ' cal-out'}${key === today ? ' cal-today' : ''}`}
            >
              <div className="cal-daynum">{d.getDate()}</div>
              {shown.map((e, i) => {
                const time = formatTime(e.airDateUtc)
                return (
                  <Link
                    key={`${e.mediaItemId}-${i}`}
                    to="/library/$id"
                    params={{ id: String(e.mediaItemId) }}
                    className={
                      `cal-entry ${STATE_CLASS[entryState(e)]}` +
                      (isUnmonitored(e) ? ' cal-unmonitored' : '')
                    }
                    title={`${e.title}${e.detail ? ` — ${e.detail}` : ''}${
                      e.network ? ` (${e.network})` : ''
                    }`}
                  >
                    {KIND_GLYPH[e.kind] && <span className="cal-kind">{KIND_GLYPH[e.kind]}</span>}
                    {time && <span className="cal-entry-time">{time}</span>}
                    <span className="cal-entry-title">{e.title}</span>
                    {e.detail && <span className="cal-entry-detail">{e.detail}</span>}
                  </Link>
                )
              })}
              {rest > 0 && (
                <button className="cal-more" onClick={() => setOpenDay(key)}>
                  +{rest} more
                </button>
              )}
            </div>
          )
        })}
      </div>
      <div className="cal-legend muted">
        <span className="cal-swatch cal-have" /> On disk
        <span className="cal-swatch cal-missing" /> Aired, missing
        <span className="cal-swatch cal-upcoming" /> Upcoming
        <span className="cal-swatch cal-upcoming cal-unmonitored" /> Not monitored
        <span className="cal-legend-sep">·</span>
        <span className="cal-kind">▸</span> Movie
        <span className="cal-kind">▪</span> Book
      </div>
      {open && <DayPanel group={open} onClose={() => setOpenDay(null)} />}
    </>
  )
}

// ---- agenda view ----

// How many times the sentinel widens the window on its own before handing
// over to a button. Six steps is half a year past the initial six weeks —
// far enough that scrolling feels endless, bounded enough that a library
// with nothing scheduled cannot spin the window out to the 2040s.
const MAX_AUTO_STEPS = 6

function AgendaView({
  anchor,
  entries,
  loading,
  onEarlier,
  onLater,
  horizon,
  autoSteps,
}: {
  anchor: Date
  entries: CalendarEntry[]
  loading: boolean
  onEarlier: () => void
  onLater: () => void
  horizon: string
  autoSteps: number
}) {
  const anchorKey = iso(anchor)
  const groups = useMemo(() => groupByDay(entries, anchorKey), [entries, anchorKey])
  const anchorRef = useRef<HTMLDivElement | null>(null)
  const jumped = useRef(false)

  // Open pinned at the anchor day rather than at the top: the window starts
  // a week in the past, and a list that opens on last Tuesday reads as
  // stale. Once only — re-centring on every widen would fight the reader.
  useEffect(() => {
    if (jumped.current || !anchorRef.current || groups.length === 0) return
    jumped.current = true
    anchorRef.current.scrollIntoView({ block: 'start' })
  }, [groups.length])

  if (loading && entries.length === 0) {
    return (
      <div className="cal-agenda">
        {Array.from({ length: 6 }, (_, i) => (
          <div key={i} className="cal-row cal-skeleton" aria-hidden="true" />
        ))}
      </div>
    )
  }

  return (
    <div className="cal-agenda">
      <div className="cal-agenda-top">
        <button onClick={onEarlier}>Load earlier</button>
        <span className="muted">{horizon}</span>
      </div>
      {groups.map((g) => (
        <section key={g.key} className={`cal-day${g.key === anchorKey ? ' cal-today' : ''}`}>
          <div className="cal-day-head" ref={g.key === anchorKey ? anchorRef : undefined}>
            {dayLabel(g.key, anchor)}
          </div>
          {g.entries.length === 0 ? (
            <p className="muted cal-day-empty">Nothing on this day.</p>
          ) : (
            <div className="cal-rows">
              {g.entries.map((e, i) => (
                <EntryRow key={`${e.mediaItemId}-${i}`} e={e} />
              ))}
            </div>
          )}
        </section>
      ))}
      {/* Keyed on the step so each widening remounts it: useInView latches by
          design, and a latched sentinel would extend the window exactly once.
          Past the auto budget it becomes a button — an empty stretch of
          calendar keeps the sentinel in view, so unbounded auto-loading
          would walk the window into next decade on its own. */}
      {autoSteps < MAX_AUTO_STEPS ? (
        <LoadMoreSentinel key={autoSteps} onSeen={onLater} label={loading ? 'Loading…' : horizon} />
      ) : (
        <div className="cal-agenda-end muted">
          <button onClick={onLater}>Load more</button>
        </div>
      )}
    </div>
  )
}

function LoadMoreSentinel({
  onSeen,
  label,
}: {
  onSeen: () => void
  label: string
}) {
  const { ref, seen } = useInView<HTMLDivElement>()
  const fired = useRef(false)
  useEffect(() => {
    if (seen && !fired.current) {
      fired.current = true
      onSeen()
    }
  }, [seen, onSeen])
  return (
    <div ref={ref} className="cal-agenda-end muted">
      {label}
    </div>
  )
}

// ---- the page ----

/** The two URL params, both optional. `view` deep-links (and overrides the
 *  stored choice); `date` anchors the month shown and the day the agenda
 *  opens at. */
export interface CalendarSearch {
  view?: View
  date?: string
}

export function CalendarPage() {
  const isMobile = useIsMobile()
  const search = useSearch({ from: '/calendar' })
  const navigate = useNavigate()

  // The anchor: ?date when given, otherwise today. It exists because a
  // rolling agenda opens at today and a fixture library's dates are not
  // today — but it is a real feature too, for linking at a week.
  const anchor = useMemo(
    () => (search.date ? parseISODate(search.date) : new Date()),
    [search.date],
  )

  const [stored, setStored] = useState<View>(storedView)
  // Below the breakpoint the page is always the agenda — a seven-column grid
  // at 390px is what the old code already refused to render.
  const view: View = isMobile ? 'agenda' : (search.view ?? stored)

  const setView = (v: View) => {
    setStored(v)
    try {
      localStorage.setItem(VIEW_KEY, v)
    } catch {
      // Storage refused; the choice still applies to this visit.
    }
    // Clearing ?view keeps the stored choice authoritative afterwards,
    // rather than pinning the URL to whatever was last clicked.
    void navigate({ to: '/calendar', search: (s: CalendarSearch) => ({ ...s, view: undefined }) })
  }

  const [window_, setWindow] = useState(() => initialWindow(anchor))
  const [autoSteps, setAutoSteps] = useState(0)
  const monthSpan = useMemo(() => monthGrid(anchor), [anchor])
  // Re-anchoring (a ?date change, or paging months) resets the agenda window.
  useEffect(() => {
    setWindow(initialWindow(anchor))
    setAutoSteps(0)
  }, [anchor])

  const range =
    view === 'month'
      ? { start: iso(monthSpan.start), end: iso(monthSpan.end) }
      : window_

  const entries = useQuery({
    queryKey: ['calendar', range.start, range.end],
    queryFn: () => getCalendar(range.start, range.end),
  })

  const shiftMonth = (n: number) => {
    const next = new Date(anchor.getFullYear(), anchor.getMonth() + n, 1)
    void navigate({ to: '/calendar', search: (s: CalendarSearch) => ({ ...s, date: iso(next) }) })
  }
  const goToday = () =>
    void navigate({ to: '/calendar', search: (s: CalendarSearch) => ({ ...s, date: undefined }) })

  const data = entries.data ?? []

  return (
    <>
      <header className="page-head">
        <h1>Calendar</h1>
        <div className="head-actions">
          {!isMobile && (
            <div className="tabs" role="group" aria-label="Calendar view">
              <button
                className={`tab${view === 'month' ? ' active' : ''}`}
                onClick={() => setView('month')}
              >
                Month
              </button>
              <button
                className={`tab${view === 'agenda' ? ' active' : ''}`}
                onClick={() => setView('agenda')}
              >
                Agenda
              </button>
            </div>
          )}
          <button onClick={goToday}>Today</button>
        </div>
      </header>

      {entries.isError && (
        <div className="banner warning">{String((entries.error as Error).message)}</div>
      )}

      {view === 'month' ? (
        <MonthView anchor={anchor} entries={data} onShiftMonth={shiftMonth} />
      ) : (
        <AgendaView
          anchor={anchor}
          entries={data}
          loading={entries.isFetching}
          horizon={windowLabel(window_, anchor)}
          autoSteps={autoSteps}
          onEarlier={() => setWindow((w) => extendBackward(w, AGENDA_STEP_DAYS))}
          onLater={() => {
            setWindow((w) => extendForward(w, AGENDA_STEP_DAYS))
            setAutoSteps((n) => n + 1)
          }}
        />
      )}
    </>
  )
}
