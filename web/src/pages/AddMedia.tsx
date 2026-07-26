import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import type { MediaKind, SearchResult } from '../api'
import {
  addLibraryItem,
  getProfiles,
  getRootFolders,
  getSettings,
  posterUrl,
  searchMetadata,
} from '../api'

// resultKey identifies a search result across renders and refetches. The list
// has no server-side id before the item exists, and the same title can come
// back from more than one provider, so the tuple is the identity.
const resultKey = (r: SearchResult) => `${r.kind}-${r.olid ?? ''}-${r.tmdbId}-${r.tvdbId ?? 0}`

// AddedItem is one thing added during this visit to the page.
interface AddedItem {
  key: string
  id: number
  title: string
}

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), ms)
    return () => clearTimeout(id)
  }, [value, ms])
  return debounced
}

export function AddMediaPage() {
  const search = useSearch({ from: '/add' })
  const navigate = useNavigate()
  const qc = useQueryClient()

  const [kind, setKind] = useState<MediaKind>(
    search.kind === 'series' || search.kind === 'book' ? search.kind : 'movie',
  )
  const [query, setQuery] = useState(search.q ?? '')
  const debouncedQuery = useDebounced(query, 400)

  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [rootId, setRootId] = useState<number | undefined>(undefined)
  const [profileId, setProfileId] = useState<number | ''>('')

  // What was added without leaving the page, newest first, and which single
  // row is mid-flight. Adding used to navigate to the new item, so adding ten
  // things meant ten trips back here — re-typing nothing but clicking through
  // a detail page nobody asked to see.
  const [added, setAdded] = useState<AddedItem[]>([])
  const [pendingKey, setPendingKey] = useState<string | null>(null)
  const addedByKey = new Map(added.map((a) => [a.key, a]))

  // The default the server will apply, named rather than implied. "(default)"
  // on its own is a promise with no content.
  const defaultProfileId = settings.data?.defaultProfiles?.[kind]
  const defaultProfileName = profiles.data?.find((p) => p.id === defaultProfileId)?.name
  const [monitored, setMonitored] = useState(true)
  const [monitor, setMonitor] = useState<'all' | 'latest' | 'none'>('all')
  const [searchNow, setSearchNow] = useState(true)

  // Root folders declare which kind they hold (ADR 0009), so only some of
  // them can take what is being added. Offering the rest turns a settled
  // question into a rejected POST: the add fails with "root folder holds a
  // different media kind" *after* the user picked from a list that put the
  // wrong root in front of them, preselected.
  const eligibleRoots = (roots.data ?? []).filter((rf) => rf.kind === 'mixed' || rf.kind === kind)
  const eligibleIds = eligibleRoots.map((rf) => rf.id).join(',')

  useEffect(() => {
    if (eligibleRoots.length === 0) {
      setRootId(undefined)
      return
    }
    // Re-pick on a kind change as well: yesterday's movie root is not a valid
    // choice on the Series tab, and a stale selection is invisible until the
    // add is refused.
    if (rootId === undefined || !eligibleRoots.some((rf) => rf.id === rootId)) {
      setRootId(eligibleRoots[0].id)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [eligibleIds, rootId])

  const results = useQuery({
    queryKey: ['metadata-search', kind, debouncedQuery],
    queryFn: () => searchMetadata(kind, debouncedQuery),
    enabled: debouncedQuery.trim().length > 1,
    retry: false,
  })

  const add = useMutation({
    mutationFn: (r: SearchResult) => {
      const common = {
        rootFolderId: rootId,
        qualityProfileId: profileId === '' ? undefined : Number(profileId),
        monitored,
        monitor: r.kind === 'series' ? monitor : undefined,
        searchNow: monitored && searchNow,
      }
      return addLibraryItem(
        r.kind === 'book'
          ? { kind: r.kind, olid: r.olid, ...common }
          // A series from the provider chain is identified by its TVDB id and
          // has no TMDB one (ADR 0011); sending tmdbId: 0 would ask TMDB for
          // series zero.
          : { kind: r.kind, tmdbId: r.tmdbId || undefined, tvdbId: r.tvdbId, ...common },
      )
    },
    onMutate: (r: SearchResult) => setPendingKey(resultKey(r)),
    onSettled: () => setPendingKey(null),
    onSuccess: (item, r) => {
      // Without this the Library page keeps serving its cached list and the
      // new item is simply absent from its section — while the dashboard's
      // "recently added" and global search, which are different queries, both
      // show it. That combination reads as "the item was added somewhere I
      // cannot find".
      void qc.invalidateQueries({ queryKey: ['library'] })
      void qc.invalidateQueries({ queryKey: ['wanted'] })
      // Stay put. The search, the tab, and every option above are still set,
      // so the next add is one click rather than a round trip.
      setAdded((prev) => [{ key: resultKey(r), id: item.id, title: item.title || r.title }, ...prev])
    },
  })

  return (
    <>
      <header className="page-head">
        <h1>Add media</h1>
        <div className="tabs" role="tablist">
          {(['movie', 'series', 'book'] as MediaKind[]).map((k) => (
            <button
              key={k}
              role="tab"
              aria-selected={kind === k}
              className={kind === k ? 'tab active' : 'tab'}
              onClick={() => setKind(k)}
            >
              {k === 'movie' ? 'Movie' : k === 'series' ? 'Series' : 'Book'}
            </button>
          ))}
        </div>
      </header>

      <div className="add-controls">
        <input
          autoFocus
          type="search"
          placeholder={
            kind === 'movie'
              ? 'Search movies on TMDB…'
              : kind === 'series'
                ? 'Search series on TMDB…'
                : 'Search books on Open Library…'
          }
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <label className="inline">
          Root folder{' '}
          <select
            value={rootId ?? ''}
            onChange={(e) => setRootId(e.target.value ? Number(e.target.value) : undefined)}
          >
            <option value="">(none)</option>
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
            no root folder holds {kind === 'series' ? 'TV' : kind === 'book' ? 'books' : 'movies'} —
            add one in Settings, or the item lands with no folder
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
        {kind === 'series' && (
          <label className="inline" title="Which seasons start monitored — untick more later per season/episode">
            Seasons:{' '}
            <select value={monitor} onChange={(e) => setMonitor(e.target.value as typeof monitor)}>
              <option value="all">all</option>
              <option value="latest">latest only</option>
              <option value="none">none</option>
            </select>
          </label>
        )}
        <label className="inline" title="Automatically search and grab the best release right after adding">
          <input
            type="checkbox"
            checked={searchNow}
            onChange={(e) => setSearchNow(e.target.checked)}
          />{' '}
          Search on add
        </label>
      </div>

      {results.isError && <div className="banner warning">{String((results.error as Error).message)}</div>}
      {add.isError && <div className="banner warning">{String((add.error as Error).message)}</div>}

      {added.length > 0 && (
        <div className="added-strip" data-testid="added-strip">
          <span>
            <strong>
              Added {added.length} {added.length === 1 ? 'item' : 'items'}
            </strong>{' '}
            — keep searching, or
          </span>
          <button onClick={() => navigate({ to: '/' })}>Go to library</button>
          <ul>
            {added.map((a) => (
              <li key={a.key}>
                <Link to="/library/$id" params={{ id: String(a.id) }}>
                  {a.title}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      )}

      <ul className="result-list">
        {results.data?.map((r) => (
          <li key={resultKey(r)} className="result">
            {r.posterPath ? (
              <img src={posterUrl(r.posterPath, 'w185')} alt="" loading="lazy" />
            ) : (
              <div className="poster-fallback small">{r.title.slice(0, 1)}</div>
            )}
            <div className="result-body">
              <div className="result-title">
                {r.title} <span className="muted">({r.year || '—'})</span>
                {r.author && <span className="muted"> · {r.author}</span>}
              </div>
              <p className="muted clamp">{r.overview}</p>
            </div>
            <div className="result-action">
              {(() => {
                const just = addedByKey.get(resultKey(r))
                if (just) {
                  return (
                    <>
                      <span className="pill pill-ok">added</span>{' '}
                      <Link to="/library/$id" params={{ id: String(just.id) }}>
                        Open
                      </Link>
                    </>
                  )
                }
                if (r.inLibrary) return <span className="pill pill-ok">in library</span>
                const busy = pendingKey === resultKey(r)
                return (
                  <button className="btn-accent" disabled={busy} onClick={() => add.mutate(r)}>
                    {busy ? 'Adding…' : 'Add'}
                  </button>
                )
              })()}
            </div>
          </li>
        ))}
        {results.isFetching && (
          <li className="muted result-note">Searching…</li>
        )}
        {results.data?.length === 0 && (
          <li className="muted result-note">No results for “{debouncedQuery}”.</li>
        )}
      </ul>
    </>
  )
}
