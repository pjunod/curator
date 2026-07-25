import { useEffect, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import type { MediaKind, SearchResult } from '../api'
import { addLibraryItem, getProfiles, getRootFolders, posterUrl, searchMetadata } from '../api'

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

  const [kind, setKind] = useState<MediaKind>(
    search.kind === 'series' || search.kind === 'book' ? search.kind : 'movie',
  )
  const [query, setQuery] = useState(search.q ?? '')
  const debouncedQuery = useDebounced(query, 400)

  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const [rootId, setRootId] = useState<number | undefined>(undefined)
  const [profileId, setProfileId] = useState<number | ''>('')
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
          : { kind: r.kind, tmdbId: r.tmdbId, ...common },
      )
    },
    onSuccess: (item) => navigate({ to: '/library/$id', params: { id: String(item.id) } }),
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
            value={profileId}
            onChange={(e) => setProfileId(e.target.value ? Number(e.target.value) : '')}
          >
            <option value="">(default)</option>
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

      <ul className="result-list">
        {results.data?.map((r) => (
          <li key={`${r.kind}-${r.olid ?? r.tmdbId}`} className="result">
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
            <div>
              {r.inLibrary ? (
                <span className="pill pill-ok">in library</span>
              ) : (
                <button
                  className="btn-accent"
                  disabled={add.isPending}
                  onClick={() => add.mutate(r)}
                >
                  Add
                </button>
              )}
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
