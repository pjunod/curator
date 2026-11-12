import { useMemo, useState } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { ActionDialog } from './ActionDialog'
import {
  browseFilesystem,
  cancelQueueImport,
  fmtBytes,
  getManualImportDefaultPath,
  getQueueItem,
  getRootFolders,
  manualImport,
  placePublicationFiles,
  planImport,
  scanImportFolder,
  type ImportPlanFile,
  type ImportPlanMode,
  type MediaItemDetail,
  type QueueItem,
  type ScannedFile,
} from './api'
import './ImportFileDialog.css'

/** A series file's episode choice, as "season:e1,e2". */
type Choice = string

const episodeLabel = (season: number, episodes: number[]) =>
  episodes.length === 0
    ? ''
    : `S${String(season).padStart(2, '0')}E${String(episodes[0]).padStart(2, '0')}` +
      (episodes.length > 1 ? `-E${String(episodes[episodes.length - 1]).padStart(2, '0')}` : '')

const choiceOf = (season: number, episodes: number[]): Choice => (episodes.length ? `${season}:${episodes.join(',')}` : '')
function parseChoice(choice: Choice): { season: number; episodes: number[] } | null {
  if (!choice) return null
  const [season, eps] = choice.split(':')
  return { season: Number(season), episodes: eps.split(',').map(Number) }
}

const MODE_TEXT: Record<ImportPlanMode, string> = {
  runner: 'placed from the runner delivery; curator cleans up the original under its usual rules',
  hardlink: 'hardlinked — your copy stays',
  copy: 'copied across volumes — your copy stays',
  rename: 'renamed into the library',
  copy_remove: 'copied, verified, then the original is removed',
}

/** Results of a submitted import: one Activity job per folder or runner file. */
interface Started { downloadId?: number; label: string; error?: string }

/**
 * Import file… from the item page: browse a folder, pick files, confirm one
 * plan. Runner deliveries go through Resolve's placement; loose files through
 * the queued manual import. The target item is fixed: it is this page's.
 */
export function ImportFileDialog({ item, onClose }: { item: MediaItemDetail; onClose: () => void }) {
  const qc = useQueryClient()
  const [step, setStep] = useState<'choose' | 'confirm' | 'started'>('choose')
  const [path, setPath] = useState('')
  const [draft, setDraft] = useState('')
  const [selected, setSelected] = useState<Record<string, ScannedFile>>({})
  const [choices, setChoices] = useState<Record<string, Choice>>({})
  const [copyId, setCopyId] = useState(0)
  const [removeOriginal, setRemoveOriginal] = useState(false)
  const [started, setStarted] = useState<Started[]>([])

  const defaultPath = useQuery({ queryKey: ['import-default-path'], queryFn: getManualImportDefaultPath })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const current = path || defaultPath.data?.path || ''
  const dirs = useQuery({
    queryKey: ['filesystem', current.endsWith('/') ? current : current + '/'],
    queryFn: () => browseFilesystem(current.endsWith('/') ? current : current + '/'),
    enabled: current !== '',
    retry: false,
  })
  const files = useQuery({
    queryKey: ['import-folder', current],
    queryFn: () => scanImportFolder(current),
    enabled: current !== '',
    retry: false,
  })

  const go = (next: string) => {
    setPath(next.length > 1 ? next.replace(/\/+$/, '') : next)
    setDraft('')
  }
  const crumbs = useMemo(() => {
    const parts = current.split('/').filter(Boolean)
    return parts.map((name, i) => ({ name, path: '/' + parts.slice(0, i + 1).join('/') }))
  }, [current])

  const chosen = Object.values(selected)
  const toggle = (f: ScannedFile) =>
    setSelected((prev) => {
      const next = { ...prev }
      if (next[f.path]) delete next[f.path]
      else next[f.path] = f
      return next
    })

  const episodeOptions = useMemo(
    () =>
      item.seasons.flatMap((s) =>
        s.episodes.map((e) => ({
          value: choiceOf(s.number, [e.episodeNumber]),
          label: `${episodeLabel(s.number, [e.episodeNumber])}${e.title ? ` · ${e.title}` : ''}`,
        })),
      ),
    [item.seasons],
  )
  const choiceFor = (f: ScannedFile): Choice =>
    choices[f.path] ?? (item.kind === 'series' ? choiceOf(f.season, f.episodes) : '')

  const planRequest = {
    mediaItemId: item.id,
    copyId: copyId || undefined,
    removeOriginal,
    files: chosen.map((f) => {
      const c = parseChoice(choiceFor(f))
      return { path: f.path, season: c?.season ?? -1, episodes: c?.episodes ?? [] }
    }),
  }
  const plan = useQuery({
    queryKey: ['import-plan', planRequest],
    queryFn: () => planImport(planRequest),
    enabled: step === 'confirm' && chosen.length > 0,
    retry: false,
  })
  const blocked = !plan.data || plan.data.some((line) => line.error)
  const loose = chosen.filter((f) => !f.publication)

  const submit = useMutation({
    mutationFn: async () => {
      const out: Started[] = []
      const byFolder = new Map<string, ScannedFile[]>()
      // A runner delivery's files go together: one placement, one import.
      const byPublication = new Map<number, ScannedFile[]>()
      for (const f of chosen) {
        if (f.publication) {
          byPublication.set(f.publication.publicationId, [...(byPublication.get(f.publication.publicationId) ?? []), f])
          continue
        }
        const dir = f.path.slice(0, f.path.lastIndexOf('/')) || '/'
        byFolder.set(dir, [...(byFolder.get(dir) ?? []), f])
      }
      for (const [publicationId, group] of byPublication) {
        const label = group.length === 1 ? group[0].name : `${group.length} files of a runner delivery`
        const downloadId = group[0].publication!.downloadId
        try {
          await placePublicationFiles(downloadId, publicationId, {
            mediaItemId: item.id,
            copyId: copyId || undefined,
            files: group.map((f) => {
              const c = parseChoice(choiceFor(f))
              return { path: f.publication!.path, season: c?.season, episodes: c?.episodes }
            }),
          })
          out.push({ downloadId, label })
        } catch (e) {
          out.push({ label, error: (e as Error).message })
        }
      }
      for (const [dir, group] of byFolder) {
        const label = group.length === 1 ? group[0].name : `${group.length} files from ${dir}`
        try {
          const res = await manualImport({
            path: dir,
            paths: group.map((f) => f.path),
            mediaItemId: item.id,
            copyId: copyId || undefined,
            removeOriginal,
            episodeTargets: item.kind === 'series'
              ? group.flatMap((f) => {
                  const c = parseChoice(choiceFor(f))
                  return c ? [{ path: f.path, season: c.season, episodes: c.episodes }] : []
                })
              : undefined,
          })
          out.push({ downloadId: res.downloadId, label })
        } catch (e) {
          out.push({ label, error: (e as Error).message })
        }
      }
      return out
    },
    onSuccess: (out) => {
      setStarted(out)
      setStep('started')
      void qc.invalidateQueries({ queryKey: ['queue'] })
    },
  })

  return (
    <ActionDialog title={`Import into ${item.title}`} className="ifd" onClose={onClose}>
      <ol className="ifd-steps" aria-label="Import steps">
        <li aria-current={step === 'choose' ? 'step' : undefined}>1 · Choose files</li>
        <li aria-current={step === 'confirm' ? 'step' : undefined}>2 · Confirm</li>
      </ol>

      {step === 'choose' && (
        <>
          <div className="ifd-shortcuts" role="group" aria-label="Go to folder">
            {defaultPath.data?.path && <button type="button" onClick={() => go(defaultPath.data!.path)}>Completed downloads</button>}
            {item.path && <button type="button" onClick={() => go(item.path)}>This item's folder</button>}
            {(roots.data ?? []).map((r) => (
              <button type="button" key={r.id} onClick={() => go(r.path)}>{r.path}</button>
            ))}
          </div>
          <form className="ifd-pathbox" onSubmit={(e) => { e.preventDefault(); if (draft.trim()) go(draft.trim()) }}>
            <input id="import-path" aria-label="Folder path" type="text" spellCheck={false} autoComplete="off"
              placeholder="Type a path…" value={draft} onChange={(e) => setDraft(e.target.value)} />
            <button type="submit" disabled={!draft.trim()}>Open</button>
          </form>
          <nav className="ifd-crumbs mono" aria-label="Current folder">
            <button type="button" className="link-btn" onClick={() => go('/')}>/</button>
            {crumbs.map((c, i) => (
              <span key={c.path}>
                {i > 0 && '/'}
                {i === crumbs.length - 1 ? <span aria-current="location">{c.name}</span>
                  : <button type="button" className="link-btn" onClick={() => go(c.path)}>{c.name}</button>}
              </span>
            ))}
          </nav>
          {files.isError && <div className="banner warning" role="alert">{(files.error as Error).message}</div>}
          <ul className="ifd-list" aria-label="Folder contents">
            {dirs.data?.parent && dirs.data.parent !== current && (
              <li><button type="button" className="ifd-row" onClick={() => go(dirs.data!.parent)}><span aria-hidden="true">↰</span> ..</button></li>
            )}
            {(dirs.data?.dirs ?? []).map((d) => (
              <li key={d.path}><button type="button" className="ifd-row" onClick={() => go(d.path)}><span aria-hidden="true">▸</span> {d.name}/</button></li>
            ))}
            {(files.data ?? []).map((f) => f.skipped ? (
              <li key={f.path} className="ifd-row ifd-skipped">
                <span className="ifd-name">{f.name}</span>
                <span className="muted">{f.skipped}</span>
              </li>
            ) : (
              <li key={f.path}>
                <label className="ifd-row ifd-file">
                  <input type="checkbox" checked={!!selected[f.path]} onChange={() => toggle(f)} />
                  <span className="ifd-name">{f.name}</span>
                  <span className="ifd-meta">
                    <span className="pill pill-neutral">{fmtBytes(f.size)}</span>
                    {item.kind === 'series' && (f.episodes.length
                      ? <span className="pill pill-ok">{episodeLabel(f.season, f.episodes)}</span>
                      : <span className="pill pill-warning">no episode in name</span>)}
                    {f.quality && <span className="pill pill-neutral">{f.quality}</span>}
                    {!!f.subtitles && <span className="pill pill-info">{f.subtitles} sub{f.subtitles === 1 ? '' : 's'}</span>}
                    {f.publication && <span className="pill pill-info">runner delivery</span>}
                  </span>
                </label>
              </li>
            ))}
            {files.isSuccess && files.data.length === 0 && (dirs.data?.dirs.length ?? 0) === 0 && (
              <li className="muted ifd-empty">Nothing importable in this folder.</li>
            )}
          </ul>
          <div className="ifd-foot">
            <span className="muted">{chosen.length} file{chosen.length === 1 ? '' : 's'} selected</span>
            <div className="row-actions">
              <button type="button" onClick={onClose}>Cancel</button>
              <button type="button" className="btn-accent" disabled={chosen.length === 0} onClick={() => setStep('confirm')}>Next</button>
            </div>
          </div>
        </>
      )}

      {step === 'confirm' && (
        <>
          {item.copies.length > 0 && (
            <label className="ifd-copy">
              {item.kind === 'book' ? 'Edition' : 'Copy'}{' '}
              <select value={copyId} onChange={(e) => setCopyId(Number(e.target.value))}>
                <option value={0}>{item.kind === 'book' ? 'Primary edition' : 'Primary copy'}</option>
                {item.copies.map((c) => (
                  <option key={c.id} value={c.id}>{c.name || c.bookType || `Copy ${c.id}`}</option>
                ))}
              </select>
            </label>
          )}
          {plan.isError && <div className="banner warning" role="alert">{(plan.error as Error).message}</div>}
          <ul className="ifd-plan" aria-label="Import plan">
            {chosen.map((f) => {
              const line: ImportPlanFile | undefined = plan.data?.find((p) => p.path === f.path)
              const choice = choiceFor(f)
              return (
                <li key={f.path} className="ifd-plan-line">
                  <div className="ifd-from">
                    <span className="ifd-name">{f.name}</span>
                    {!!line?.subtitles && <span className="muted"> + {line.subtitles} subtitle{line.subtitles === 1 ? '' : 's'}</span>}
                  </div>
                  <div className="ifd-to">
                    {item.kind === 'series' && (
                      <label>
                        <select aria-label={`Episode for ${f.name}`} value={choice} required
                          onChange={(e) => setChoices((prev) => ({ ...prev, [f.path]: e.target.value }))}>
                          {!choice && <option value="">Choose the episode…</option>}
                          {f.episodes.length > 1 && <option value={choiceOf(f.season, f.episodes)}>{episodeLabel(f.season, f.episodes)} (from the name)</option>}
                          {episodeOptions.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                        </select>
                      </label>
                    )}
                    {plan.isFetching && !line && <span className="muted">Working out where it goes…</span>}
                    {line?.error && <span className="error-text" role="alert">{line.error}</span>}
                    {line?.destination && !line.error && (
                      <>
                        <span className="mono ifd-dest">{line.destination}</span>
                        <span className={`pill ${line.action === 'replace' ? 'pill-warning' : line.action === 'beside' ? 'pill-info' : 'pill-ok'}`}>
                          {line.action === 'replace' ? `replaces ${line.existing}` : line.action === 'beside' ? `kept beside ${line.existing}` : 'new file'}
                        </span>
                        {line.reason && <span className="muted">{line.reason}</span>}
                        {line.note && <span className="warning-text">{line.note}</span>}
                        {line.mode && <span className="muted">{MODE_TEXT[line.mode]}</span>}
                      </>
                    )}
                  </div>
                </li>
              )
            })}
          </ul>
          {chosen.some((f) => f.publication) && (
            <p className="muted">Runner deliveries: curator places these files and cleans up the delivery folder under its usual rules. Cinema is told when they land.</p>
          )}
          {loose.length > 0 && (
            <label className="ifd-remove">
              <input type="checkbox" checked={removeOriginal} onChange={(e) => setRemoveOriginal(e.target.checked)} />
              Remove original{loose.length < chosen.length ? ' (loose files only)' : ''} — renamed on the same volume; copied, verified and removed across volumes
            </label>
          )}
          {submit.isError && <div className="banner warning" role="alert">{(submit.error as Error).message}</div>}
          <div className="ifd-foot">
            <span className="muted">{chosen.length} file{chosen.length === 1 ? '' : 's'}</span>
            <div className="row-actions">
              <button type="button" onClick={() => setStep('choose')}>Back</button>
              <button type="button" className="btn-accent" disabled={blocked || plan.isFetching || submit.isPending} onClick={() => submit.mutate()}>
                {submit.isPending ? 'Starting…' : `Import ${chosen.length} file${chosen.length === 1 ? '' : 's'}`}
              </button>
            </div>
          </div>
        </>
      )}

      {step === 'started' && <ImportStarted started={started} onClose={onClose} />}
    </ActionDialog>
  )
}

const TERMINAL = new Set(['imported', 'failed'])

/** What a row's trace says Remove original did, or is doing. */
function originalsNote(row: QueueItem): string {
  const trace = row.handoff ?? []
  for (let i = trace.length - 1; i >= 0; i--) {
    if (trace[i].step === 'originals_removal_done') return trace[i].detail ?? ''
    if (trace[i].step === 'originals_removal_pending') return 'Removing the originals…'
  }
  return ''
}

/** The jobs an import started, live through completion: state, Stop, and what happened to the originals. */
function ImportStarted({ started, onClose }: { started: Started[]; onClose: () => void }) {
  const ids = started.flatMap((s) => (s.downloadId ? [s.downloadId] : []))
  const rows = useQueries({
    queries: ids.map((id) => ({
      queryKey: ['queue-item', id],
      queryFn: () => getQueueItem(id),
      refetchInterval: (query: { state: { data?: QueueItem } }) => {
        const row = query.state.data
        return row && TERMINAL.has(row.state) && !originalsNote(row).endsWith('…') ? false : 2000
      },
    })),
  })
  const byId = new Map(ids.map((id, i) => [id, rows[i]?.data]))
  const [stopMsg, setStopMsg] = useState('')
  const stop = useMutation({
    mutationFn: ({ id, generationKey, epoch }: { id: number; generationKey: string; epoch: string }) =>
      cancelQueueImport(id, { generationKey, epoch }),
    onSuccess: () => { setStopMsg('Stopped. The files stay where they were; retry from Activity.'); rows.forEach((r) => void r.refetch()) },
    onError: (e) => setStopMsg(`✕ ${(e as Error).message}`),
  })
  return (
    <div className="ifd-started" role="status">
      <ul className="ifd-plan">
        {started.map((s, i) => {
          const row = s.downloadId ? byId.get(s.downloadId) : undefined
          const running = row && (row.state === 'importing' || row.importState)
          const originals = row ? originalsNote(row) : ''
          return (
            <li key={i} className="ifd-plan-line">
              <span className="ifd-name">{s.label}</span>
              {s.error ? <span className="error-text">✕ {s.error}</span> : (
                <span>
                  Activity #{s.downloadId}:{' '}
                  {row ? `${row.state}${row.bytes && !TERMINAL.has(row.state) ? ` · ${fmtBytes(row.bytes)}` : ''}${row.state === 'failed' && row.error ? ` — ${row.error}` : ''}` : 'queued'}
                  {running && row.importControl && (
                    <button type="button" disabled={stop.isPending}
                      onClick={() => stop.mutate({ id: row.id, generationKey: row.importControl!.generationKey, epoch: row.importControl!.epoch })}>
                      Stop
                    </button>
                  )}
                  {originals && <><br /><span className="muted">{originals}</span></>}
                </span>
              )}
            </li>
          )
        })}
      </ul>
      {stopMsg && <p>{stopMsg}</p>}
      <div className="ifd-foot">
        <span className="muted">Progress also shows in Activity.</span>
        <div className="row-actions"><button type="button" className="btn-accent" onClick={onClose}>Done</button></div>
      </div>
    </div>
  )
}
