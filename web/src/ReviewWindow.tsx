import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { MediaKind, Proposal, ReviewCounts } from './api'
import { getReviewQueue, ignoreDir } from './api'
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

  const dismiss = useMutation({
    mutationFn: (path: string) => ignoreDir(path),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['review'] })
      void qc.invalidateQueries({ queryKey: ['scan-report'] })
      void qc.invalidateQueries({ queryKey: ['ignored-dirs'] })
    },
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
            <ReviewRow key={p.path} p={p} onDismiss={() => dismiss.mutate(p.path)} />
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
function ReviewRow({ p, onDismiss }: { p: Proposal; onDismiss: () => void }) {
  return (
    <div className="review-row">
      <div className="review-main">
        <span className="mono">{p.name}</span>
        <span className="muted review-parsed">
          read as <strong>{p.parsedTitle}</strong>
          {p.parsedYear ? ` (${p.parsedYear})` : ''}
          {p.kind ? ` · ${p.kind}` : ' · kind unknown (mixed root)'}
        </span>
      </div>

      {p.candidates.length > 0 ? (
        <div className="review-candidates">
          {p.candidates.map((c) => (
            <Link
              key={`${c.kind}-${c.tmdbId}-${c.olid ?? ''}`}
              to="/add"
              search={{ q: c.title, kind: c.kind }}
              className="candidate"
              title={c.overview}
            >
              {c.title}
              {c.year ? ` (${c.year})` : ''}
            </Link>
          ))}
        </div>
      ) : (
        // No proposal to confirm, so fall back to the manual route rather
        // than leaving the row with nothing actionable on it.
        <div className="review-candidates">
          <span className="muted">no match —</span>
          <Link to="/add" search={{ q: p.parsedTitle, kind: 'movie' }} className="candidate">
            search movies
          </Link>
          <Link to="/add" search={{ q: p.parsedTitle, kind: 'series' }} className="candidate">
            search series
          </Link>
        </div>
      )}

      <button className="link-btn" title="Never offer this folder again" onClick={onDismiss}>
        not media
      </button>
    </div>
  )
}
