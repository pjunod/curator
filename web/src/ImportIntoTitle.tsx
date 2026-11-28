import { useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ActionDialog } from './ActionDialog'
import { ImportFileDialog } from './ImportFileDialog'
import {
  addLibraryItem,
  deleteLibraryItem,
  getLibrary,
  getLibraryItem,
  getRootFolders,
  searchMetadata,
  type MediaItemDetail,
} from './api'
import { rematchCandidates, type RematchCandidate } from './rematch'
import './RematchDialog.css'

type Kind = 'movie' | 'series'

/**
 * Import files… from the Library: files you have — a disc you ripped, files
 * you placed yourself — for a title that may not be in the library yet.
 * Choose the title first (library titles, then the provider's, as Re-match
 * lists them); a new one is added unmonitored with no search, because you
 * already have the files. Then the item page's own Import file… takes over:
 * browse, episodes, source, confirm. A title added here and left without an
 * import is removed again, so backing out leaves nothing behind.
 */
export function ImportIntoTitleButton() {
  const trigger = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false)
  return (
    <>
      <button ref={trigger} onClick={() => setOpen(true)}
        title="Import files you already have — a disc you ripped, or files you placed yourself — into a title, adding it if it is not in your library">
        Import files…
      </button>
      {open && <ImportIntoTitleFlow returnFocus={trigger} onClose={() => setOpen(false)} />}
    </>
  )
}

function ImportIntoTitleFlow({ onClose, returnFocus }: { onClose: () => void; returnFocus: React.RefObject<HTMLButtonElement | null> }) {
  const qc = useQueryClient()
  const [kind, setKind] = useState<Kind>('movie')
  const [draft, setDraft] = useState('')
  const [query, setQuery] = useState('')
  const [picked, setPicked] = useState<RematchCandidate | null>(null)
  const [rootId, setRootId] = useState(0)
  const [item, setItem] = useState<MediaItemDetail | null>(null)
  const [addedId, setAddedId] = useState(0)
  const started = useRef(false)

  const library = useQuery({ queryKey: ['library', kind], queryFn: () => getLibrary(kind), enabled: query !== '' })
  const search = useQuery({ queryKey: ['metadata-search', kind, query], queryFn: () => searchMetadata(kind, query), enabled: query !== '', retry: false })
  const candidates = useMemo(() => {
    const listed = rematchCandidates(library.data ?? [], search.data ?? [], query, 0)
    // A provider hit the library already holds (found by id, or under
    // another title such as tmdb:550) is that library title.
    const have = new Set(listed.map((c) => c.target.itemId).filter(Boolean))
    for (const r of search.data ?? []) {
      if (!r.inLibrary || !r.libraryItemId || have.has(r.libraryItemId)) continue
      have.add(r.libraryItemId)
      listed.push({ key: `lib-${r.libraryItemId}`, title: r.title, year: r.year || undefined, detail: 'in your library', target: { itemId: r.libraryItemId }, inLibrary: true, current: false })
    }
    return listed
  }, [library.data, search.data, query])
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const usable = (roots.data ?? []).filter((r) => (r.kind === 'mixed' || r.kind === kind) && r.accessible)
  const chosenRoot = usable.some((r) => r.id === rootId) ? rootId : usable[0]?.id || 0
  const [closing, setClosing] = useState(false)

  const proceed = useMutation({
    mutationFn: async (c: RematchCandidate): Promise<{ item: MediaItemDetail; added: number }> => {
      if (c.target.itemId) return { item: await getLibraryItem(c.target.itemId), added: 0 }
      const t = c.target
      const added = await addLibraryItem({
        kind, tmdbId: t.tmdbId, tvdbId: t.tvdbId, imdbId: t.imdbId, hydrationSource: t.hydrationSource,
        rootFolderId: chosenRoot, monitored: false, monitor: kind === 'series' ? 'none' : undefined, searchNow: false,
      })
      // The add answers with the stored title; read it back as the item page does.
      return { item: await getLibraryItem(added.id), added: added.id }
    },
    onSuccess: ({ item, added }) => {
      setItem(item)
      setAddedId(added)
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
  })

  const finish = async () => {
    if (closing) return
    setClosing(true)
    if (addedId && !started.current) {
      // Added only for this import, and nothing was imported: take it back
      // out — unless files reached it some other way meanwhile (a disk
      // scan). Deleting removes the record only; nothing on disk is touched.
      try {
        const now = await getLibraryItem(addedId)
        if (now.files.length === 0) await deleteLibraryItem(addedId)
      } catch { /* it stays, visible and unmonitored */ }
      void qc.invalidateQueries({ queryKey: ['library'] })
    }
    onClose()
  }
  // While the add is in flight the dialog cannot be dismissed: the title it
  // is adding must reach the import, or be taken back out.
  const dismiss = () => { if (!proceed.isPending) onClose() }

  if (item) {
    return <ImportFileDialog item={item} returnFocus={returnFocus} onStarted={() => { started.current = true }} onClose={() => void finish()} />
  }

  const isNew = !!picked && !picked.target.itemId
  return (
    <ActionDialog title="Import files" className="rmd" onClose={dismiss} returnFocus={returnFocus}>
      <p className="muted">Which title are these files? Titles in your library are listed first; a title that is not there yet is added, unmonitored and without a search.</p>
      <div className="rmd-scope" role="radiogroup" aria-label="Kind">
        <label><input type="radio" name="import-kind" checked={kind === 'movie'} onChange={() => { setKind('movie'); setPicked(null) }} /> Movie</label>
        <label><input type="radio" name="import-kind" checked={kind === 'series'} onChange={() => { setKind('series'); setPicked(null) }} /> Series</label>
      </div>
      <form className="rmd-search" onSubmit={(e) => { e.preventDefault(); setPicked(null); setQuery(draft.trim()) }}>
        <input aria-label="Title to search for" placeholder="Title, or tmdb:550" value={draft} onChange={(e) => setDraft(e.target.value)} />
        <button type="submit" disabled={!draft.trim()}>Search</button>
      </form>
      <div role="status">
      {(library.isFetching || search.isFetching) && <span className="muted">Searching…</span>}
      {search.isError && <span className="muted">Provider search unavailable: {(search.error as Error).message}. Library titles are still listed.</span>}
      {query && !library.isFetching && !search.isFetching && candidates.length === 0 && <span className="muted">Nothing found for “{query}”.</span>}
      </div>
      {candidates.length > 0 && (
        <ul className="rmd-cands" aria-label="Titles">
          {candidates.map((c) => (
            <li key={c.key}>
              <label className={`rmd-cand${picked?.key === c.key ? ' sel' : ''}`}>
                <input type="radio" name="import-target" checked={picked?.key === c.key} onChange={() => setPicked(c)} />
                <span className="rmd-cand-text"><b>{c.title}</b>{c.year ? <span className="muted"> ({c.year})</span> : null}<br />
                  <span className="muted">{c.inLibrary ? 'in your library' : c.detail.replace('added to the library', 'added to your library, unmonitored')}</span></span>
                {c.inLibrary ? <span className="pill pill-ok">in your library</span> : null}
              </label>
            </li>
          ))}
        </ul>
      )}
      {isNew && (
        <label className="rmd-copy">Root folder{' '}
          <select value={chosenRoot} onChange={(e) => setRootId(Number(e.target.value))}>
            {usable.length === 0 && <option value={0}>No reachable root folder holds {kind === 'series' ? 'TV' : 'movies'}</option>}
            {usable.map((r) => <option key={r.id} value={r.id}>{r.path}{r.kind === 'mixed' ? ' (mixed)' : ''}</option>)}
          </select>
        </label>
      )}
      {proceed.isError && <div className="banner warning" role="alert">{(proceed.error as Error).message}</div>}
      <div className="row-actions">
        <button type="button" onClick={dismiss} disabled={proceed.isPending}>Cancel</button>
        <button type="button" className="btn-accent" disabled={!picked || (isNew && !chosenRoot) || proceed.isPending}
          onClick={() => picked && proceed.mutate(picked)}>
          {proceed.isPending ? 'Adding…' : isNew ? 'Add and choose files' : 'Choose files'}
        </button>
      </div>
    </ActionDialog>
  )
}
