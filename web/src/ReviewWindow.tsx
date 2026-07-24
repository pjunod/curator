import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { Proposal } from './api'
import { getReviewQueue, ignoreDir } from './api'

const PAGE_SIZE = 25

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
    queryKey: ['review', debounced, page],
    queryFn: () => getReviewQueue(debounced, PAGE_SIZE, page * PAGE_SIZE),
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
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
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

        <div className="form-row">
          <input
            type="search"
            placeholder="Filter by folder or title…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {counts && (
            <span className="muted">
              {counts.total} total · {counts.ambiguous} ambiguous · {counts.none} no match
            </span>
          )}
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
          <span className="muted">
            {total === 0
              ? 'No entries'
              : `${page * PAGE_SIZE + 1}–${Math.min((page + 1) * PAGE_SIZE, total)} of ${total}`}
          </span>
          <div className="pager">
            <button disabled={page === 0} onClick={() => setPage(0)}>
              « First
            </button>
            <button disabled={page === 0} onClick={() => setPage((n) => n - 1)}>
              ‹ Prev
            </button>
            <span className="muted">
              Page {page + 1} of {pages}
            </span>
            <button disabled={page + 1 >= pages} onClick={() => setPage((n) => n + 1)}>
              Next ›
            </button>
            <button disabled={page + 1 >= pages} onClick={() => setPage(pages - 1)}>
              Last »
            </button>
          </div>
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
