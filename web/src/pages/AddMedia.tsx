import { useEffect, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import type { MediaKind, SearchResult } from '../api'
import { addLibraryItem, getRootFolders, posterUrl, searchMetadata } from '../api'

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

  const [kind, setKind] = useState<MediaKind>(search.kind === 'series' ? 'series' : 'movie')
  const [query, setQuery] = useState(search.q ?? '')
  const debouncedQuery = useDebounced(query, 400)

  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const [rootId, setRootId] = useState<number | undefined>(undefined)
  const [monitored, setMonitored] = useState(true)

  useEffect(() => {
    if (rootId === undefined && roots.data && roots.data.length > 0) {
      setRootId(roots.data[0].id)
    }
  }, [roots.data, rootId])

  const results = useQuery({
    queryKey: ['metadata-search', kind, debouncedQuery],
    queryFn: () => searchMetadata(kind, debouncedQuery),
    enabled: debouncedQuery.trim().length > 1,
    retry: false,
  })

  const add = useMutation({
    mutationFn: (r: SearchResult) =>
      addLibraryItem({ kind: r.kind, tmdbId: r.tmdbId, rootFolderId: rootId, monitored }),
    onSuccess: (item) => navigate({ to: '/library/$id', params: { id: String(item.id) } }),
  })

  return (
    <>
      <header className="page-head">
        <h1>Add media</h1>
        <div className="tabs" role="tablist">
          {(['movie', 'series'] as MediaKind[]).map((k) => (
            <button
              key={k}
              role="tab"
              aria-selected={kind === k}
              className={kind === k ? 'tab active' : 'tab'}
              onClick={() => setKind(k)}
            >
              {k === 'movie' ? 'Movie' : 'Series'}
            </button>
          ))}
        </div>
      </header>

      <div className="add-controls">
        <input
          autoFocus
          type="search"
          placeholder={kind === 'movie' ? 'Search movies on TMDB…' : 'Search series on TMDB…'}
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
            {roots.data?.map((rf) => (
              <option key={rf.id} value={rf.id}>
                {rf.path}
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
      </div>

      {results.isError && <div className="banner warning">{String((results.error as Error).message)}</div>}
      {add.isError && <div className="banner warning">{String((add.error as Error).message)}</div>}

      <ul className="result-list">
        {results.data?.map((r) => (
          <li key={`${r.kind}-${r.tmdbId}`} className="result">
            {r.posterPath ? (
              <img src={posterUrl(r.posterPath, 'w185')} alt="" loading="lazy" />
            ) : (
              <div className="poster-fallback small">{r.title.slice(0, 1)}</div>
            )}
            <div className="result-body">
              <div className="result-title">
                {r.title} <span className="muted">({r.year || '—'})</span>
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
