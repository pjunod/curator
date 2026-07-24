import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import type { MediaItemSummary, MediaKind } from '../api'
import {
  ACTIVE_DOWNLOAD_STATES, bulkEditLibrary, completeness, fmtRating, getLibrary,
  getProfiles, getQueue, getScanReport, getSettings, posterUrl,
  RATING_SOURCE_LABELS, triggerScan,
} from '../api'
import { PAGE_SIZES, Pager, PageSizePicker, sliceForPage } from '../Pager'

// CardBadges: at-a-glance state on a poster — rating, in-flight downloads,
// and the green/yellow/red completeness pill.
function CardBadges(props: { m: MediaItemSummary; downloading: boolean }) {
  const { m } = props
  const comp = completeness(m.kind, m.episodeFileCount, m.episodeCount, m.fileCount)
  return (
    <>
      {m.ratingVotes > 0 && (
        <span
          className="poster-chip chip-left"
          title={`${
            RATING_SOURCE_LABELS[m.ratings[0]?.source] ??
            (m.kind === 'book' ? 'Open Library' : 'TMDB')
          } · ${m.ratingVotes.toLocaleString()} votes`}
        >
          ★ {fmtRating(m.kind, m.rating)}
        </span>
      )}
      {props.downloading && (
        <span className="poster-chip chip-right" title="Download in flight">
          ↓
        </span>
      )}
      <span
        className={`pill ${comp.cls}`}
        title={
          m.kind === 'series'
            ? 'Monitored episodes aired to date that are on disk'
            : 'On disk?'
        }
      >
        {comp.total === 0 ? '—' : `${comp.have}/${comp.total}`}
      </span>
    </>
  )
}

const KIND_TABS: { label: string; kind?: MediaKind }[] = [
  { label: 'All' },
  { label: 'Movies', kind: 'movie' },
  { label: 'TV', kind: 'series' },
  { label: 'Books', kind: 'book' },
]

// The All view groups by kind in this order — never interleaved.
const KIND_SECTIONS: { kind: MediaKind; label: string }[] = [
  { kind: 'movie', label: 'Movies' },
  { kind: 'series', label: 'TV' },
  { kind: 'book', label: 'Books' },
]

type SortKey = 'title' | 'author' | 'year' | 'added' | 'rating'
type FilterKey = 'all' | 'monitored' | 'unmonitored' | 'missing' | 'incomplete' | 'complete'

// Per-kind sort menus: books sort by author too, nobody else needs it.
const SORTS: { key: SortKey; label: string; kinds?: MediaKind[] }[] = [
  { key: 'title', label: 'Title' },
  { key: 'author', label: 'Author', kinds: ['book'] },
  { key: 'year', label: 'Year' },
  { key: 'added', label: 'Recently added' },
  { key: 'rating', label: 'Rating' },
]

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: 'all', label: 'Everything' },
  { key: 'monitored', label: 'Monitored' },
  { key: 'unmonitored', label: 'Unmonitored' },
  { key: 'missing', label: 'Missing (nothing on disk)' },
  { key: 'incomplete', label: 'Incomplete' },
  { key: 'complete', label: 'Complete' },
]

// matchesFilter applies the completeness/monitoring filter to one item.
function matchesFilter(m: MediaItemSummary, f: FilterKey): boolean {
  const comp = completeness(m.kind, m.episodeFileCount, m.episodeCount, m.fileCount)
  switch (f) {
    case 'monitored':
      return m.monitored
    case 'unmonitored':
      return !m.monitored
    case 'missing':
      return comp.have === 0 && comp.total > 0
    case 'incomplete':
      return comp.total > 0 && comp.have < comp.total
    case 'complete':
      return comp.total > 0 && comp.have >= comp.total
    default:
      return true
  }
}

function compare(a: MediaItemSummary, b: MediaItemSummary, key: SortKey): number {
  switch (key) {
    case 'year':
      return a.year - b.year
    case 'added':
      return a.addedAt.localeCompare(b.addedAt)
    case 'rating':
      return a.rating - b.rating
    case 'author':
      return (a.author || a.title).localeCompare(b.author || b.title, undefined, { sensitivity: 'base' })
    default:
      return a.title.localeCompare(b.title, undefined, { sensitivity: 'base' })
  }
}

// Each kind carries its OWN controls — the Movies section sorts by year
// while Books sort by author. The same settings drive that kind's flat
// tab, and everything but the text filter persists per browser.
interface SectionState {
  q: string
  filter: FilterKey
  sortKey: SortKey
  sortDir: 'asc' | 'desc'
  /** Rows per page; 0 means All. Persisted — a preference about how much
   *  you want on screen should not reset every visit. */
  pageSize: number
}

function loadSection(kind: MediaKind): SectionState {
  const state: SectionState = {
    q: '',
    filter: 'all',
    sortKey: 'title',
    sortDir: 'asc',
    pageSize: 100,
  }
  try {
    const raw = localStorage.getItem(`monarr-lib-${kind}`)
    if (raw) {
      const saved = JSON.parse(raw) as Partial<SectionState>
      if (FILTERS.some((f) => f.key === saved.filter)) state.filter = saved.filter as FilterKey
      if (SORTS.some((s) => s.key === saved.sortKey)) state.sortKey = saved.sortKey as SortKey
      if (saved.sortDir === 'asc' || saved.sortDir === 'desc') state.sortDir = saved.sortDir
      // Only honour a size we actually offer, so a hand-edited or stale
      // value cannot leave someone stuck on a page size the UI cannot show.
      if (PAGE_SIZES.includes(saved.pageSize as (typeof PAGE_SIZES)[number])) {
        state.pageSize = saved.pageSize as number
      }
    }
  } catch {
    /* private mode / bad JSON — defaults win */
  }
  return state
}

function persistSection(kind: MediaKind, s: SectionState) {
  try {
    localStorage.setItem(
      `monarr-lib-${kind}`,
      JSON.stringify({
        filter: s.filter,
        sortKey: s.sortKey,
        sortDir: s.sortDir,
        pageSize: s.pageSize,
      }),
    )
  } catch {
    /* private mode */
  }
}

function applySection(items: MediaItemSummary[], s: SectionState): MediaItemSummary[] {
  const needle = s.q.trim().toLowerCase()
  return items
    .filter((m) => matchesFilter(m, s.filter))
    .filter(
      (m) =>
        !needle ||
        m.title.toLowerCase().includes(needle) ||
        (m.author && m.author.toLowerCase().includes(needle)),
    )
    .sort((a, b) => (s.sortDir === 'asc' ? 1 : -1) * compare(a, b, s.sortKey))
}

// SectionToolbar: the compact filter/sort strip one kind owns.
function SectionToolbar(props: {
  kind: MediaKind
  label: string
  state: SectionState
  onChange: (patch: Partial<SectionState>) => void
  shown: number
  total: number
}) {
  const { label, state, onChange } = props
  const filtered = state.filter !== 'all' || state.q.trim() !== ''
  return (
    <div className="section-toolbar">
      <input
        type="search"
        placeholder="Filter…"
        aria-label={`Filter ${label} by title`}
        value={state.q}
        onChange={(e) => onChange({ q: e.target.value })}
      />
      <select
        aria-label={`Show ${label}`}
        title="Filter by state"
        value={state.filter}
        onChange={(e) => onChange({ filter: e.target.value as FilterKey })}
      >
        {FILTERS.map((f) => (
          <option key={f.key} value={f.key}>
            {f.label}
          </option>
        ))}
      </select>
      <select
        aria-label={`Sort ${label}`}
        title="Sort"
        value={state.sortKey}
        onChange={(e) => {
          const key = e.target.value as SortKey
          // A fresh sort key gets its natural direction once.
          onChange({ sortKey: key, sortDir: key === 'added' || key === 'rating' ? 'desc' : 'asc' })
        }}
      >
        {SORTS.filter((s) => !s.kinds || s.kinds.includes(props.kind)).map((s) => (
          <option key={s.key} value={s.key}>
            {s.label}
          </option>
        ))}
      </select>
      <button
        aria-label={`Toggle ${label} sort direction`}
        title={state.sortDir === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending'}
        onClick={() => onChange({ sortDir: state.sortDir === 'asc' ? 'desc' : 'asc' })}
      >
        {state.sortDir === 'asc' ? '↑' : '↓'}
      </button>
      {filtered && (
        <span className="muted">
          {props.shown}/{props.total}
        </span>
      )}
      <PageSizePicker
        size={state.pageSize}
        onChange={(n) => onChange({ pageSize: n })}
        label="Show"
      />
    </div>
  )
}

export function LibraryPage() {
  const [kind, setKind] = useState<MediaKind | undefined>(undefined)
  // Page number per kind. Not persisted: coming back to the library on
  // page 9 of a list that has since changed is disorienting, and the
  // page-size preference is the part worth remembering.
  const [pages, setPages] = useState<Record<MediaKind, number>>({
    movie: 0,
    series: 0,
    book: 0,
  })
  const navigate = useNavigate()
  const qc = useQueryClient()

  const items = useQuery({
    // Always fetch the whole library and split it per tab in the browser.
    // A few hundred summaries is a small payload, tab switching stops
    // hitting the network, and — the actual reason — every tab can show its
    // own count even while another tab is on screen.
    queryKey: ['library', 'all'],
    queryFn: () => getLibrary(),
    refetchInterval: 30_000,
  })
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const report = useQuery({ queryKey: ['scan-report'], queryFn: getScanReport })
  const queue = useQuery({ queryKey: ['queue'], queryFn: getQueue, refetchInterval: 30_000 })
  const downloading = new Set(
    (queue.data ?? [])
      .filter((q) => ACTIVE_DOWNLOAD_STATES.includes(q.state))
      .map((q) => q.mediaItemId),
  )

  // Per-tab counts, so a tab that looks empty is empty rather than
  // ambiguous — "Books 0" answers the question "is this broken?".
  const kindCounts = useMemo(() => {
    if (!items.data) return undefined
    const counts = { movie: 0, series: 0, book: 0, total: items.data.length }
    for (const m of items.data) counts[m.kind]++
    return counts
  }, [items.data])

  const scan = useMutation({
    mutationFn: triggerScan,
    onSuccess: () => {
      // The scan runs async; refresh the report and library shortly after.
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: ['scan-report'] })
        void qc.invalidateQueries({ queryKey: ['library'] })
      }, 1500)
    },
  })

  // Mass editor (Phase 5): select items, apply monitoring/profile at once.
  const [editing, setEditing] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [bulkProfile, setBulkProfile] = useState<number | ''>('')
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles, enabled: editing })
  const bulk = useMutation({
    mutationFn: (patch: { monitored?: boolean; qualityProfileId?: number }) =>
      bulkEditLibrary({ ids: [...selected], ...patch }),
    onSuccess: () => {
      setSelected(new Set())
      setEditing(false)
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
  })
  const toggle = (id: number) => {
    const next = new Set(selected)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setSelected(next)
  }

  const unmatched = report.data?.unmatchedDirs ?? []

  // Every kind owns its controls; the flat tabs reuse the same state.
  const [controls, setControls] = useState<Record<MediaKind, SectionState>>(() => ({
    movie: loadSection('movie'),
    series: loadSection('series'),
    book: loadSection('book'),
  }))
  const updateSection = (k: MediaKind, patch: Partial<SectionState>) => {
    setControls((prev) => {
      const next = { ...prev, [k]: { ...prev[k], ...patch } }
      persistSection(k, next[k])
      return next
    })
    // Any change to what the list contains or how it is ordered invalidates
    // the page number: page 7 of the old ordering is not page 7 of the new
    // one, and a stale page can leave the grid looking empty.
    setPages((prev) => ({ ...prev, [k]: 0 }))
  }

  const renderCard = (m: MediaItemSummary) =>
    editing ? (
      <button
        key={m.id}
        className="poster-card"
        style={{
          textAlign: 'inherit', cursor: 'pointer',
          outline: selected.has(m.id) ? '2px solid var(--accent, #7c5cff)' : 'none',
        }}
        aria-pressed={selected.has(m.id)}
        onClick={() => toggle(m.id)}
      >
        {m.posterPath ? (
          <img src={posterUrl(m.posterPath)} alt="" loading="lazy" />
        ) : (
          <div className="poster-fallback">{m.title.slice(0, 1)}</div>
        )}
        <div className="poster-meta">
          <div className="poster-title" title={m.title}>
            {selected.has(m.id) ? '☑ ' : '☐ '}
            {m.title}
          </div>
          <div className="muted">{m.monitored ? 'monitored' : 'unmonitored'}</div>
        </div>
      </button>
    ) : (
      <Link key={m.id} to="/library/$id" params={{ id: String(m.id) }} className="poster-card">
        {m.posterPath ? (
          <img src={posterUrl(m.posterPath)} alt="" loading="lazy" />
        ) : (
          <div className="poster-fallback">{m.title.slice(0, 1)}</div>
        )}
        <div className="poster-meta">
          <div className="poster-title" title={m.title}>
            {m.title}
          </div>
          <div className="muted poster-sub">
            <span>
              {m.kind === 'book' && m.author ? m.author : `${m.year || '—'} · ${m.kind}`}
            </span>
            <CardBadges m={m} downloading={downloading.has(m.id)} />
          </div>
        </div>
      </Link>
    )

  return (
    <>
      <header className="page-head">
        <h1>Library</h1>
        <div className="tabs" role="tablist">
          {KIND_TABS.map((t) => (
            <button
              key={t.label}
              role="tab"
              aria-selected={kind === t.kind}
              className={kind === t.kind ? 'tab active' : 'tab'}
              onClick={() => setKind(t.kind)}
            >
              {t.label}
              {kindCounts && (
                <span className="tab-count">
                  {t.kind ? kindCounts[t.kind] : kindCounts.total}
                </span>
              )}
            </button>
          ))}
        </div>
        <div className="head-actions">
          <button onClick={() => scan.mutate()} disabled={scan.isPending}>
            Scan disk
          </button>
          <button onClick={() => { setEditing(!editing); setSelected(new Set()) }}>
            {editing ? 'Done' : 'Edit'}
          </button>
          <Link to="/add" className="btn-accent">
            + Add media
          </Link>
        </div>
      </header>

      {editing && (
        <div className="banner">
          <strong>{selected.size}</strong> selected ·{' '}
          <button disabled={selected.size === 0 || bulk.isPending} onClick={() => bulk.mutate({ monitored: true })}>
            Monitor
          </button>{' '}
          <button disabled={selected.size === 0 || bulk.isPending} onClick={() => bulk.mutate({ monitored: false })}>
            Unmonitor
          </button>{' '}
          <select value={bulkProfile} onChange={(e) => setBulkProfile(e.target.value ? Number(e.target.value) : '')}>
            <option value="">(profile…)</option>
            {profiles.data?.map((p) => (
              <option key={p.id} value={p.id}>{p.name}</option>
            ))}
          </select>{' '}
          <button
            disabled={selected.size === 0 || bulkProfile === '' || bulk.isPending}
            onClick={() => bulk.mutate({ qualityProfileId: Number(bulkProfile) })}
          >
            Apply profile
          </button>
        </div>
      )}

      {settings.data && !settings.data.tmdbApiKeyConfigured && (
        <div className="banner warning">
          No TMDB API key configured — searching and adding media won't work yet.{' '}
          <Link to="/settings">Set it in Settings →</Link>
        </div>
      )}

      {unmatched.length > 0 && (
        <div className="banner">
          <strong>{unmatched.length}</strong> unmatched folder{unmatched.length > 1 ? 's' : ''} found
          on disk:
          <ul className="unmatched-list">
            {unmatched.slice(0, 5).map((d) => (
              <li key={d.path}>
                <span className="mono">{d.name}</span>
                <button
                  onClick={() => navigate({ to: '/add', search: { q: d.name, kind: 'series' } })}
                >
                  Match…
                </button>
              </li>
            ))}
            {unmatched.length > 5 && <li className="muted">…and {unmatched.length - 5} more</li>}
          </ul>
        </div>
      )}

      {items.data && items.data.length === 0 && (
        <div className="empty-state">
          <p>The library is empty.</p>
          <p className="muted">
            Add a root folder and your TMDB key under <Link to="/settings">Settings</Link>, then{' '}
            <Link to="/add">add your first movie or series</Link>.
          </p>
        </div>
      )}

      {kind === undefined ? (
        // The All view: everything, grouped by kind — never interleaved —
        // and every section runs its OWN filter/sort strip.
        KIND_SECTIONS.map((section) => {
          const all = (items.data ?? []).filter((m) => m.kind === section.kind)
          if (all.length === 0) return null
          const group = applySection(all, controls[section.kind])
          const size = controls[section.kind].pageSize
          const page = pages[section.kind]
          const visible = sliceForPage(group, page, size)
          return (
            <section key={section.kind} className="lib-section">
              <div className="lib-section-bar">
                <h2 className="lib-section-head">
                  {section.label} <span className="muted">({all.length})</span>
                </h2>
                <SectionToolbar
                  kind={section.kind}
                  label={section.label}
                  state={controls[section.kind]}
                  onChange={(patch) => updateSection(section.kind, patch)}
                  shown={group.length}
                  total={all.length}
                />
              </div>
              {visible.length > 0 ? (
                <div className="poster-grid">{visible.map(renderCard)}</div>
              ) : (
                <p className="muted">Nothing in {section.label} matches the current filter.</p>
              )}
              {group.length > 0 && (
                <div className="lib-pager-bar">
                  <Pager
                    page={page}
                    size={size}
                    total={group.length}
                    onPage={(n) => setPages((prev) => ({ ...prev, [section.kind]: n }))}
                  />
                </div>
              )}
            </section>
          )
        })
      ) : (
        // A flat kind tab: the same section state, one grid.
        (() => {
          const section = KIND_SECTIONS.find((s) => s.kind === kind)!
          const all = (items.data ?? []).filter((m) => m.kind === kind)
          const group = applySection(all, controls[kind])
          const size = controls[kind].pageSize
          const page = pages[kind]
          const visible = sliceForPage(group, page, size)
          return (
            <>
              {all.length > 0 && (
                <div className="lib-section-bar">
                  <SectionToolbar
                    kind={kind}
                    label={section.label}
                    state={controls[kind]}
                    onChange={(patch) => updateSection(kind, patch)}
                    shown={group.length}
                    total={all.length}
                  />
                </div>
              )}
              <div className="poster-grid">{visible.map(renderCard)}</div>
              {all.length > 0 && group.length === 0 && (
                <p className="muted">Nothing in {section.label} matches the current filter.</p>
              )}
              {group.length > 0 && (
                <div className="lib-pager-bar">
                  <Pager
                    page={page}
                    size={size}
                    total={group.length}
                    onPage={(n) => setPages((prev) => ({ ...prev, [kind]: n }))}
                  />
                </div>
              )}
            </>
          )
        })()
      )}
    </>
  )
}
