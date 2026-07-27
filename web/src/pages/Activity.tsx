import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  type ImportOutcome,
  type ManualImportRequest,
  type QueueItem,
  type ScannedFile,
  type Stage,
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

// One job, one row, and the stage word changes as it moves.
//
// Fetching, repairing, extracting and copying into the library are parts of
// one job, not different kinds of work that belong on different screens. They
// are separate SECTIONS because lumping them into one pile answers "how many
// things are busy" and not "what is this one doing" — and the second question
// is the one anybody actually opens this page with.
//
// The keys are the download client's own stage names. Translating them here,
// at the edge, is deliberate: nothing between nzbd and this line has to agree
// on English.
const STAGE_LABEL: Record<Stage, string> = {
  downloading: 'Downloading',
  par_rename: 'Renaming',
  par_verify: 'Verifying',
  par_repair: 'Repairing',
  rar_rename: 'Naming archives',
  unpack: 'Extracting',
  cleanup: 'Cleaning up',
  move: 'Moving',
  post_unpack_rename: 'Renaming',
  script: 'Running scripts',
  importing: 'Copying to library',
  notifying: 'Notifying',
}

const STAGE_TITLE: Record<Stage, string> = {
  downloading: 'The download client is fetching articles from the newsgroup server',
  par_rename: 'The download client is restoring real filenames from the par2 set',
  par_verify: 'The download client is checking the payload against its par2 blocks',
  par_repair: 'The download client is rebuilding damaged blocks. This can take a while on a big release',
  rar_rename: 'The download client is putting the archive volumes back in order',
  unpack: 'The download client is extracting the archive',
  cleanup: 'The download client is deleting the archive volumes it no longer needs',
  move: 'The download client is moving the payload to its finished folder',
  post_unpack_rename: 'The download client is tidying names after extraction',
  script: 'The download client is running its post-processing scripts',
  importing: 'Monarr is copying the files into your library. This is the one stage Monarr does itself',
  notifying: 'Monarr is telling the media server an import landed',
}

// Sections, in the order a job passes through them. Rendered only when
// occupied, so an idle instance shows nothing rather than eleven empty
// headings.
const STAGES: Stage[] = [
  'downloading',
  'par_rename',
  'par_verify',
  'par_repair',
  'rar_rename',
  'unpack',
  'cleanup',
  'move',
  'post_unpack_rename',
  'script',
  'importing',
  'notifying',
]

// Rows with nothing in flight still have to go somewhere, grouped by what a
// person would do about them: one needs a decision, one is finished, one went
// wrong.
const RESTING = [
  { key: 'waiting', label: 'Waiting', states: ['grabbed', 'downloaded', 'awaiting_import'] },
  { key: 'done', label: 'Finished', states: ['imported'] },
  { key: 'failed', label: 'Failed', states: ['failed'] },
] as const

type Section = { key: string; label: string; title?: string; rows: QueueItem[] }

function sections(rows: QueueItem[]): Section[] {
  const out: Section[] = []
  const live = new Set<number>()
  for (const stage of STAGES) {
    const inStage = rows.filter((r) => r.stage === stage)
    inStage.forEach((r) => live.add(r.id))
    if (inStage.length > 0) {
      out.push({ key: stage, label: STAGE_LABEL[stage], title: STAGE_TITLE[stage], rows: inStage })
    }
  }
  for (const group of RESTING) {
    const inGroup = rows.filter((r) => !live.has(r.id) && group.states.includes(r.state as never))
    if (inGroup.length > 0) out.push({ key: group.key, label: group.label, rows: inGroup })
  }
  // Anything in a state this build has not heard of still gets shown. A row
  // that silently vanishes because of an unrecognised word is worse than an
  // ugly heading.
  const placed = new Set(out.flatMap((s) => s.rows.map((r) => r.id)))
  const rest = rows.filter((r) => !placed.has(r.id))
  if (rest.length > 0) out.push({ key: 'other', label: 'Other', rows: rest })
  return out
}

// Progress the row can actually stand behind.
//
// A stage that cannot measure itself gets no bar at all, rather than one
// sitting at zero: an empty bar is the claim that nothing has happened, which
// is a different and usually false statement about a job that is midway
// through repairing a 60 GB archive.
function StageProgress({ d }: { d: QueueItem }) {
  const known = typeof d.total === 'number' && d.total > 0
  const done = d.bytes ?? 0
  const fraction = known ? Math.min(1, done / (d.total as number)) : d.stage ? null : d.progress

  return (
    <div className="stage-progress">
      {fraction !== null && (
        <div className="progress-track">
          <i style={{ width: `${Math.round(fraction * 100)}%` }} />
        </div>
      )}
      {known ? (
        <div className="muted stage-bytes">
          {fmtBytes(done)} / {fmtBytes(d.total as number)}
          {d.bytesPerSecond ? ` · ${fmtBytes(d.bytesPerSecond)}/s` : ''}
        </div>
      ) : d.stage ? (
        // Words, not digits — so this one wraps. Forcing the numeric
        // treatment on it pushed "post-processing: par repair · 4m ago"
        // straight out of the column and under the next one.
        <div className="muted stage-note">
          {d.stageDetail || 'in progress'}
          {d.stageSince ? ` · ${fmtRelative(d.stageSince)}` : ''}
        </div>
      ) : null}
    </div>
  )
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
          <table className="queue-table">
            {/* An explicit column layout. Without one the release title takes
                whatever width it likes and squeezes the actions column until
                every button wraps onto its own line — which is what made these
                rows six lines tall. */}
            <colgroup>
              <col className="col-toggle" />
              <col className="col-release" />
              <col className="col-quality" />
              <col className="col-state" />
              <col className="col-progress" />
              <col className="col-added" />
              <col className="col-actions" />
            </colgroup>
            <thead>
              <tr>
                <th></th>
                <th>Release</th>
                <th>Quality</th>
                <th>Doing it</th>
                <th>Progress</th>
                <th>Added</th>
                <th>Actions</th>
              </tr>
            </thead>
            {sections(rows).map((section) => (
              <tbody key={section.key}>
                <tr className="section-row">
                  <td colSpan={7}>
                    <span className="section-label" title={section.title}>
                      {section.label}
                    </span>
                    <span className="section-count">{section.rows.length}</span>
                  </td>
                </tr>
                {section.rows.map((d) => (
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
            ))}
          </table>
        )}
      </section>

      <p className="muted" style={{ fontSize: 13 }}>
        Grouped by what each job is doing right now. Stages up to and including{' '}
        <span className="mono">Moving</span> happen inside the download client;{' '}
        <span className="mono">Copying to library</span> is the one Monarr does itself. Clients that
        push report a change within a second; the rest are reconciled by the 30s sweep (task{' '}
        <span className="mono">queue.refresh</span>). Expand a row for the handoff — every step from
        grab to import, and the exact paths Monarr used.
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
          {/*
            While a job is in flight the section heading already says which
            stage it is at, so repeating it here is noise — and worse than
            noise: a row sitting under "Repairing" showed a "downloading" pill,
            because `state` is the coarse lifecycle and cannot tell the two
            apart. That is the same wrong claim the backend was making, left
            on the screen.

            So a moving row answers the question the section does not: WHICH
            APPLICATION is doing this. Monarr's own name appears exactly once,
            on the one stage Monarr performs itself.
          */}
          {d.stage ? (
            <span className="muted stage-owner">{d.stagePeer || 'Monarr'}</span>
          ) : (
            <span className={`pill ${STATE_PILL[d.state] ?? 'pill-neutral'}`}>
              {label(STATE_LABEL, d.state)}
            </span>
          )}
        </td>
        <td>
          <StageProgress d={d} />
        </td>
        <td className="muted nowrap" title={d.addedAt}>
          {fmtRelative(d.addedAt)}
        </td>
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
  const [outcome, setOutcome] = useState<ImportOutcome | null>(null)

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
      setOutcome(res)
      const skipped = res.files.filter((f) => !f.imported)
      setDone(
        skipped.length
          ? `Imported ${res.imported} file(s); ${skipped.length} skipped.`
          : `Imported ${res.imported} file(s).`,
      )
      // A clean import can close itself. One that skipped something must not:
      // the reasons are the reason the panel is still open.
      if (!skipped.length) setTimeout(onDone, 1200)
    },
    onError: (e: Error) => {
      setOutcome(null)
      setImportErr(e.message)
    },
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
            <div className="log-scroll">
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
            </div>
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
      {/* Per-file outcomes. An import that declines everything used to say
          only "no files imported from <path>", which is true and useless —
          the reasons existed, they just never reached the person who needed
          them. */}
      {outcome && outcome.files.some((f) => !f.imported) && (
        <div className="log-scroll">
          <table className="import-outcome">
          <thead>
            <tr>
              <th>File</th>
              <th>Result</th>
              <th>Why</th>
            </tr>
          </thead>
          <tbody>
            {outcome.files.map((f) => (
              <tr key={f.name}>
                <td className="mono">{f.name}</td>
                <td>
                  {f.imported ? (
                    <span className="pill pill-ok">imported{f.upgrade ? ' · upgrade' : ''}</span>
                  ) : (
                    <span className="pill pill-neutral">skipped</span>
                  )}
                </td>
                <td className="muted">{f.reason || '—'}</td>
              </tr>
            ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
