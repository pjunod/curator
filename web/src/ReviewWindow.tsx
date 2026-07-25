import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { AdoptionCandidate, MediaKind, Proposal, ReviewCounts } from './api'
import {
  adoptExact,
  adoptOne,
  ApiError,
  getReviewQueue,
  ignoreDir,
  searchMetadata,
} from './api'
import { clampPage, Pager, PageSizePicker } from './Pager'

// Kind tabs, matching the library's vocabulary so the same content is called
// the same thing in both places.
const KIND_TABS: { label: string; kind?: MediaKind }[] = [
  { label: 'All' },
  { label: 'Movies', kind: 'movie' },
  { label: 'TV', kind: 'series' },
  { label: 'Books', kind: 'book' },
]

function tabCount(counts: ReviewCounts | undefined, kind?: MediaKind): number | undefined {
  if (!counts) return undefined
  if (!kind) return counts.total
  return kind === 'movie' ? counts.movie : kind === 'series' ? counts.series : counts.book
}

/**
 * The adoption review queue, in a window with pages.
 *
 * A folder of several hundred movies used to render as several hundred rows
 * inline on the settings page. Pagination is done at the API (see
 * /library/review) rather than by slicing a fully-downloaded array, so the
 * settings page never fetches six hundred proposals to show a count.
 */
export function ReviewWindow({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [page, setPage] = useState(0)
  const [size, setSize] = useState(25)
  const [kind, setKind] = useState<MediaKind | undefined>(undefined)
  const [query, setQuery] = useState('')
  const [debounced, setDebounced] = useState('')

  useEffect(() => {
    const t = setTimeout(() => {
      setDebounced(query)
      setPage(0) // a new filter starts at the top; page 7 of the old one is meaningless
    }, 250)
    return () => clearTimeout(t)
  }, [query])

  // Escape closes, which is the one keyboard behaviour a modal owes you.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  const review = useQuery({
    queryKey: ['review', kind ?? 'all', debounced, size, page],
    queryFn: () => getReviewQueue(kind, debounced, size, page * size),
  })

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['review'] })
    void qc.invalidateQueries({ queryKey: ['scan-report'] })
    void qc.invalidateQueries({ queryKey: ['ignored-dirs'] })
    void qc.invalidateQueries({ queryKey: ['library'] })
  }

  const dismiss = useMutation({
    mutationFn: (path: string) => ignoreDir(path),
    onSuccess: refresh,
  })
  // Accepting a match happens here, in place. It used to be a link to the
  // Add page, which meant searching again, clicking Add beside the title you
  // had already picked, and being dropped on the item's page — having lost
  // your place in the queue.
  // Failures are held per folder. A single shared error banner put the
  // message at the top of the window, detached from the row that caused it —
  // so a click on row seven reported itself above row one.
  const [rowErrors, setRowErrors] = useState<Record<string, { message: string; conflict: boolean }>>(
    {},
  )
  const clearRowError = (path: string) =>
    setRowErrors((prev) => {
      const { [path]: _drop, ...rest } = prev
      return rest
    })

  const accept = useMutation({
    mutationFn: ({ path, pick, force }: { path: string; pick: AdoptionCandidate; force?: boolean }) =>
      adoptOne(path, pick, force),
    onSuccess: (_data, vars) => {
      clearRowError(vars.path)
      refresh()
    },
    onError: (err, vars) => {
      setRowErrors((prev) => ({
        ...prev,
        [vars.path]: {
          message: (err as Error).message,
          // 409 means the user can resolve it by moving the entry here.
          conflict: err instanceof ApiError && err.status === 409,
        },
      }))
    },
  })
  const acceptAll = useMutation({
    mutationFn: adoptExact,
    onSuccess: refresh,
  })

  const total = review.data?.total ?? 0
  const counts = review.data?.counts

  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="Folders needing review"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <header className="modal-head">
          <h2>Folders needing review</h2>
          <button onClick={onClose} aria-label="Close">
            ✕
          </button>
        </header>

        <div className="tabs" role="tablist">
          {KIND_TABS.map((t) => (
            <button
              key={t.label}
              role="tab"
              aria-selected={kind === t.kind}
              className={kind === t.kind ? 'tab active' : 'tab'}
              onClick={() => {
                setKind(t.kind)
                setPage(0) // a different tab is a different list
              }}
            >
              {t.label}
              {tabCount(counts, t.kind) !== undefined && (
                <span className="tab-count">{tabCount(counts, t.kind)}</span>
              )}
            </button>
          ))}
        </div>

        <div className="form-row">
          <button
            className="btn-accent"
            disabled={acceptAll.isPending}
            title="Adopt every folder whose match is unambiguous"
            onClick={() => acceptAll.mutate()}
          >
            {acceptAll.isPending ? 'Adopting…' : 'Accept all confident matches'}
          </button>
          {acceptAll.data && (
            <span className="ok-text">
              {acceptAll.data.adopted.length} adopted · {acceptAll.data.review.length} still need a
              look
            </span>
          )}
          {acceptAll.isError && (
            <span className="error-text">{String((acceptAll.error as Error).message)}</span>
          )}
        </div>

        {acceptAll.data && acceptAll.data.failures.length > 0 && (
          <details className="banner warning">
            <summary>
              {acceptAll.data.failures.length} folder
              {acceptAll.data.failures.length > 1 ? 's' : ''} could not be adopted
            </summary>
            <ul className="unmatched-list">
              {acceptAll.data.failures.map((f) => (
                <li key={f}>
                  <span className="mono">{f}</span>
                </li>
              ))}
            </ul>
          </details>
        )}

        <div className="form-row">
          <input
            type="search"
            placeholder="Filter by folder or title…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {counts && (
            <span className="muted">
              {counts.ambiguous} ambiguous · {counts.none} no match
              {counts.unknown > 0 && ` · ${counts.unknown} from mixed roots`}
            </span>
          )}
          <PageSizePicker size={size} onChange={(n) => { setSize(n); setPage(0) }} />
        </div>

        <div className="modal-body">
          {review.isPending && <p className="muted">Loading…</p>}
          {review.isError && (
            <div className="banner warning">{String((review.error as Error).message)}</div>
          )}
          {review.data && review.data.items.length === 0 && (
            <p className="muted">
              {debounced
                ? 'Nothing matches that filter.'
                : 'Nothing to review — every folder was either matched or dismissed.'}
            </p>
          )}
          {review.data?.items.map((p) => (
            <ReviewRow
              key={p.path}
              p={p}
              busy={accept.isPending || acceptAll.isPending}
              error={rowErrors[p.path]}
              onAccept={(pick, force) => accept.mutate({ path: p.path, pick, force })}
              onDismiss={() => dismiss.mutate(p.path)}
            />
          ))}
        </div>

        <footer className="modal-foot">
          <Pager
            page={page}
            size={size}
            total={total}
            onPage={(n) => setPage(clampPage(n, total, size))}
          />
        </footer>
      </div>
    </div>
  )
}

/** One candidate folder, with whatever adoption managed to work out. */
function ReviewRow({
  p,
  onAccept,
  onDismiss,
  busy,
  error,
}: {
  p: Proposal
  onAccept: (pick: AdoptionCandidate, force?: boolean) => void
  onDismiss: () => void
  busy: boolean
  error?: { message: string; conflict: boolean }
}) {
  const [searching, setSearching] = useState(false)
  const [term, setTerm] = useState(p.parsedTitle)
  const [debounced, setDebounced] = useState(p.parsedTitle)
  // A mixed root resolves no kind, so the user picks which provider to ask.
  const [searchKind, setSearchKind] = useState<MediaKind>(p.kind ?? 'movie')

  useEffect(() => {
    const t = setTimeout(() => setDebounced(term), 300)
    return () => clearTimeout(t)
  }, [term])

  const results = useQuery({
    queryKey: ['metadata-search', searchKind, debounced],
    queryFn: () => searchMetadata(searchKind, debounced),
    enabled: searching && debounced.trim().length > 1,
    retry: false,
  })

  return (
    <div className={error ? 'review-row has-error' : 'review-row'}>
      <div className="review-main">
        <span className="mono">{p.name}</span>
        <span className="muted review-parsed">
          read as <strong>{p.parsedTitle}</strong>
          {p.parsedYear ? ` (${p.parsedYear})` : ''}
          {p.kind ? ` · ${p.kind}` : ' · kind unknown (mixed root)'}
          {p.confidence === 'exact' && ' · confident'}
        </span>
      </div>

      {/* Said before the click, not after it. A chip reading "Cunk on… aka
          Cunk on Earth" looks like the answer; the fact that another folder
          already holds that entry is the part that changes what to do. */}
      {(p.heldBy || (p.sharedWith?.length ?? 0) > 0) && (
        <p className="review-note muted">
          {p.heldBy ? (
            <>
              <strong>{p.candidates[0]?.title}</strong> is already this library&rsquo;s{' '}
              <span className="mono">{p.heldBy}</span>. The provider lists both names under one
              title, so it cannot also be this folder — search for a separate entry, or dismiss
              this folder.
            </>
          ) : (
            <>
              {p.sharedWith!.length + 1} folders answer to{' '}
              <strong>{p.candidates[0]?.title}</strong> through its other names, so that title
              covers all of them rather than being any one&rsquo;s match. Adopting one leaves the
              rest to search or dismiss.
            </>
          )}
        </p>
      )}

      <div className="review-candidates">
        {p.candidates.length === 0 && <span className="muted">no match —</span>}
        {p.candidates.map((c, i) => (
          <button
            key={`${c.kind}-${c.tmdbId}-${c.olid ?? ''}`}
            className={i === 0 && p.confidence === 'exact' ? 'candidate best' : 'candidate'}
            disabled={busy}
            title={
              c.altTitles?.length
                ? `${c.title}${c.year ? ` (${c.year})` : ''} is also released as ${c.altTitles.join(', ')}`
                : `Adopt this folder as ${c.title}${c.year ? ` (${c.year})` : ''}`
            }
            onClick={() => onAccept(c)}
          >
            {c.title}
            {c.year ? ` (${c.year})` : ''}
            {/* A chip that names a different film than the folder does looks
                like a bad suggestion until it says why it is here. */}
            {c.altTitles?.length ? (
              <span className="candidate-aka"> aka {c.altTitles[0]}</span>
            ) : null}
          </button>
        ))}
        {/* Searching happens here rather than on the Add page. Sending the
            user there created a library item at the naming-rule path and left
            this folder unmatched — a phantom entry pointing at a directory
            that does not exist, and the folder still in the queue. */}
        <button
          className="candidate search-link"
          disabled={busy}
          title={`Search ${p.kind === 'series' ? 'TV' : p.kind === 'book' ? 'books' : 'movies'} for another title`}
          onClick={() => setSearching((v) => !v)}
        >
          {searching ? 'cancel search' : 'search…'}
        </button>
      </div>

      <button
        className="link-btn"
        title="Never offer this folder again"
        disabled={busy}
        onClick={onDismiss}
      >
        not media
      </button>

      {searching && (
        <div className="review-search">
          <input
            type="search"
            autoFocus
            value={term}
            placeholder="Search for a title…"
            onChange={(e) => setTerm(e.target.value)}
          />
          {!p.kind && (
            <select value={searchKind} onChange={(e) => setSearchKind(e.target.value as MediaKind)}>
              <option value="movie">Movies</option>
              <option value="series">TV</option>
              <option value="book">Books</option>
            </select>
          )}
          {results.isPending && debounced.trim().length > 1 && (
            <span className="muted">searching…</span>
          )}
          {results.isError && (
            <span className="error-text">{String((results.error as Error).message)}</span>
          )}
          {results.data?.length === 0 && <span className="muted">nothing found</span>}
          {results.data?.slice(0, 8).map((r) => (
            <button
              key={`${r.kind}-${r.tmdbId}-${r.olid ?? ''}`}
              className="candidate"
              disabled={busy}
              title={`Adopt this folder as ${r.title}${r.year ? ` (${r.year})` : ''}`}
              onClick={() =>
                onAccept({
                  kind: r.kind,
                  tmdbId: r.tmdbId,
                  olid: r.olid,
                  author: r.author,
                  title: r.title,
                  year: r.year,
                })
              }
            >
              {r.title}
              {r.year ? ` (${r.year})` : ''}
            </button>
          ))}
        </div>
      )}

      {error && (
        <div className="review-error">
          <span className="error-text">{error.message}</span>
          {/* Not offered for the umbrella case: the other folder is a real
              folder with a real claim, and moving the entry to this one only
              relocates the problem. heldBy already said as much above. */}
          {error.conflict && p.candidates.length > 0 && !p.heldBy && (
            <button
              disabled={busy}
              title="Move the existing library entry to this folder"
              onClick={() => onAccept(p.candidates[0], true)}
            >
              Point the entry at this folder
            </button>
          )}
        </div>
      )}
    </div>
  )
}
