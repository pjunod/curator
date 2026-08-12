import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { BookType, MediaItemSummary, MediaKind } from '../api'
import {
  ACTIVE_DOWNLOAD_STATES, bulkEditLibrary, completeness, getLibrary,
  getProfiles, getQueue, getReviewQueue, getScanReport, getSettings, posterUrl,
  triggerScan,
} from '../api'
import { PAGE_SIZES, Pager, PageSizePicker, sliceForPage } from '../Pager'
import { CardSizePicker } from '../CardSizePicker'

// CardBadges: the poster stays a poster. Nothing is drawn over the artwork —
// quality, target, upgrade state and ratings all live on the item page, which
// is where someone goes when they want detail. What is left here is the
// completeness pill and an in-flight marker, both in the card's text footer
// rather than on the image.
function CardBadges(props: { m: MediaItemSummary; downloading: boolean }) {
  const { m } = props
  const comp = completeness(m.kind, m.episodeFileCount, m.episodeCount, m.fileCount)
  return (
    <>
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
      {props.downloading && (
        <span className="pill pill-info" title="Download in flight">
          ↓
        </span>
      )}
    </>
  )
}

type LibraryTab = 'all' | 'movie' | 'series' | BookType

const KIND_TABS: { label: string; key: LibraryTab }[] = [
  { label: 'All', key: 'all' },
  { label: 'Movies', key: 'movie' },
  { label: 'TV', key: 'series' },
  { label: 'Ebooks', key: 'ebook' },
  { label: 'Audiobooks', key: 'audiobook' },
]

// The All view groups by kind in this order — never interleaved.
const KIND_SECTIONS: { key: Exclude<LibraryTab, 'all'>; kind: MediaKind; bookType?: BookType; label: string }[] = [
  { key: 'movie', kind: 'movie', label: 'Movies' },
  { key: 'series', kind: 'series', label: 'TV' },
  { key: 'ebook', kind: 'book', bookType: 'ebook', label: 'Ebooks' },
  { key: 'audiobook', kind: 'book', bookType: 'audiobook', label: 'Audiobooks' },
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

type SectionKey = Exclude<LibraryTab, 'all'>

function loadSection(key: SectionKey): SectionState {
  const state: SectionState = {
    q: '',
    filter: 'all',
    sortKey: 'title',
    sortDir: 'asc',
    pageSize: 100,
  }
  try {
    // The old Books section becomes Ebooks on upgrade, so retain its saved
    // controls while Audiobooks receives an independent clean slate.
    const raw = localStorage.getItem(`monarr-lib-${key}`) ??
      (key === 'ebook' ? localStorage.getItem('monarr-lib-book') : null)
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

function persistSection(key: SectionKey, s: SectionState) {
  try {
    localStorage.setItem(
      `monarr-lib-${key}`,
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

// HiddenNotice says out loud when a section is not showing everything it has,
// and offers the one click that fixes it.
//
// The filter and page size PERSIST across visits, so a filter set weeks ago
// silently hides a title added today — and the only clue was a small "12/300"
// with no explanation. "I added it, I can search for it, and it is not in the
// Movies section" is what that looks like from the outside.
function HiddenNotice(props: {
  label: string
  shown: number
  total: number
  filterActive: boolean
  onClear: () => void
}) {
  const hidden = props.total - props.shown
  if (hidden <= 0 || !props.filterActive) return null
  return (
    <p className="hidden-notice">
      {hidden} of {props.total} {props.label.toLowerCase()} hidden by the current filter.{' '}
      <button className="link-button" onClick={props.onClear}>
        Show all
      </button>
    </p>
  )
}

export function LibraryPage() {
  const [tab, setTab] = useState<LibraryTab>('all')
  // Page number per kind. Not persisted: coming back to the library on
  // page 9 of a list that has since changed is disorienting, and the
  // page-size preference is the part worth remembering.
  const [pages, setPages] = useState<Record<SectionKey, number>>({
    movie: 0,
    series: 0,
    ebook: 0,
    audiobook: 0,
  })
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
    const counts = { movie: 0, series: 0, ebook: 0, audiobook: 0, total: items.data.length }
    for (const m of items.data) {
      if (m.kind === 'book') counts[m.bookType ?? 'ebook']++
      else counts[m.kind]++
    }
    return counts
  }, [items.data])

  const scan = useMutation({
    mutationFn: triggerScan,
    onSuccess: () => {
      // The scan runs async; refresh the report and library shortly after.
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: ['scan-report'] })
        void qc.invalidateQueries({ queryKey: ['review'] })
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

  // unmatchedDirs is a capped prefix, so it is only ever a sample of names.
  // The count comes from the review queue — the same source the review window
  // pages through — because a number here that disagrees with the window it
  // sends people to is worse than no number at all.
  const unmatched = report.data?.unmatchedDirs ?? []
  const reviewCount = useQuery({
    queryKey: ['review', 'count'],
    queryFn: () => getReviewQueue(undefined, '', 1, 0),
    select: (page) => page.counts.total,
  })
  const unmatchedTotal = reviewCount.data ?? 0

  // Every first-class library section owns its controls; the flat tabs reuse
  // that state. Ebooks and Audiobooks therefore sort/filter independently.
  const [controls, setControls] = useState<Record<SectionKey, SectionState>>(() => ({
    movie: loadSection('movie'),
    series: loadSection('series'),
    ebook: loadSection('ebook'),
    audiobook: loadSection('audiobook'),
  }))
  const updateSection = (key: SectionKey, patch: Partial<SectionState>) => {
    setControls((prev) => {
      const next = { ...prev, [key]: { ...prev[key], ...patch } }
      persistSection(key, next[key])
      return next
    })
    // Any change to what the list contains or how it is ordered invalidates
    // the page number: page 7 of the old ordering is not page 7 of the new
    // one, and a stale page can leave the grid looking empty.
    setPages((prev) => ({ ...prev, [key]: 0 }))
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
              {m.kind === 'book'
                ? [m.author, m.bookType === 'audiobook' ? 'Audiobook' : 'Ebook'].filter(Boolean).join(' · ')
                : `${m.year || '—'} · ${m.kind}`}
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
              aria-selected={tab === t.key}
              className={tab === t.key ? 'tab active' : 'tab'}
              onClick={() => setTab(t.key)}
            >
              {t.label}
              {kindCounts && (
                <span className="tab-count">
                  {t.key === 'all' ? kindCounts.total : kindCounts[t.key]}
                </span>
              )}
            </button>
          ))}
        </div>
        <CardSizePicker />
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

      {unmatchedTotal > 0 && (
        <div className="banner">
          <strong>{unmatchedTotal}</strong> unmatched folder{unmatchedTotal > 1 ? 's' : ''} found on
          disk:
          <ul className="unmatched-list">
            {unmatched.slice(0, 5).map((d) => (
              <li key={d.path}>
                <span className="mono">{d.name}</span>
              </li>
            ))}
          </ul>
          {/* One route into the paged review window rather than five
              hardcoded-kind buttons: the window knows each folder's kind and
              carries adoption's proposals, which a Match… button here never
              could. */}
          <Link to="/settings" hash="library-folders">
            Review all {unmatchedTotal} in Settings →
          </Link>
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

      {tab === 'all' ? (
        // The All view: everything, grouped by kind — never interleaved —
        // and every section runs its OWN filter/sort strip.
        KIND_SECTIONS.map((section) => {
          const all = (items.data ?? []).filter((m) =>
            m.kind === section.kind && (!section.bookType || (m.bookType ?? 'ebook') === section.bookType),
          )
          if (all.length === 0) return null
          const group = applySection(all, controls[section.key])
          const size = controls[section.key].pageSize
          const page = pages[section.key]
          const visible = sliceForPage(group, page, size)
          return (
            <section key={section.key} className="lib-section">
              <div className="lib-section-bar">
                <h2 className="lib-section-head">
                  {section.label} <span className="muted">({all.length})</span>
                </h2>
                <SectionToolbar
                  kind={section.kind}
                  label={section.label}
                  state={controls[section.key]}
                  onChange={(patch) => updateSection(section.key, patch)}
                  shown={group.length}
                  total={all.length}
                />
              </div>
              <HiddenNotice
                label={section.label}
                shown={group.length}
                total={all.length}
                filterActive={
                  controls[section.key].filter !== 'all' || controls[section.key].q.trim() !== ''
                }
                onClear={() => updateSection(section.key, { filter: 'all', q: '' })}
              />
              {visible.length > 0 ? (
                <div className="poster-grid">{visible.map(renderCard)}</div>
              ) : (
                <p className="muted">
                  Nothing in {section.label} matches the current filter.{' '}
                  <button
                    className="link-button"
                    onClick={() => updateSection(section.key, { filter: 'all', q: '' })}
                  >
                    Show all {all.length}
                  </button>
                </p>
              )}
              {group.length > 0 && (
                <div className="lib-pager-bar">
                  <Pager
                    page={page}
                    size={size}
                    total={group.length}
                    onPage={(n) => setPages((prev) => ({ ...prev, [section.key]: n }))}
                  />
                </div>
              )}
            </section>
          )
        })
      ) : (
        // A flat kind tab: the same section state, one grid.
        (() => {
          const section = KIND_SECTIONS.find((s) => s.key === tab)!
          const all = (items.data ?? []).filter((m) =>
            m.kind === section.kind && (!section.bookType || (m.bookType ?? 'ebook') === section.bookType),
          )
          const group = applySection(all, controls[section.key])
          const size = controls[section.key].pageSize
          const page = pages[section.key]
          const visible = sliceForPage(group, page, size)
          return (
            <>
              {all.length > 0 && (
                <div className="lib-section-bar">
                  <SectionToolbar
                    kind={section.kind}
                    label={section.label}
                    state={controls[section.key]}
                    onChange={(patch) => updateSection(section.key, patch)}
                    shown={group.length}
                    total={all.length}
                  />
                </div>
              )}
              <HiddenNotice
                label={section.label}
                shown={group.length}
                total={all.length}
                filterActive={controls[section.key].filter !== 'all' || controls[section.key].q.trim() !== ''}
                onClear={() => updateSection(section.key, { filter: 'all', q: '' })}
              />
              <div className="poster-grid">{visible.map(renderCard)}</div>
              {all.length > 0 && group.length === 0 && (
                <p className="muted">
                  Nothing in {section.label} matches the current filter.{' '}
                  <button
                    className="link-button"
                    onClick={() => updateSection(section.key, { filter: 'all', q: '' })}
                  >
                    Show all {all.length}
                  </button>
                </p>
              )}
              {group.length > 0 && (
                <div className="lib-pager-bar">
                  <Pager
                    page={page}
                    size={size}
                    total={group.length}
                    onPage={(n) => setPages((prev) => ({ ...prev, [section.key]: n }))}
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
