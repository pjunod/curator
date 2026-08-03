import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { DiscoverList, SearchResult } from '../api'
import {
  addLibraryItem,
  getDiscoverItems,
  getDiscoverLists,
  getProfiles,
  getRootFolders,
  getSettings,
  posterUrl,
} from '../api'
import { CardSizePicker } from '../CardSizePicker'
import type { DiscoverFilter } from '../discover'
import { DISCOVER_FILTERS, cardKey, rowCounts, sourceLabel, visibleLists } from '../discover'
import { useInView } from '../useInView'

// AddedItem is something added during this visit, so the card that was
// clicked can say so without refetching the row from upstream.
interface AddedItem {
  id: number
  title: string
}

export function DiscoverPage() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState<DiscoverFilter>('all')
  const [open, setOpen] = useState<SearchResult | null>(null)
  const [added, setAdded] = useState<Record<string, AddedItem>>({})

  const lists = useQuery({ queryKey: ['discover-lists'], queryFn: getDiscoverLists })
  const rows = visibleLists(lists.data ?? [], filter)
  const counts = rowCounts(lists.data ?? [])

  return (
    <>
      <header className="page-head">
        <h1>Discover</h1>
        <div className="tabs" role="tablist">
          {DISCOVER_FILTERS.map((f) => (
            <button
              key={f.key}
              role="tab"
              aria-selected={filter === f.key}
              className={filter === f.key ? 'tab active' : 'tab'}
              onClick={() => setFilter(f.key)}
            >
              {f.label} <span className="tab-count">{counts[f.key]}</span>
            </button>
          ))}
        </div>
        <CardSizePicker />
      </header>

      {lists.isError && (
        <div className="banner warning">{String((lists.error as Error).message)}</div>
      )}

      {/* An empty catalogue is not an error — it is an install with no
          provider configured, which is a setup step and not a failure. */}
      {lists.isSuccess && lists.data.length === 0 && (
        <div className="empty-state">
          <p>Nothing to browse yet.</p>
          <p className="muted">
            Discover reads from your metadata providers. Add a TMDB API key under{' '}
            <Link to="/settings">Settings</Link> for nine rows of trending, popular and
            upcoming titles — and an optional free Trakt client id for five more.
          </p>
        </div>
      )}

      {rows.map((list) => (
        <DiscoverRow
          key={list.id}
          list={list}
          // Four row titles exist twice, once per kind — "Trending this
          // week", "Popular", "Top rated", "Most anticipated". Side by side
          // under All they are indistinguishable, so the kind goes in the
          // heading there. On a filtered tab every row is that kind already
          // and repeating it is noise.
          showKind={filter === 'all'}
          added={added}
          onPick={(r) => setOpen(r)}
        />
      ))}

      {open && (
        <AddDialog
          result={open}
          added={added[cardKey(open)]}
          onClose={() => setOpen(null)}
          onAdded={(item) => {
            setAdded((prev) => ({ ...prev, [cardKey(open)]: item }))
            void qc.invalidateQueries({ queryKey: ['library'] })
            void qc.invalidateQueries({ queryKey: ['wanted'] })
          }}
        />
      )}
    </>
  )
}

// ---- one row ----

function DiscoverRow(props: {
  list: DiscoverList
  showKind: boolean
  added: Record<string, AddedItem>
  onPick: (r: SearchResult) => void
}) {
  const { list } = props
  // The row does not fetch until it is nearly on screen. Fourteen rows fetched
  // eagerly is fourteen upstream calls to draw a page most of which is below
  // the fold.
  const { ref, seen } = useInView<HTMLElement>()
  const items = useQuery({
    queryKey: ['discover-items', list.id],
    queryFn: () => getDiscoverItems(list.id),
    enabled: seen,
    retry: false,
    // The server already caches these for half an hour (ADR 0015), so a
    // remount inside a session should not go back over the wire at all.
    staleTime: 5 * 60_000,
  })

  return (
    <section className="lib-section discover-section" ref={ref} data-list={list.id}>
      <div className="lib-section-bar">
        <div>
          <h2 className="lib-section-head">
            {list.title}
            {props.showKind && (
              <span className="discover-kind">
                {' · '}
                {list.kind === 'series' ? 'Shows' : 'Movies'}
              </span>
            )}{' '}
            <span className="discover-source">{sourceLabel(list.source)}</span>
          </h2>
          <p className="muted discover-blurb">{list.blurb}</p>
        </div>
      </div>

      {items.isError ? (
        <p className="muted discover-note">
          Could not load this row: {String((items.error as Error).message)}
        </p>
      ) : (
        <Strip
          loading={!seen || items.isPending}
          items={items.data ?? []}
          added={props.added}
          onPick={props.onPick}
        />
      )}
    </section>
  )
}

// Strip is the horizontally scrolling track. Native scroll does the work —
// the buttons exist because a mouse has no horizontal gesture, and they are
// hidden from assistive tech since the same content is reachable by scrolling.
function Strip(props: {
  loading: boolean
  items: SearchResult[]
  added: Record<string, AddedItem>
  onPick: (r: SearchResult) => void
}) {
  const track = useRef<HTMLDivElement | null>(null)
  const [edge, setEdge] = useState<{ start: boolean; end: boolean }>({ start: true, end: true })

  const measure = () => {
    const el = track.current
    if (!el) return
    const max = el.scrollWidth - el.clientWidth
    setEdge({ start: el.scrollLeft <= 1, end: el.scrollLeft >= max - 1 })
  }
  useEffect(measure, [props.items, props.loading])

  const nudge = (dir: -1 | 1) => {
    const el = track.current
    if (!el) return
    el.scrollBy({ left: dir * el.clientWidth * 0.8, behavior: 'smooth' })
  }

  if (props.loading) {
    return (
      <div className="discover-strip">
        <div className="discover-track">
          {Array.from({ length: 6 }, (_, i) => (
            <div key={i} className="discover-card discover-skeleton" aria-hidden="true" />
          ))}
        </div>
      </div>
    )
  }

  if (props.items.length === 0) {
    return <p className="muted discover-note">Nothing in this row right now.</p>
  }

  return (
    <div className="discover-strip">
      <button
        className="discover-arrow start"
        aria-hidden="true"
        tabIndex={-1}
        disabled={edge.start}
        onClick={() => nudge(-1)}
      >
        ‹
      </button>
      <div className="discover-track" ref={track} onScroll={measure}>
        {props.items.map((r) => (
          <Card key={cardKey(r)} result={r} added={props.added[cardKey(r)]} onPick={props.onPick} />
        ))}
      </div>
      <button
        className="discover-arrow end"
        aria-hidden="true"
        tabIndex={-1}
        disabled={edge.end}
        onClick={() => nudge(1)}
      >
        ›
      </button>
    </div>
  )
}

// Card is the Library page's poster card as a button. Nothing is drawn over
// the artwork — the state chip lives in the text footer, same rule as there.
function Card(props: { result: SearchResult; added?: AddedItem; onPick: (r: SearchResult) => void }) {
  const { result: r } = props
  const inLibrary = r.inLibrary || props.added !== undefined
  return (
    <button
      className="poster-card discover-card"
      onClick={() => props.onPick(r)}
      title={r.title}
    >
      {r.posterPath ? (
        <img src={posterUrl(r.posterPath)} alt="" loading="lazy" />
      ) : (
        <div className="poster-fallback">{r.title.slice(0, 1)}</div>
      )}
      <div className="poster-meta">
        <div className="poster-title">{r.title}</div>
        <div className="muted poster-sub">
          <span>{r.year || '—'}</span>
          {inLibrary && <span className="pill pill-ok">in library</span>}
        </div>
      </div>
    </button>
  )
}

// ---- the add dialog ----

function AddDialog(props: {
  result: SearchResult
  added?: AddedItem
  onClose: () => void
  onAdded: (item: AddedItem) => void
}) {
  const r = props.result
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })

  const [rootId, setRootId] = useState<number | undefined>(undefined)
  const [profileId, setProfileId] = useState<number | ''>('')
  const [monitored, setMonitored] = useState(true)
  const [monitor, setMonitor] = useState<'all' | 'latest' | 'none'>('all')
  const [searchNow, setSearchNow] = useState(true)

  // Same rule as the Add page: a root folder declares which kind it holds
  // (ADR 0009), so offering the others turns a settled question into a
  // rejected POST.
  const eligibleRoots = (roots.data ?? []).filter((rf) => rf.kind === 'mixed' || rf.kind === r.kind)
  const eligibleIds = eligibleRoots.map((rf) => rf.id).join(',')
  const selectedRootId = eligibleRoots.some((rf) => rf.id === rootId)
    ? rootId
    : eligibleRoots[0]?.id
  useEffect(() => {
    if (eligibleRoots.length === 0) {
      setRootId(undefined)
      return
    }
    if (rootId === undefined || !eligibleRoots.some((rf) => rf.id === rootId)) {
      setRootId(eligibleRoots[0].id)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [eligibleIds, rootId])

  const { onClose } = props
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const defaultProfileId = settings.data?.defaultProfiles?.[r.kind]
  const defaultProfileName = profiles.data?.find((p) => p.id === defaultProfileId)?.name

  const add = useMutation({
    mutationFn: () =>
      addLibraryItem({
        kind: r.kind,
        tmdbId: r.tmdbId || undefined,
        tvdbId: r.tvdbId,
        rootFolderId: selectedRootId,
        qualityProfileId: profileId === '' ? undefined : Number(profileId),
        monitored,
        monitor: r.kind === 'series' ? monitor : undefined,
        searchNow: monitored && searchNow,
      }),
    onSuccess: (item) => props.onAdded({ id: item.id, title: item.title || r.title }),
  })

  return (
    // The backdrop is the flex container and the modal is its child — the
    // house pattern (ReviewWindow). As siblings the backdrop's z-index puts
    // it over the dialog and swallows every click.
    <div className="modal-backdrop" onMouseDown={props.onClose}>
      <div
        className="modal discover-modal"
        role="dialog"
        aria-modal="true"
        aria-label={`Add ${r.title}`}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="modal-head">
          <h2>
            {r.title} <span className="muted">({r.year || '—'})</span>
          </h2>
          <button className="link-button" onClick={props.onClose}>
            Close
          </button>
        </div>

        <div className="modal-body discover-modal-body">
          {r.posterPath ? (
            <img className="discover-modal-poster" src={posterUrl(r.posterPath, 'w342')} alt="" />
          ) : (
            <div className="poster-fallback discover-modal-poster">{r.title.slice(0, 1)}</div>
          )}
          <div className="discover-modal-detail">
            <p className="muted">
              {r.kind === 'series' ? 'Series' : 'Movie'}
              {r.source ? ` · from ${sourceLabel(r.source)}` : ''}
            </p>
            <p>{r.overview || <span className="muted">No description available.</span>}</p>

            {props.added ? (
              <p>
                <span className="pill pill-ok">added</span>{' '}
                <Link to="/library/$id" params={{ id: String(props.added.id) }}>
                  Open in library
                </Link>
              </p>
            ) : r.inLibrary ? (
              <p>
                <span className="pill pill-ok">in library</span>{' '}
                <span className="muted">already added — nothing to do here.</span>
              </p>
            ) : (
              <div className="add-controls discover-add">
                <label className="inline">
                  Root folder{' '}
                  <select
                    value={selectedRootId ?? ''}
                    onChange={(e) => setRootId(e.target.value ? Number(e.target.value) : undefined)}
                  >
                    {eligibleRoots.length === 0 && <option value="">No matching root folder</option>}
                    {eligibleRoots.map((rf) => (
                      <option key={rf.id} value={rf.id}>
                        {rf.path}
                        {rf.kind === 'mixed' ? ' (mixed)' : ''}
                      </option>
                    ))}
                  </select>
                </label>
                {roots.data && roots.data.length > 0 && eligibleRoots.length === 0 && (
                  <span className="muted">
                    no root folder holds {r.kind === 'series' ? 'TV' : 'movies'} — add one in
                    Settings before adding or downloading this title
                  </span>
                )}
                <label className="inline">
                  Profile{' '}
                  <select
                    aria-label="Quality profile"
                    value={profileId}
                    onChange={(e) => setProfileId(e.target.value ? Number(e.target.value) : '')}
                  >
                    <option value="">
                      {defaultProfileName ? `Default — ${defaultProfileName}` : 'Default'}
                    </option>
                    {profiles.data?.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.name}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="inline">
                  <input
                    type="checkbox"
                    checked={monitored}
                    onChange={(e) => setMonitored(e.target.checked)}
                  />{' '}
                  Monitored
                </label>
                {r.kind === 'series' && (
                  <label className="inline">
                    Seasons:{' '}
                    <select
                      value={monitor}
                      onChange={(e) => setMonitor(e.target.value as typeof monitor)}
                    >
                      <option value="all">all</option>
                      <option value="latest">latest only</option>
                      <option value="none">none</option>
                    </select>
                  </label>
                )}
                <label className="inline">
                  <input
                    type="checkbox"
                    checked={searchNow}
                    onChange={(e) => setSearchNow(e.target.checked)}
                  />{' '}
                  Search on add
                </label>
              </div>
            )}

            {add.isError && (
              <div className="banner warning">{String((add.error as Error).message)}</div>
            )}
          </div>
        </div>

        <div className="modal-foot">
          {!props.added && !r.inLibrary && (
            <button
              className="btn-accent"
              disabled={add.isPending || !selectedRootId}
              title={!selectedRootId ? 'Add a matching root folder in Settings first' : undefined}
              onClick={() => add.mutate()}
            >
              {add.isPending ? 'Adding…' : 'Add to library'}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
