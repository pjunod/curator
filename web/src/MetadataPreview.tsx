import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ApiError, getMetadataPreview, posterUrl } from './api'
import type { BookType, SearchResult } from './api'
import { externalLinks, previewKey, previewQuery, previewState, previewIdentityConflict } from './metadataPreview'
import './MetadataPreview.css'

export function MetadataPreviewDrawer({ item, bookType, options, onClose, onAdd, busy, canAdd, addedId, addError }: {
  item: SearchResult; bookType: BookType; options: ReactNode; onClose: () => void; onAdd: () => void
  busy: boolean; canAdd: boolean; addedId?: number; addError?: string
}) {
  const key = previewKey(item)
  const preview = useQuery({ queryKey: ['metadata-preview', key], queryFn: () => getMetadataPreview(previewQuery(key!)), enabled: !!key, retry: false, staleTime: 0, refetchOnMount: 'always' })
  const remoteConflict = useRef(false)
  if (preview.error instanceof ApiError && preview.error.code === 'identity_conflict') remoteConflict.current = true
  else if (preview.isSuccess && !preview.isFetching) remoteConflict.current = false
  const conflict = remoteConflict.current || previewIdentityConflict(item, preview.data)
  const data = conflict ? undefined : preview.data
  const state = previewState(item, bookType, data, conflict)
  const links = externalLinks(item, data, conflict)
  const panel = useRef<HTMLDivElement>(null)
  const close = useRef(onClose)
  close.current = onClose
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    const host = panel.current?.parentElement
    const siblings = Array.from(document.body.children).filter((node): node is HTMLElement => node instanceof HTMLElement && node !== host)
    const inert = siblings.map((node) => node.inert)
    siblings.forEach((node) => { node.inert = true })
    const overflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    panel.current?.focus()
    const keyboard = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); close.current(); return }
      if (event.key !== 'Tab') return
      const focusable = Array.from(panel.current?.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), summary, [tabindex="0"]') ?? []).filter((node) => node.getClientRects().length > 0)
      const first = focusable[0], last = focusable.at(-1)
      if (!first) { event.preventDefault(); panel.current?.focus() }
      else if (event.shiftKey && (document.activeElement === first || document.activeElement === panel.current)) { event.preventDefault(); last?.focus() }
      else if (!event.shiftKey && (document.activeElement === last || document.activeElement === panel.current)) { event.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', keyboard, true)
    return () => {
      siblings.forEach((node, i) => { node.inert = inert[i] })
      document.body.style.overflow = overflow
      document.removeEventListener('keydown', keyboard, true)
      if (previous?.isConnected) previous.focus({ preventScroll: true })
    }
  }, [])
  const openId = addedId ?? state.libraryItemId
  const poster = data?.posterPath || item.posterPath
  return createPortal(
    <div className="preview-backdrop" onClick={(event) => { if (event.target === event.currentTarget) onClose() }}>
      <div className="metadata-preview" ref={panel} role="dialog" aria-modal="true" aria-labelledby="preview-title" tabIndex={-1}>
        <header className="preview-header"><span className="muted">{item.kind === 'series' ? 'Series' : item.kind === 'book' ? 'Book' : 'Movie'} details</span><button aria-label="Close details" onClick={onClose}>×</button></header>
        <div className="preview-scroll">
          <div className="preview-hero">
            {poster ? <img src={posterUrl(poster, 'w185')} alt="" /> : <div className="poster-fallback small">{item.title.slice(0, 1)}</div>}
            <div><h2 id="preview-title">{data?.title || item.title}</h2><p className="muted">{data?.year || item.year || 'Year unknown'}{(data?.author || item.author) && ` · ${data?.author || item.author}`}</p>
              {data?.runtimeMinutes && <p className="muted">{data.runtimeMinutes} min{item.kind === 'series' ? ' per episode' : ''}</p>}
              {data?.status && <p className="muted">{data.status}{data.previewSource && ` · ${data.previewSource}`}</p>}
            </div>
          </div>
          {!!data?.genres?.length && <p className="preview-genres">{data.genres.join(' · ')}</p>}
          <h3>Synopsis</h3><p className="preview-synopsis">{data?.overview || item.overview || 'No synopsis available.'}</p>
          {preview.isFetching && <p className="muted" role="status">Loading more details…</p>}
          {(preview.isError || conflict) && <div className="banner warning" role="alert">{conflict ? 'The provider returned conflicting identities. Resolve this before adding.' : 'More details are unavailable. The original search information is still shown.'} <button onClick={() => void preview.refetch()}>Retry</button></div>}
          {data?.ownership === 'unknown' && <p className="muted">Library ownership could not be checked.</p>}
          {state.blocked && !conflict && <p className="banner warning">{data?.addBlockReason || 'This title cannot currently be added.'}</p>}
          {links.length > 0 && <nav className="preview-links" aria-label="External databases">{links.map((link) => <a key={link.label} href={link.url} target="_blank" rel="noopener noreferrer">{link.label} ↗<span className="sr-only"> (opens in a new tab)</span></a>)}</nav>}
        </div>
        <footer className="preview-footer">
          {!state.owned && !addedId && <details><summary>Add options</summary><div className="preview-options">{options}</div></details>}
          {addError && <div className="banner warning" role="alert">{addError}</div>}
          <div className="preview-actions">
            {addedId || state.owned ? <span className="pill pill-ok">{addedId ? 'Added' : item.kind === 'book' ? `${bookType} in library` : 'In library'}</span> : <button className="btn-accent" disabled={busy || !canAdd || state.blocked} onClick={onAdd}>{busy ? 'Adding…' : item.kind === 'book' ? `Add ${bookType}` : 'Add to library'}</button>}
            {openId && <Link to="/library/$id" params={{ id: String(openId) }}>Open in library</Link>}
          </div>
          {!canAdd && !state.owned && <p className="muted">Add a matching root folder in Settings first.</p>}
        </footer>
      </div>
    </div>, document.body,
  )
}
