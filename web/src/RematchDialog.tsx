import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ActionDialog, ActionNotice } from './ActionDialog'
import {
  ApiError,
  cancelRematch,
  fmtBytes,
  getLibrary,
  getRematch,
  previewRematch,
  searchMetadata,
  startRematch,
  type MediaFileInfo,
  type MediaItemDetail,
  type RematchChoice,
  type RematchMove,
  type RematchOperation,
  type RematchPreview,
  type RematchTarget,
} from './api'
import { episodeToken, rematchCandidates, type RematchCandidate } from './rematch'
import './RematchDialog.css'

const MODE_TEXT = { rename: 'renamed (same volume)', copy: 'copied across volumes, verified, then the original removed', relink: 'already named this; only the library changes' }
const RUNNING = new Set(['planned', 'moving', 'moved', 'committed', 'notified', 'cancel_pending'])

/**
 * Re-match… (every file of the item) and Move to… (one file): choose the
 * right title or episode, read the preview — every rename, what does not
 * fit, what happens to this item, the blocklist and what Cinema does — then
 * move. Progress and Stop stay in the dialog until it is done (ADR 0027).
 */
export function RematchDialog({ item, file, onClose }: { item: MediaItemDetail; file?: MediaFileInfo; onClose: () => void }) {
  const qc = useQueryClient()
  const series = item.kind === 'series'
  const [scope, setScope] = useState<'same' | 'other'>(file && series ? 'same' : 'other')
  const [draft, setDraft] = useState(item.title)
  const [query, setQuery] = useState('')
  const [picked, setPicked] = useState<RematchCandidate | null>(null)
  const [choices, setChoices] = useState<Record<number, RematchChoice>>({})
  const [copyId, setCopyId] = useState(0)
  const [blocklist, setBlocklist] = useState<boolean | null>(null)
  const [operation, setOperation] = useState<RematchOperation | null>(null)

  const library = useQuery({ queryKey: ['library', item.kind], queryFn: () => getLibrary(item.kind), enabled: scope === 'other' && query !== '' })
  const search = useQuery({ queryKey: ['metadata-search', item.kind, query], queryFn: () => searchMetadata(item.kind, query), enabled: scope === 'other' && query !== '', retry: false })
  const candidates = useMemo(() => rematchCandidates(library.data ?? [], search.data ?? [], query, item.id), [library.data, search.data, query, item.id])

  const target: RematchTarget | null = scope === 'same' ? { itemId: item.id } : picked?.target ?? null
  const request = useMemo(() => target ? {
    target, copyId: copyId || undefined, fileIds: file ? [file.id] : undefined,
    choices: Object.values(choices), blocklist: blocklist ?? undefined,
  } : null, [target, copyId, file, choices, blocklist])
  const preview = useQuery({
    queryKey: ['rematch-preview', item.id, request],
    queryFn: () => previewRematch(item.id, request!),
    enabled: Boolean(request) && !operation,
    retry: false,
  })
  const run = useMutation({
    mutationFn: (p: RematchPreview) => startRematch(item.id, {
      ...request!, requestId: `rematch-${item.id}-${p.fingerprint}-${Date.now()}`,
      sourceRevision: p.sourceRevision, targetRevision: p.targetRevision, fingerprint: p.fingerprint,
    }),
    onSuccess: (op) => setOperation(op),
    onError: (e) => {
      // Blocked or stale: the preview says what changed.
      if (e instanceof ApiError && e.status === 409) void preview.refetch()
    },
  })
  const status = useQuery({
    queryKey: ['rematch', operation?.operationId],
    queryFn: () => getRematch(operation!.operationId),
    enabled: Boolean(operation),
    refetchInterval: (q) => (RUNNING.has(q.state.data?.state ?? 'planned') ? 1000 : false),
  })
  const op = status.data ?? operation
  const finished = op?.state === 'complete'
  useEffect(() => {
    if (finished) void qc.invalidateQueries({ queryKey: ['library-item'] })
  }, [finished, qc])
  const stop = useMutation({ mutationFn: () => cancelRematch(op!.operationId), onSuccess: () => void status.refetch() })

  const p = preview.data
  const pick = (m: RematchMove, value: string) => {
    setChoices((prev) => {
      const next = { ...prev }
      if (value === '') delete next[m.fileId]
      else if (value === 'leave') next[m.fileId] = { fileId: m.fileId, leave: true }
      else {
        const [s, e] = value.split(':').map(Number)
        next[m.fileId] = { fileId: m.fileId, season: s, episodes: [e] }
      }
      return next
    })
  }
  const episodeSelect = (m: RematchMove) => series && p?.episodes && (
    <select aria-label={`Target episode for ${m.from.split('/').pop()}`} value={choices[m.fileId]?.leave ? 'leave' : choices[m.fileId]?.episodes ? `${choices[m.fileId].season}:${choices[m.fileId].episodes![0]}` : ''}
      onChange={(e) => pick(m, e.target.value)}>
      <option value="">{m.episodes.length ? `${episodeToken(m.season, m.episodes)} (from the file)` : 'Choose an episode'}</option>
      {p.episodes.map((ep) => (
        <option key={`${ep.season}:${ep.episode}`} value={`${ep.season}:${ep.episode}`}>
          {episodeToken(ep.season, [ep.episode])}{ep.title ? ` · ${ep.title}` : ''}{ep.hasFile ? ' (has a file)' : ''}
        </option>
      ))}
      <option value="leave">Leave it out</option>
    </select>
  )

  const title = file ? `Move ${file.path.split('/').pop()}` : `Re-match ${item.title}${item.year ? ` (${item.year})` : ''}`
  return (
    <ActionDialog title={title} className="rmd" onClose={onClose}>
      {op ? (
        <RematchProgress op={op} stopping={stop.isPending} onStop={() => stop.mutate()} stopError={stop.error as Error | null} onClose={onClose} />
      ) : (
        <>
          {file && series && (
            <div className="rmd-scope" role="radiogroup" aria-label="Where it goes">
              <label><input type="radio" checked={scope === 'same'} onChange={() => setScope('same')} /> Same show, different episode</label>
              <label><input type="radio" checked={scope === 'other'} onChange={() => setScope('other')} /> A different show</label>
            </div>
          )}
          {scope === 'other' && (
            <>
              <form className="rmd-search" onSubmit={(e) => { e.preventDefault(); setPicked(null); setQuery(draft.trim()) }}>
                <input aria-label="Title to search for" value={draft} onChange={(e) => setDraft(e.target.value)} />
                <button type="submit">Search</button>
              </form>
              {(library.isFetching || search.isFetching) && <span className="muted">Searching…</span>}
              {search.isError && <span className="muted">Provider search unavailable: {(search.error as Error).message}. Library titles are still listed.</span>}
              {query && !library.isFetching && !search.isFetching && candidates.length === 0 && <span className="muted">Nothing found for “{query}”.</span>}
              {candidates.length > 0 && (
                <ul className="rmd-cands" aria-label="Titles">
                  {candidates.map((c) => (
                    <li key={c.key}>
                      <label className={`rmd-cand${picked?.key === c.key ? ' sel' : ''}`}>
                        <input type="radio" name="rmd-target" checked={picked?.key === c.key} disabled={c.current && !file}
                          onChange={() => { setPicked(c); setChoices({}); setCopyId(0) }} />
                        <span className="rmd-cand-text"><b>{c.title}</b>{c.year ? <span className="muted"> ({c.year})</span> : null}<br /><span className="muted">{c.detail}</span></span>
                        {c.current ? <span className="pill">current</span> : c.inLibrary ? <span className="pill pill-ok">in your library</span> : null}
                      </label>
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}

          {preview.isFetching && !p && <span className="muted">Working out what moves where…</span>}
          {preview.isError && <div className="banner warning" role="alert">{(preview.error as Error).message}</div>}
          {p && target && (
            <>
              {p.target.inLibrary && p.target.id !== item.id && <div className="banner">{p.target.title}{p.target.year ? ` (${p.target.year})` : ''} is already in your library{p.target.folder ? ` at ${p.target.folder}` : ''}. The files go into that item.</div>}
              {p.copies && p.copies.length > 0 && (
                <label className="rmd-copy">Copy or edition
                  <select value={copyId} onChange={(e) => setCopyId(Number(e.target.value))}>
                    <option value={0}>Primary</option>
                    {p.copies.map((c) => <option key={c.id} value={c.id}>{c.name || c.bookType || `Copy ${c.id}`}</option>)}
                  </select>
                </label>
              )}
              <b className="rmd-h">What will change</b>
              <ul className="rmd-plan">
                {!p.target.inLibrary && <li className="rmd-line"><span>{p.target.title}{p.target.year ? ` (${p.target.year})` : ''}</span><span className="rmd-arr" aria-hidden="true">→</span><span>added to the library at <span className="mono">{p.target.folder}</span></span></li>}
                {p.moves.map((m) => (
                  <li key={m.fileId} className="rmd-line">
                    <span className="mono">{m.from}</span><span className="rmd-arr" aria-hidden="true">→</span>
                    <span className="rmd-to">
                      <span className="mono">{m.to}</span>
                      {m.label && <span>{m.label}</span>}
                      {episodeSelect(m)}
                      {m.mode && <span className="muted">{MODE_TEXT[m.mode]} · {fmtBytes(m.bytes)}</span>}
                      {m.sidecars && m.sidecars.length > 0 && <span className="muted">{m.sidecars.length} sidecar file{m.sidecars.length === 1 ? '' : 's'} travel with it</span>}
                      {m.note && <span className="warning-text">{m.note}</span>}
                    </span>
                  </li>
                ))}
                {p.leftOut.map((m) => (
                  <li key={m.fileId} className="rmd-line">
                    <span className="mono">{m.from}</span><span className="rmd-arr" aria-hidden="true">→</span>
                    <span className="rmd-to">
                      <span className="warning-text">{m.problem}</span>
                      {episodeSelect(m)}
                      {!m.needsPick && !choices[m.fileId]?.leave ? null : <span className="muted">Left out: it stays on disk and leaves the library.</span>}
                    </span>
                  </li>
                ))}
                {p.source.id !== p.target.id && <li className="rmd-line"><span>{p.source.title}{p.source.year ? ` (${p.source.year})` : ''} item</span><span className="rmd-arr" aria-hidden="true">→</span><span>{p.sourceReason}</span></li>}
              </ul>
              {p.blocklist.length > 0 && (
                <label className="rmd-check">
                  <input type="checkbox" checked={blocklist ?? p.blocklistOn} onChange={(e) => setBlocklist(e.target.checked)} />
                  Blocklist {p.blocklist.map((b) => b.release).join(', ')} for {p.source.title}
                </label>
              )}
              {p.blocklistNote && <span className="muted">{p.blocklistNote}.</span>}
              {p.cinema && <span className={p.cinema.includes('does not support') ? 'warning-text' : 'muted'}>{p.cinema}.</span>}
              {p.blocked && (
                <div className="banner warning" role="alert">
                  {p.blocked.message}. <button type="button" onClick={() => void preview.refetch()}>Wait for it</button>
                </div>
              )}
              {run.isError && <div className="banner warning" role="alert">{(run.error as Error).message}</div>}
              <div className="rmd-foot">
                <span className="muted">{p.moves.length} file{p.moves.length === 1 ? '' : 's'}{p.leftOut.length ? `, ${p.leftOut.length} left out` : ''}</span>
                <div className="row-actions">
                  <button type="button" onClick={onClose}>Cancel</button>
                  <button type="button" className="btn-accent" disabled={Boolean(p.blocked) || run.isPending || preview.isFetching || (p.moves.length === 0 && p.leftOut.length === 0)}
                    onClick={() => run.mutate(p)}>
                    {run.isPending ? 'Starting…' : file ? 'Move' : p.summary}
                  </button>
                </div>
              </div>
            </>
          )}
        </>
      )}
    </ActionDialog>
  )
}

function RematchProgress({ op, stopping, onStop, stopError, onClose }: { op: RematchOperation; stopping: boolean; onStop: () => void; stopError: Error | null; onClose: () => void }) {
  const running = RUNNING.has(op.state)
  const stoppable = ['planned', 'moving', 'moved'].includes(op.state)
  const r = op.result ?? {}
  const cinema = op.cinema ?? []
  const confirmed = cinema.length > 0 && cinema.every((c) => c.visibleAt)
  let text: string
  switch (op.state) {
    case 'complete': text = `Moved ${r.moved?.length ?? 0} file${r.moved?.length === 1 ? '' : 's'}.`; break
    case 'cancelled': text = 'Stopped. Every file is back where it was.'; break
    case 'failed': text = `Nothing moved: ${op.lastError ?? 'the move failed'}. Every file is back where it was.`; break
    case 'cancel_pending': text = 'Stopping…'; break
    default: text = op.bytesTotal > 0 ? `Copying ${fmtBytes(op.bytesDone)} of ${fmtBytes(op.bytesTotal)}…` : 'Moving…'
  }
  return (
    <div className="rmd-progress">
      <ActionNotice warning={op.state === 'failed'}>{text}</ActionNotice>
      {op.bytesTotal > 0 && running && <progress max={op.bytesTotal} value={op.bytesDone} aria-label="Bytes copied" />}
      {r.failed && r.failed.length > 0 && <p className="warning-text">Not moved, needs review: {r.failed.map((f) => `${f.from} (${f.to})`).join('; ')}</p>}
      {r.leftOut && r.leftOut.length > 0 && <p className="muted">Left out (still on disk, no longer in the library): {r.leftOut.join(', ')}</p>}
      {r.blocklisted && r.blocklisted.length > 0 && <p className="muted">Blocklisted: {r.blocklisted.map((b) => b.release).join(', ')}</p>}
      {r.leftBehind && r.leftBehind.length > 0 && <p className="muted">Left in the old folder: {r.leftBehind.join(', ')}</p>}
      {op.state === 'complete' && cinema.length > 0 && (
        <p className="muted">{confirmed ? 'In cinema.' : 'Moved, cinema confirming…'} {cinema.map((c) => c.result).filter(Boolean).join('; ')}</p>
      )}
      {stopError && <p className="error-text">{stopError.message}</p>}
      <div className="rmd-foot">
        <span className="muted">Operation {op.operationId}</span>
        <div className="row-actions">
          {stoppable && <button type="button" className="btn-danger" disabled={stopping} onClick={onStop}>{stopping ? 'Stopping…' : 'Stop'}</button>}
          <button type="button" className="btn-accent" onClick={onClose}>{running ? 'Close (keeps running)' : 'Done'}</button>
        </div>
      </div>
    </div>
  )
}
