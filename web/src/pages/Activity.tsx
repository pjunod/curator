import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  type ManualImportRequest,
  type QueueItem,
  type ScannedFile,
  type Stage,
  blocklistQueueItem,
  cancelQueueImport,
  clearFailedQueue,
  clearFinishedQueue,
  fmtBytes,
  fmtRelative,
  getLibrary,
  getLibraryItem,
  getManualImportDefaultPath,
  getQueuePage,
  getQueueSummary,
  importQueueItem,
  manualImport,
  removeQueueItem,
  scanImportPath,
} from '../api'
import { PathInput } from '../PathInput'

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
  import_queued: 'Queued for import',
  importing: 'Importing',
  import_cancelled: 'Import cancelled',
  import_recovered: 'Import recovered',
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
  importing: 'Curator is copying the files into your library. This is the one stage Curator does itself',
  notifying: 'Curator is telling the media server an import landed',
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
  const queued = rows.filter((r) => r.importState === 'queued')
  for (const stage of STAGES) {
    if (stage === 'importing' && queued.length > 0) {
      queued.forEach((r) => live.add(r.id))
      out.push({
        key: 'import-queued',
        label: 'Waiting to copy',
        title: 'Accepted by Curator and waiting for an available import worker',
        rows: queued,
      })
    }
    const inStage = rows.filter(
      (r) => !live.has(r.id) && r.importState !== 'queued' && r.stage === stage,
    )
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
  if (d.importState === 'queued') {
    return <span className="muted">Waiting for an import worker</span>
  }
  if (d.importState === 'running' && !d.stage) {
    return <span className="muted">Starting importer…</span>
  }
  if (d.state === 'importing' && !d.stage && !d.importState) {
    return <span className="error-text">No importer is running</span>
  }
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

// How many terminal rows one "show more" adds. Small on purpose: the point
// of this page is what is happening, and the history behind it is opened
// deliberately, a screenful at a time.
const PAGE = 25

// The groups, in the order they matter. Active is never collapsed and never
// paged — it is the answer to "what is happening", and an answer that is
// partly hidden is not one. Finished and Failed are history: collapsed to a
// count, opened when asked, and paged from there.
const GROUPS = [
  { key: 'imported' as const, label: 'Finished' },
  { key: 'failed' as const, label: 'Failed' },
]

export function ActivityPage() {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  const [manual, setManual] = useState<Partial<ManualImportRequest> | null>(null)
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const [shown, setShown] = useState<Record<string, number>>({ imported: PAGE, failed: PAGE })
  const [confirmClear, setConfirmClear] = useState<'imported' | 'failed' | null>(null)
  const [actionErr, setActionErr] = useState('')

  // Only the active list polls. Finished and Failed do not change on their
  // own — something has to finish or fail first, and that moves the counts,
  // which is what tells this page to look again.
  const summary = useQuery({
    queryKey: ['queue-summary'],
    queryFn: getQueueSummary,
    refetchInterval: 4_000,
  })
  const active = useQuery({
    queryKey: ['queue', 'active', search],
    queryFn: () => getQueuePage('active', search, 200),
    refetchInterval: 4_000,
  })
  const finished = useQuery({
    queryKey: ['queue', 'imported', search, shown.imported],
    queryFn: () => getQueuePage('imported', search, shown.imported),
    enabled: !!open.imported,
  })
  const failed = useQuery({
    queryKey: ['queue', 'failed', search, shown.failed],
    queryFn: () => getQueuePage('failed', search, shown.failed),
    enabled: !!open.failed,
  })

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['queue'] })
    void qc.invalidateQueries({ queryKey: ['queue-summary'] })
  }
  const remove = useMutation({ mutationFn: (id: number) => removeQueueItem(id, false), onSettled: invalidate })
  const doImport = useMutation({
    mutationFn: (id: number) => importQueueItem(id),
    onMutate: () => setActionErr(''),
    onError: (e: Error) => setActionErr(e.message),
    onSettled: invalidate,
  })
  const cancelImport = useMutation({
    mutationFn: (id: number) => cancelQueueImport(id),
    onMutate: () => setActionErr(''),
    onError: (e: Error) => setActionErr(e.message),
    onSettled: invalidate,
  })
  const doBlocklist = useMutation({ mutationFn: (id: number) => blocklistQueueItem(id), onSettled: invalidate })
  const clearFinished = useMutation({
    mutationFn: clearFinishedQueue,
    onSettled: () => {
      setConfirmClear(null)
      invalidate()
    },
  })
  const clearFailed = useMutation({
    mutationFn: clearFailedQueue,
    onSettled: () => {
      setConfirmClear(null)
      invalidate()
    },
  })

  const busy =
    remove.isPending || doImport.isPending || cancelImport.isPending || doBlocklist.isPending ||
    clearFinished.isPending || clearFailed.isPending
  const toggle = (id: number) =>
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const counts = summary.data?.counts ?? {}
  const activeRows = active.data ?? []
  const groupRows: Record<string, QueueItem[]> = {
    imported: finished.data ?? [],
    failed: failed.data ?? [],
  }
  const retention = summary.data?.retentionDays
  const nothingAtAll = (summary.data?.total ?? 0) === 0

  const rowProps = (d: QueueItem) => ({
    d,
    open: expanded.has(d.id),
    busy,
    onToggle: () => toggle(d.id),
    onImport: () => doImport.mutate(d.id),
    onCancelImport: () => cancelImport.mutate(d.id),
    onBlocklist: () => doBlocklist.mutate(d.id),
    onRemove: () => remove.mutate(d.id),
    onManual: () =>
      setManual({
        path: d.importPath || d.savePath || '',
        mediaItemId: d.mediaItemId,
        copyId: d.copyId,
        downloadId: d.id,
      }),
  })

  const columns = (
    <>
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
    </>
  )

  return (
    <>
      <header className="page-head">
        <h1>Activity</h1>
        <button onClick={() => setManual(manual ? null : {})} aria-expanded={!!manual}>
          {manual ? 'Close manual import' : 'Manual import'}
        </button>
      </header>

      {manual && <ManualImportPanel prefill={manual} onDone={() => { setManual(null); invalidate() }} />}

      {actionErr && <div className="banner warning">{actionErr}</div>}

      <div className="activity-toolbar">
        <input
          type="search"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Filter by release title"
          aria-label="Filter by release title"
        />
        <span className="muted" data-testid="activity-counts">
          {summary.data?.active ?? 0} active · {counts.imported ?? 0} finished ·{' '}
          {counts.failed ?? 0} failed
        </span>
      </div>

      {/* ACTIVE — always whole, never paged. */}
      <section className="panel">
        {activeRows.length === 0 && (
          <p className="muted">
            {nothingAtAll
              ? 'Nothing in the queue. Grab something from a title\'s search.'
              : search
                ? 'Nothing moving matches that.'
                : 'Nothing moving right now.'}
          </p>
        )}
        {activeRows.length > 0 && (
          <table className="queue-table">
            {columns}
            {sections(activeRows).map((section) => (
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
                  <RowGroup key={d.id} {...rowProps(d)} />
                ))}
              </tbody>
            ))}
          </table>
        )}
      </section>

      {/* FINISHED and FAILED — collapsed to a count. This is the part that
          used to grow without limit: every receipt Curator ever wrote, in one
          table, forever. */}
      {GROUPS.map((group) => {
        const total = counts[group.key] ?? 0
        const rows = groupRows[group.key]
        const isOpen = !!open[group.key]
        const action = group.key === 'imported' ? clearFinished : clearFailed
        const actionLabel = group.key === 'imported' ? 'finished' : 'failed'
        return (
          <section className="panel activity-group" key={group.key}>
            <div className="activity-group-head">
              <button
                className="link-button"
                aria-expanded={isOpen}
                data-testid={`toggle-${group.key}`}
                onClick={() => setOpen((p) => ({ ...p, [group.key]: !p[group.key] }))}
              >
                {isOpen ? '▾' : '▸'} {group.label}
                <span className="section-count">{total}</span>
              </button>
              {total > 0 && (
                confirmClear === group.key ? (
                  <span className="confirm-inline">
                    Clear {total} {actionLabel} row{total === 1 ? '' : 's'}?
                    <button disabled={busy} onClick={() => action.mutate()}>
                      Yes, clear
                    </button>
                    <button className="link-button" onClick={() => setConfirmClear(null)}>
                      Cancel
                    </button>
                  </span>
                ) : (
                  <button
                    className="link-button"
                    data-testid={`clear-${actionLabel}`}
                    onClick={() => setConfirmClear(group.key)}
                  >
                    Clear {actionLabel}
                  </button>
                )
              )}
            </div>
            {isOpen && (
              <>
                {rows.length === 0 && (
                  <p className="muted">{search ? 'Nothing here matches that.' : 'Nothing here.'}</p>
                )}
                {rows.length > 0 && (
                  <table className="queue-table">
                    {columns}
                    <tbody>
                      {rows.map((d) => (
                        <RowGroup key={d.id} {...rowProps(d)} />
                      ))}
                    </tbody>
                  </table>
                )}
                {rows.length >= (shown[group.key] ?? PAGE) && (
                  <button
                    className="link-button"
                    onClick={() =>
                      setShown((p) => ({ ...p, [group.key]: (p[group.key] ?? PAGE) + PAGE }))
                    }
                  >
                    Show {PAGE} more
                  </button>
                )}
              </>
            )}
          </section>
        )
      })}

      <p className="muted" style={{ fontSize: 13 }}>
        Grouped by what each job is doing right now. Stages up to and including{' '}
        <span className="mono">Moving</span> happen inside the download client;{' '}
        <span className="mono">Copying to library</span> is the one Curator does itself. Clients that
        push report a change within a second; the rest are reconciled by the 30s sweep (task{' '}
        <span className="mono">queue.refresh</span>). Expand a row for the handoff — every step from
        grab to import, and the exact paths Curator used.
        {typeof retention === 'number' && (
          <>
            {' '}
            Finished and failed rows are kept for{' '}
            {retention === 0 ? 'ever' : `${retention} days`} (Settings → Library).
          </>
        )}
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
  onCancelImport: () => void
  onBlocklist: () => void
  onRemove: () => void
  onManual: () => void
}) {
  const { d, open, busy } = props
  const queuedImport = d.importState === 'queued'
  const startingImport = d.importState === 'running' && !d.stage
  const canImport = (d.state === 'awaiting_import' || d.state === 'downloaded') && !d.importState
  const canRetry = d.state === 'failed'
  const interruptedImport = d.state === 'importing' && !d.stage && !d.importState
  const activeImport = d.stage === 'importing'
  const importerOwned = queuedImport || startingImport || activeImport
  const canBlocklist = d.state !== 'imported' && !importerOwned

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
          {/* One line, elided: the release column is what is left after the
              fixed columns, and a four-line match sentence under every title
              turned each row into a paragraph. The full reason is the title. */}
          {d.match?.reason && <div className="muted queue-match" title={d.match.reason}>Match: {d.match.reason}</div>}
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
            APPLICATION is doing this. Curator's own name appears exactly once,
            on the one stage Curator performs itself.
          */}
          {queuedImport ? (
            <span className="pill pill-neutral">queued</span>
          ) : startingImport ? (
            <span className="muted stage-owner">Curator</span>
          ) : interruptedImport ? (
            <span className="pill pill-warning">interrupted</span>
          ) : d.stage ? (
            <span className="muted stage-owner">{d.stagePeer || 'Curator'}</span>
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
            {interruptedImport && (
              <button onClick={props.onImport} disabled={busy}>
                Restart import
              </button>
            )}
            {(importerOwned || interruptedImport) && (
              <button className="btn-danger" onClick={props.onCancelImport} disabled={busy}>
                Cancel import
              </button>
            )}
            {d.error?.includes('no library folder assigned') && (
              <Link
                to="/library/$id"
                params={{ id: String(d.mediaItemId) }}
                search={{ assignFolder: true }}
              >
                Assign folder
              </Link>
            )}
            {canBlocklist && (
              <button className="btn-danger" onClick={props.onBlocklist} disabled={busy}>
                Blocklist
              </button>
            )}
            {!importerOwned && !interruptedImport && (
              <button onClick={props.onRemove} disabled={busy}>
                Remove
              </button>
            )}
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
              <span className="muted">Curator looked in</span>{' '}
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
  // The API learns the completed-download folder from path mappings or recent
  // rows. Queue-row manual imports still win with their exact recorded path.
  const [path, setPath] = useState(prefill.path ?? '')
  const [itemId, setItemId] = useState<number>(prefill.mediaItemId ?? 0)
  const [copyId, setCopyId] = useState<number>(prefill.copyId ?? 0)
  const [scanned, setScanned] = useState<ScannedFile[] | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [scanErr, setScanErr] = useState<string>('')
  const [importErr, setImportErr] = useState<string>('')

  const library = useQuery({ queryKey: ['library'], queryFn: () => getLibrary() })
  const defaultPath = useQuery({
    queryKey: ['manual-import-default-path'],
    queryFn: getManualImportDefaultPath,
    enabled: !prefill.path,
  })
  const shownPath = path || defaultPath.data?.path || ''
  const itemDetail = useQuery({
    queryKey: ['library', itemId],
    queryFn: () => getLibraryItem(itemId),
    enabled: itemId > 0,
  })

  const scan = useMutation({
    mutationFn: () => scanImportPath(shownPath),
    onSuccess: (files) => {
      setScanned(files)
      // A single-file result is unambiguous. A directory with several files
      // needs an explicit choice so one title cannot accidentally swallow a
      // neighbour from the completed folder.
      setSelected(new Set(files.length === 1 ? [files[0].path] : []))
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
        path: shownPath,
        paths: [...selected],
        mediaItemId: itemId,
        copyId: copyId || undefined,
        downloadId: prefill.downloadId,
      }),
    onSuccess: () => {
      setImportErr('')
      // The importer owns the rest. Close immediately so the accepted job is
      // visible in Activity and this form never becomes a progress modal for
      // a multi-gigabyte copy.
      onDone()
    },
    onError: (e: Error) => {
      setImportErr(e.message)
    },
  })

  const copies = itemDetail.data?.copies ?? []

  return (
    <section className="panel manual-import">
      <h2>Manual import</h2>
      <p className="muted" style={{ fontSize: 13 }}>
        Point Curator at a folder or file, choose where it belongs, and import it — for a download
        Curator couldn't place automatically, or files you moved into place yourself.
      </p>

      <div className="form-row">
        <label htmlFor="manual-import-path">Path</label>
        <PathInput
          id="manual-import-path"
          value={shownPath}
          onChange={(next) => {
            setPath(next)
            setScanned(null)
            setSelected(new Set())
          }}
          placeholder="/pool/downloads/Some.Release.2024.1080p"
          showParent
        />
        <button onClick={() => scan.mutate()} disabled={!shownPath || scan.isPending}>
          {scan.isPending ? 'Scanning…' : 'Scan'}
        </button>
      </div>
      <p className="muted manual-import-path-help">
        Starts in Curator&apos;s completed-download folder. Type another absolute path or choose a
        folder suggestion; use Parent folder to navigate back up.
      </p>
      {scanErr && <p className="error-text">{scanErr}</p>}

      {scanned && (
        <div className="scan-result">
          {scanned.length === 0 ? (
            <p className="error-text">No media files found there. Check the path is reachable from Curator's container.</p>
          ) : (
            <div className="log-scroll">
            <table>
              <thead>
                <tr>
                  <th className="scan-select">
                    <input
                      type="checkbox"
                      aria-label="Select all files"
                      checked={scanned.length > 0 && selected.size === scanned.length}
                      onChange={(e) =>
                        setSelected(e.target.checked ? new Set(scanned.map((f) => f.path)) : new Set())
                      }
                    />
                  </th>
                  <th>File</th>
                  <th>Kind</th>
                  <th>Quality</th>
                  <th>Episode</th>
                  <th>Size</th>
                </tr>
              </thead>
              <tbody>
                {scanned.map((f) => (
                  <tr
                    key={f.path}
                    className={selected.has(f.path) ? 'scan-selected' : undefined}
                    onClick={() =>
                      setSelected((prev) => {
                        const next = new Set(prev)
                        if (next.has(f.path)) next.delete(f.path)
                        else next.add(f.path)
                        return next
                      })
                    }
                  >
                    <td className="scan-select">
                      <input
                        type="checkbox"
                        aria-label={`Select ${f.name}`}
                        checked={selected.has(f.path)}
                        onChange={() =>
                          setSelected((prev) => {
                            const next = new Set(prev)
                            if (next.has(f.path)) next.delete(f.path)
                            else next.add(f.path)
                            return next
                          })
                        }
                        onClick={(e) => e.stopPropagation()}
                      />
                    </td>
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
          disabled={!shownPath || !itemId || selected.size === 0 || run.isPending}
        >
          {run.isPending ? 'Queueing…' : `Queue selected (${selected.size})`}
        </button>
      </div>
      {scanned && scanned.length > 1 && selected.size === 0 && (
        <p className="muted">Select one or more files above to import.</p>
      )}
      {selected.size > 0 && !itemId && (
        <p className="muted">Choose the library title these selected files belong to.</p>
      )}
      {importErr && <p className="error-text">{importErr}</p>}
    </section>
  )
}
