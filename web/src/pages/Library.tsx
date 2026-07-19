import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import type { MediaItemSummary, MediaKind } from '../api'
import {
  ACTIVE_DOWNLOAD_STATES, bulkEditLibrary, completeness, fmtRating, getLibrary,
  getProfiles, getQueue, getScanReport, getSettings, posterUrl,
  RATING_SOURCE_LABELS, triggerScan,
} from '../api'

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
  { label: 'Series', kind: 'series' },
  { label: 'Books', kind: 'book' },
]

// The All view groups by kind in this order — never interleaved.
const KIND_SECTIONS: { kind: MediaKind; label: string }[] = [
  { kind: 'movie', label: 'Movies' },
  { kind: 'series', label: 'Series' },
  { kind: 'book', label: 'Books' },
]

type SortKey = 'title' | 'year' | 'added' | 'rating'
type FilterKey = 'all' | 'monitored' | 'unmonitored' | 'missing' | 'incomplete' | 'complete'

const SORTS: { key: SortKey; label: string }[] = [
  { key: 'title', label: 'Title' },
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
    default:
      return a.title.localeCompare(b.title, undefined, { sensitivity: 'base' })
  }
}

// Sort/filter choices persist per browser, like the theme.
function savedPref<T extends string>(key: string, valid: readonly T[], fallback: T): T {
  try {
    const v = localStorage.getItem(key)
    if (v && (valid as readonly string[]).includes(v)) return v as T
  } catch {
    /* private mode */
  }
  return fallback
}

function savePref(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    /* private mode */
  }
}

export function LibraryPage() {
  const [kind, setKind] = useState<MediaKind | undefined>(undefined)
  const navigate = useNavigate()
  const qc = useQueryClient()

  const items = useQuery({
    queryKey: ['library', kind ?? 'all'],
    queryFn: () => getLibrary(kind),
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

  // Sort / filter toolbar state, remembered per browser.
  const [sortKey, setSortKey] = useState<SortKey>(() =>
    savedPref('monarr-lib-sort', ['title', 'year', 'added', 'rating'] as const, 'title'))
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>(() =>
    savedPref('monarr-lib-dir', ['asc', 'desc'] as const, 'asc'))
  const [filter, setFilter] = useState<FilterKey>(() =>
    savedPref('monarr-lib-filter', ['all', 'monitored', 'unmonitored', 'missing', 'incomplete', 'complete'] as const, 'all'))
  const [q, setQ] = useState('')

  const needle = q.trim().toLowerCase()
  const visible = (items.data ?? [])
    .filter((m) => matchesFilter(m, filter))
    .filter(
      (m) =>
        !needle ||
        m.title.toLowerCase().includes(needle) ||
        (m.author && m.author.toLowerCase().includes(needle)),
    )
    .sort((a, b) => (sortDir === 'asc' ? 1 : -1) * compare(a, b, sortKey))

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

      {(items.data?.length ?? 0) > 0 && (
        <div className="lib-toolbar">
          <input
            type="search"
            placeholder="Filter by title or author…"
            aria-label="Filter library by title"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <label className="inline">
            Show:{' '}
            <select
              aria-label="Filter"
              value={filter}
              onChange={(e) => {
                setFilter(e.target.value as FilterKey)
                savePref('monarr-lib-filter', e.target.value)
              }}
            >
              {FILTERS.map((f) => (
                <option key={f.key} value={f.key}>
                  {f.label}
                </option>
              ))}
            </select>
          </label>
          <label className="inline">
            Sort:{' '}
            <select
              aria-label="Sort"
              value={sortKey}
              onChange={(e) => {
                const key = e.target.value as SortKey
                setSortKey(key)
                savePref('monarr-lib-sort', key)
                // New sort key gets its natural direction once.
                const dir = key === 'added' || key === 'rating' ? 'desc' : 'asc'
                setSortDir(dir)
                savePref('monarr-lib-dir', dir)
              }}
            >
              {SORTS.map((s) => (
                <option key={s.key} value={s.key}>
                  {s.label}
                </option>
              ))}
            </select>
          </label>
          <button
            title={sortDir === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending'}
            aria-label="Toggle sort direction"
            onClick={() => {
              const dir = sortDir === 'asc' ? 'desc' : 'asc'
              setSortDir(dir)
              savePref('monarr-lib-dir', dir)
            }}
          >
            {sortDir === 'asc' ? '↑' : '↓'}
          </button>
          {(filter !== 'all' || needle) && (
            <span className="muted">
              {visible.length} of {items.data!.length} shown
            </span>
          )}
        </div>
      )}

      {kind === undefined ? (
        // The All view: everything, grouped by kind — never interleaved.
        KIND_SECTIONS.map((section) => {
          const group = visible.filter((m) => m.kind === section.kind)
          if (group.length === 0) return null
          return (
            <section key={section.kind} className="lib-section">
              <h2 className="lib-section-head">
                {section.label} <span className="muted">({group.length})</span>
              </h2>
              <div className="poster-grid">{group.map(renderCard)}</div>
            </section>
          )
        })
      ) : (
        <div className="poster-grid">{visible.map(renderCard)}</div>
      )}

      {(items.data?.length ?? 0) > 0 && visible.length === 0 && (
        <p className="muted">Nothing matches the current filter.</p>
      )}
    </>
  )
}
