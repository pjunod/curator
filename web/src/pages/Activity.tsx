import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  type ManualImportRequest,
  type QueueItem,
  type ScannedFile,
  blocklistQueueItem,
  fmtBytes,
  fmtRelative,
  getLibrary,
  getLibraryItem,
  getQueue,
  importQueueItem,
  manualImport,
  removeQueueItem,
  scanImportPath,
} from '../api'

const STATE_PILL: Record<string, string> = {
  imported: 'pill-ok',
  failed: 'pill-error',
  awaiting_import: 'pill-warning',
  downloaded: 'pill-neutral',
  downloading: 'pill-neutral',
  importing: 'pill-neutral',
  grabbed: 'pill-neutral',
}

// Friendlier labels for the state pill and the trace steps.
const STATE_LABEL: Record<string, string> = {
  awaiting_import: 'awaiting approval',
}

const STEP_LABEL: Record<string, string> = {
  grabbed: 'Grabbed',
  downloading: 'Downloading',
  downloaded: 'Downloaded',
  awaiting_import: 'Awaiting approval',
  importing: 'Importing',
  imported: 'Imported',
  failed: 'Failed',
}

function label(map: Record<string, string>, key: string): string {
  return map[key] ?? key.replace(/_/g, ' ')
}

function clock(ms: number): string {
  const d = new Date(ms)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function ActivityPage() {
  const qc = useQueryClient()
  const queue = useQuery({ queryKey: ['queue'], queryFn: getQueue, refetchInterval: 4_000 })
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  const [manual, setManual] = useState<Partial<ManualImportRequest> | null>(null)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ['queue'] })
  const remove = useMutation({ mutationFn: (id: number) => removeQueueItem(id, false), onSettled: invalidate })
  const doImport = useMutation({ mutationFn: (id: number) => importQueueItem(id), onSettled: invalidate })
  const doBlocklist = useMutation({ mutationFn: (id: number) => blocklistQueueItem(id), onSettled: invalidate })

  const busy = remove.isPending || doImport.isPending || doBlocklist.isPending
  const toggle = (id: number) =>
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const rows = queue.data ?? []

  return (
    <>
      <header className="page-head">
        <h1>Activity</h1>
        <button
          onClick={() => setManual(manual ? null : {})}
          aria-expanded={!!manual}
        >
          {manual ? 'Close manual import' : 'Manual import'}
        </button>
      </header>

      {manual && <ManualImportPanel prefill={manual} onDone={() => { setManual(null); invalidate() }} />}

      <section className="panel">
        {rows.length === 0 && (
          <p className="muted">Nothing in the queue. Grab something from a title's search.</p>
        )}
        {rows.length > 0 && (
          <table>
            <thead>
              <tr>
                <th style={{ width: 24 }}></th>
                <th>Release</th>
                <th>Quality</th>
                <th>State</th>
                <th>Progress</th>
                <th>Added</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <RowGroup
                  key={d.id}
                  d={d}
                  open={expanded.has(d.id)}
                  busy={busy}
                  onToggle={() => toggle(d.id)}
                  onImport={() => doImport.mutate(d.id)}
                  onBlocklist={() => doBlocklist.mutate(d.id)}
                  onRemove={() => remove.mutate(d.id)}
                  onManual={() =>
                    setManual({
                      path: d.importPath || d.savePath || '',
                      mediaItemId: d.mediaItemId,
                      copyId: d.copyId,
                      downloadId: d.id,
                    })
                  }
                />
              ))}
            </tbody>
          </table>
        )}
      </section>

      <p className="muted" style={{ fontSize: 13 }}>
        The queue refreshes every 30s from the download clients (task{' '}
        <span className="mono">queue.refresh</span>). Expand a row to see the handoff — every step
        from grab to import, and the exact paths Monarr used. Completed downloads import
        automatically unless the client is set to require approval.
      </p>
    </>
  )
}

function RowGroup(props: {
  d: QueueItem
  open: boolean
  busy: boolean
  onToggle: () => void
  onImport: () => void
  onBlocklist: () => void
  onRemove: () => void
  onManual: () => void
}) {
  const { d, open, busy } = props
  const canImport = d.state === 'awaiting_import' || d.state === 'downloaded'
  const canRetry = d.state === 'failed'
  const canBlocklist = d.state !== 'imported'

  return (
    <>
      <tr>
        <td>
          <button
            className="row-toggle"
            aria-label={open ? 'Collapse handoff' : 'Show handoff'}
            aria-expanded={open}
            onClick={props.onToggle}
          >
            {open ? '▾' : '▸'}
          </button>
        </td>
        <td className="mono">
          {d.title}
          {d.error && <div className="error-text">{d.error}</div>}
        </td>
        <td className="muted">{d.quality}</td>
        <td>
          <span className={`pill ${STATE_PILL[d.state] ?? 'pill-neutral'}`}>
            {label(STATE_LABEL, d.state)}
          </span>
        </td>
        <td style={{ minWidth: 120 }}>
          <div className="progress-track">
            <i style={{ width: `${Math.round(d.progress * 100)}%` }} />
          </div>
        </td>
        <td className="muted">{fmtRelative(d.addedAt)}</td>
        <td>
          <div className="row-actions">
            {canImport && (
              <button onClick={props.onImport} disabled={busy}>
                Import now
              </button>
            )}
            {canRetry && (
              <>
                <button onClick={props.onImport} disabled={busy}>
                  Retry
                </button>
                <button onClick={props.onManual} disabled={busy}>
                  Manual import
                </button>
              </>
            )}
            {canBlocklist && (
              <button className="btn-danger" onClick={props.onBlocklist} disabled={busy}>
                Blocklist
              </button>
            )}
            <button onClick={props.onRemove} disabled={busy}>
              Remove
            </button>
          </div>
        </td>
      </tr>
      {open && (
        <tr className="handoff-row">
          <td></td>
          <td colSpan={6}>
            <HandoffDetail d={d} />
          </td>
        </tr>
      )}
    </>
  )
}

function HandoffDetail({ d }: { d: QueueItem }) {
  const steps = d.handoff ?? []
  return (
    <div className="handoff">
      {(d.savePath || d.importPath) && (
        <div className="handoff-paths">
          {d.savePath && (
            <div>
              <span className="muted">Client reported</span>{' '}
              <span className="path-chip">{d.savePath}</span>
            </div>
          )}
          {d.importPath && d.importPath !== d.savePath && (
            <div>
              <span className="muted">Monarr looked in</span>{' '}
              <span className="path-chip">{d.importPath}</span>
            </div>
          )}
        </div>
      )}
      {steps.length === 0 ? (
        <p className="muted">No handoff steps recorded yet.</p>
      ) : (
        <ol className="handoff-trace">
          {steps.map((h, i) => (
            <li key={i} className="handoff-step" data-step={h.step}>
              <span className="handoff-dot" />
              <span className="handoff-when mono">{clock(h.at)}</span>
              <span className="handoff-what">
                <b>{label(STEP_LABEL, h.step)}</b>
                {h.detail ? ` — ${h.detail}` : ''}
              </span>
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

function ManualImportPanel({
  prefill,
  onDone,
}: {
  prefill: Partial<ManualImportRequest>
  onDone: () => void
}) {
  const [path, setPath] = useState(prefill.path ?? '')
  const [itemId, setItemId] = useState<number>(prefill.mediaItemId ?? 0)
  const [copyId, setCopyId] = useState<number>(prefill.copyId ?? 0)
  const [scanned, setScanned] = useState<ScannedFile[] | null>(null)
  const [scanErr, setScanErr] = useState<string>('')
  const [importErr, setImportErr] = useState<string>('')
  const [done, setDone] = useState<string>('')

  const library = useQuery({ queryKey: ['library'], queryFn: () => getLibrary() })
  const itemDetail = useQuery({
    queryKey: ['library', itemId],
    queryFn: () => getLibraryItem(itemId),
    enabled: itemId > 0,
  })

  const scan = useMutation({
    mutationFn: () => scanImportPath(path),
    onSuccess: (files) => {
      setScanned(files)
      setScanErr('')
    },
    onError: (e: Error) => {
      setScanned(null)
      setScanErr(e.message)
    },
  })

  const run = useMutation({
    mutationFn: () =>
      manualImport({
        path,
        mediaItemId: itemId,
        copyId: copyId || undefined,
        downloadId: prefill.downloadId,
      }),
    onSuccess: (res) => {
      setImportErr('')
      setDone(`Imported ${res.files} file(s).`)
      setTimeout(onDone, 1200)
    },
    onError: (e: Error) => setImportErr(e.message),
  })

  const copies = itemDetail.data?.copies ?? []

  return (
    <section className="panel manual-import">
      <h2>Manual import</h2>
      <p className="muted" style={{ fontSize: 13 }}>
        Point Monarr at a folder or file, choose where it belongs, and import it — for a download
        Monarr couldn't place automatically, or files you moved into place yourself.
      </p>

      <div className="form-row">
        <label>
          Path
          <input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="/pool/downloads/Some.Release.2024.1080p"
            style={{ minWidth: 360 }}
          />
        </label>
        <button onClick={() => scan.mutate()} disabled={!path || scan.isPending}>
          Scan
        </button>
      </div>
      {scanErr && <p className="error-text">{scanErr}</p>}

      {scanned && (
        <div className="scan-result">
          {scanned.length === 0 ? (
            <p className="error-text">No media files found there. Check the path is reachable from Monarr's container.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>File</th>
                  <th>Kind</th>
                  <th>Quality</th>
                  <th>Episode</th>
                  <th>Size</th>
                </tr>
              </thead>
              <tbody>
                {scanned.map((f) => (
                  <tr key={f.path}>
                    <td className="mono">{f.name}</td>
                    <td className="muted">{f.kind}</td>
                    <td className="muted">{f.quality || '—'}</td>
                    <td className="muted">
                      {f.season >= 0 && f.episodes.length > 0
                        ? `S${String(f.season).padStart(2, '0')}E${f.episodes
                            .map((e) => String(e).padStart(2, '0'))
                            .join('E')}`
                        : '—'}
                    </td>
                    <td className="muted">{fmtBytes(f.size)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}

      <div className="form-row">
        <label>
          Import into
          <select value={itemId} onChange={(e) => { setItemId(Number(e.target.value)); setCopyId(0) }}>
            <option value={0}>Choose a title…</option>
            {(library.data ?? []).map((m) => (
              <option key={m.id} value={m.id}>
                {m.title}
                {m.year ? ` (${m.year})` : ''}
              </option>
            ))}
          </select>
        </label>
        {copies.length > 0 && (
          <label>
            Copy
            <select value={copyId} onChange={(e) => setCopyId(Number(e.target.value))}>
              <option value={0}>Primary</option>
              {copies.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name || `copy ${c.id}`}
                </option>
              ))}
            </select>
          </label>
        )}
        <button
          className="btn-accent"
          onClick={() => run.mutate()}
          disabled={!path || !itemId || run.isPending}
        >
          Import
        </button>
      </div>
      {importErr && <p className="error-text">{importErr}</p>}
      {done && <p className="ok-text">{done}</p>}
    </section>
  )
}
