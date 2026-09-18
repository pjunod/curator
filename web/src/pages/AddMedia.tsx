import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { ApiError } from '../api'
import type { BookType, MediaKind, SearchResult } from '../api'
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
const resultKey = (r: SearchResult, bookType?: BookType) =>
  `${r.kind}-${r.olid ?? ''}-${r.tmdbId}-${r.tvdbId ?? 0}-${r.imdbId ?? ''}${r.kind === 'book' ? `-${bookType ?? 'ebook'}` : ''}`

const searchError = (error: unknown) => {
  if (!(error instanceof ApiError)) return String((error as Error).message)
  switch (error.code) {
    case 'invalid_external_id': return 'That external ID is not valid. Use tvdb:414217, imdb:tt16867040, or tmdb:550.'
    case 'identity_conflict': return 'More than one record claims that identity. Resolve the conflict before adding it.'
    case 'provider_unavailable': return 'The metadata provider is unavailable. Try again when it is reachable.'
    case 'unsupported_hydration': return 'That title was identified, but no configured provider can safely add it.'
    default: return error.message
  }
}

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
  const [bookType, setBookType] = useState<BookType>('ebook')
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
  const defaultProfileId = kind === 'book'
    ? settings.data?.defaultProfiles?.[bookType === 'audiobook' ? 'audiobook' : 'book']
    : settings.data?.defaultProfiles?.[kind]
  const defaultProfileName = profiles.data?.find((p) => p.id === defaultProfileId)?.name
  const ebookSources = new Set(['pdf', 'mobi', 'azw3', 'epub'])
  const audiobookSources = new Set(['mp3', 'wma', 'aac', 'ogg', 'opus', 'm4a', 'm4b', 'flac', 'wav'])
  const eligibleProfiles = (profiles.data ?? []).filter((p) => {
    if (kind !== 'book') return !ebookSources.has(p.target.source) && !audiobookSources.has(p.target.source)
    return bookType === 'ebook' ? ebookSources.has(p.target.source) : audiobookSources.has(p.target.source)
  })
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
  const selectedRootId = eligibleRoots.some((rf) => rf.id === rootId)
    ? rootId
    : eligibleRoots[0]?.id

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
        rootFolderId: selectedRootId,
        qualityProfileId: profileId === '' ? undefined : Number(profileId),
        monitored,
        monitor: r.kind === 'series' ? monitor : undefined,
        searchNow: monitored && searchNow,
      }
      return addLibraryItem(
        r.kind === 'book'
          ? { kind: r.kind, olid: r.olid, bookType, ...common }
          // A series from the provider chain is identified by its TVDB id and
          // has no TMDB one (ADR 0011); sending tmdbId: 0 would ask TMDB for
          // series zero.
          : {
              kind: r.kind,
              tmdbId: r.tmdbId || undefined,
              tvdbId: r.tvdbId,
              imdbId: r.imdbId,
              hydrationSource: r.hydrationSource,
              ...common,
            },
      )
    },
    onMutate: (r: SearchResult) => setPendingKey(resultKey(r, bookType)),
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
      setAdded((prev) => [{ key: resultKey(r, bookType), id: item.id, title: item.title || r.title }, ...prev])
    },
  })

  return (
    <>
      <header className="page-head">
        <h1>Add media</h1>
        <div className="tabs" role="tablist">
          {([
            { label: 'Movie', kind: 'movie' as MediaKind },
            { label: 'Series', kind: 'series' as MediaKind },
            { label: 'Ebook', kind: 'book' as MediaKind, bookType: 'ebook' as BookType },
            { label: 'Audiobook', kind: 'book' as MediaKind, bookType: 'audiobook' as BookType },
          ]).map((tab) => (
            <button
              key={tab.label}
              role="tab"
              aria-selected={kind === tab.kind && (tab.kind !== 'book' || bookType === tab.bookType)}
              className={kind === tab.kind && (tab.kind !== 'book' || bookType === tab.bookType) ? 'tab active' : 'tab'}
              onClick={() => {
                setKind(tab.kind)
                if (tab.bookType) setBookType(tab.bookType)
                setProfileId('')
              }}
            >
              {tab.label}
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
              ? 'Search title, imdb:tt0137523, or tmdb:550…'
              : kind === 'series'
                ? 'Search title, tvdb:414217, or imdb:tt16867040…'
                : 'Search books on Open Library…'
          }
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
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
            no root folder holds {kind === 'series' ? 'TV' : kind === 'book' ? 'books' : 'movies'} —
            add one in Settings before adding or downloading this title
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
            {eligibleProfiles.map((p) => (
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

      {results.isError && <div className="banner warning">{searchError(results.error)}</div>}
      {add.isError && <div className="banner warning">{searchError(add.error)}</div>}

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
                const just = addedByKey.get(resultKey(r, bookType))
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
                const selectedEditionPresent = r.kind === 'book' && (r.bookTypes ?? []).includes(bookType)
                if (selectedEditionPresent) {
                  return <span className="pill pill-ok">{bookType} in library</span>
                }
                if (r.inLibrary && r.kind !== 'book') return <span className="pill pill-ok">in library</span>
                const busy = pendingKey === resultKey(r, bookType)
                return (
                  <button
                    className="btn-accent"
                    disabled={busy || !selectedRootId}
                    title={!selectedRootId ? 'Add a matching root folder in Settings first' : undefined}
                    onClick={() => add.mutate(r)}
                  >
                    {busy ? 'Adding…' : r.kind === 'book' ? `Add ${bookType}` : 'Add'}
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
