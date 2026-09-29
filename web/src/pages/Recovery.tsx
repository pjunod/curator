import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  cancelRecoveryImport, fmtBytes, getLibrary, getLibraryItem, getRecoveries, getRecoveryImport, getRecoveryImports,
  getRecoveryPreview, getRecoverySettings, previewRecovery, putRecoverySettings, queueRecovery,
} from '../api'
import type { MediaItemSummary, RecoveryPreview, RecoverySettings, RunnerRecovery } from '../api'

// ---- pure helpers (unit-tested in recovery.test.ts) --------------------------

/** Last path component; the folder Runner staged the copy from. */
export function baseName(p: string | undefined): string {
  const s = String(p ?? '').replace(/\/+$/, '')
  const i = s.lastIndexOf('/')
  return i >= 0 ? s.slice(i + 1) : s
}

/** What to call a handoff: its source folder, else its first file, else its id. */
export function handoffName(r: RunnerRecovery): string {
  return baseName(r.source) || r.files[0]?.path || r.id
}

export function handoffBytes(r: RunnerRecovery): number {
  return r.files.reduce((sum, f) => sum + (f.bytes || 0), 0)
}

export function fmtAgo(unixSeconds: number | undefined, now = Date.now()): string {
  if (!unixSeconds) return ''
  const s = Math.max(0, Math.floor(now / 1000 - unixSeconds))
  if (s < 90) return s + 's ago'
  if (s < 5400) return Math.round(s / 60) + 'm ago'
  if (s < 129600) return Math.round(s / 3600) + 'h ago'
  return Math.round(s / 86400) + 'd ago'
}

export interface HandoffStatus {
  /** Can this handoff be picked as the thing to import? */
  selectable: boolean
  pill: 'ok' | 'warning' | 'error' | 'neutral'
  label: string
  /** Why it cannot be selected, or what is happening to it. */
  note: string
}

/**
 * Runner's handoff states, as an operator needs to read them here. Only a
 * `published` copy (or one this Curator already claimed) can be imported.
 */
export function handoffStatus(r: RunnerRecovery): HandoffStatus {
  switch (r.state) {
    case 'published':
      return { selectable: true, pill: 'ok', label: 'ready', note: 'Staged and verified by Runner; ready to import.' }
    case 'claimed':
      return { selectable: true, pill: 'ok', label: 'claimed', note: r.consumer ? 'Claimed by ' + r.consumer + '.' : 'Claimed for import.' }
    case 'staging':
    case 'publishing':
      return { selectable: false, pill: 'warning', label: 'copying', note: 'Runner is still copying and verifying the files. Refresh in a little while.' }
    case 'failed':
      return { selectable: false, pill: 'error', label: 'staging failed', note: 'Runner could not stage this copy' + (r.error ? ': ' + r.error : '.') + ' Cancel it in Runner’s Files tab (the folder’s details) and stage it again.' }
    case 'cancel_pending':
      return { selectable: false, pill: 'neutral', label: 'cancelling', note: 'Being withdrawn; it disappears once Runner’s worker stops.' }
    case 'imported':
      return { selectable: false, pill: 'ok', label: 'imported', note: 'Already in the library; Runner has the receipt.' }
    case 'partial':
      return { selectable: false, pill: 'warning', label: 'partial', note: 'Some files imported, some did not' + (r.error ? ': ' + r.error : '.') + ' The source stays held in Runner for review.' }
    case 'cancelled':
      return { selectable: false, pill: 'neutral', label: 'cancelled', note: 'Withdrawn.' }
    default:
      return { selectable: false, pill: 'neutral', label: r.state || 'unknown', note: r.error ?? '' }
  }
}

/** Why the Preview button is disabled, in words — or '' when it is not. */
export function previewBlocker(o: { mount: boolean | null; handoff: RunnerRecovery | undefined; itemId: number; pending: boolean }): string {
  if (o.pending) return ''
  if (!o.handoff) return 'Choose a handoff first.'
  if (!handoffStatus(o.handoff).selectable) return 'That handoff is not ready to import.'
  if (!o.handoff.files.length) return 'That handoff has no files.'
  if (!o.itemId) return 'Pick the library title these files belong to.'
  if (o.mount === false) return 'The recovery mount is not available to Curator — set it under Settings → Dev.'
  return ''
}

/** "1:2,3" → { season: 1, episodes: [2, 3] }; blank or malformed → null (filename decides). */
export function parseEpisodeTarget(text: string): { season: number; episodes: number[] } | null {
  const m = text.trim().match(/^(\d+)\s*[:\s]\s*([\d,\s]+)$/)
  if (!m) return null
  const episodes = m[2].split(/[,\s]+/).filter(Boolean).map(Number).filter(n => Number.isInteger(n) && n > 0)
  if (!episodes.length) return null
  return { season: Number(m[1]), episodes }
}

export function filterTitles(items: MediaItemSummary[] | undefined, q: string): MediaItemSummary[] {
  const needle = q.trim().toLowerCase()
  const all = items ?? []
  if (!needle) return all
  return all.filter(i => (i.title + ' ' + (i.year || '') + ' ' + (i.author || '')).toLowerCase().includes(needle))
}

// ---- Settings → Dev: enablement ------------------------------------------------

export function RecoveryEnableSettings() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['recovery-settings'], queryFn: getRecoverySettings })
  const [draft, setDraft] = useState<RecoverySettings | null>(null)
  const value = draft ?? settings.data?.settings
  const save = useMutation({ mutationFn: putRecoverySettings, onSuccess: () => { setDraft(null); void qc.invalidateQueries({ queryKey: ['recovery-settings'] }) } })
  if (!value) return <p>{settings.error?.message ?? 'Loading recovery settings…'}</p>
  const edit = (patch: Partial<RecoverySettings>) => setDraft({ ...value, ...patch })
  return <section className="panel recovery-enable">
    <h2>Enable Runner recovery</h2>
    <p className="muted">Readiness is advisory. Explicit imports and committed receipt delivery remain available.</p>
    <label className="inline"><input type="checkbox" checked={value.enabled} onChange={e => edit({ enabled: e.target.checked })} /> Enable automatic recovery status refresh</label>
    <div className="recovery-fields">
      <label>Runner published prefix <input type="text" value={value.remote_root} onChange={e => edit({ remote_root: e.target.value })} /></label>
      <label>Curator recovery mount <input type="text" value={value.local_root} onChange={e => edit({ local_root: e.target.value })} /></label>
      <label>Recovery credential <input type="password" autoComplete="new-password" value={value.consumer_token ?? ''} placeholder={value.credential_configured ? 'Configured — blank keeps existing' : 'Same recovery credential as Runner'} onChange={e => edit({ consumer_token: e.target.value })} /></label>
    </div>
    <div className="form-row">
      <button className="btn-accent" disabled={save.isPending} onClick={() => save.mutate(value)}>Save recovery settings</button>
      {save.error && <span className="error-text" role="alert">{save.error.message}</span>}
    </div>
    <ul className="recovery-advisory">{Object.entries(settings.data?.advisory ?? {}).map(([key, status]) => <li key={key}>{key.replaceAll('_', ' ')}: <span className={'pill ' + (status === true ? 'pill-ok' : status === false ? 'pill-error' : 'pill-neutral')}>{status === true ? 'met' : status === false ? 'unmet' : 'verify'}</span>{typeof status === 'string' ? <span className="muted"> — {status}</span> : null}</li>)}</ul>
    <p className="muted">Mount only Runner’s published directory (<code>&lt;recovery-root&gt;/published</code>) read-only at the mount above, outside library and completed folders. Capacity may need the original, staged copy, library copy and replacement backup at once.</p>
  </section>
}

// ---- Settings → Recovery: the import flow -------------------------------------

function Readiness({ label, ok, detail }: { label: string; ok: boolean | null; detail: string }) {
  return <div className="recovery-ready" data-ok={ok === null ? 'unknown' : String(ok)}>
    <span className={'pill ' + (ok === true ? 'pill-ok' : ok === false ? 'pill-error' : 'pill-neutral')}>{ok === true ? 'ok' : ok === false ? 'missing' : '…'}</span>
    <span className="recovery-ready-label">{label}</span>
    <span className="recovery-ready-detail muted">{detail}</span>
  </div>
}

export function RecoveryImports() {
  const qc = useQueryClient()
  const jobs = useQuery({ queryKey: ['recovery-imports'], queryFn: getRecoveryImports, refetchInterval: 5000 })
  const cancel = useMutation({ mutationFn: cancelRecoveryImport, onSuccess: () => { void qc.invalidateQueries({ queryKey: ['recovery-imports'] }) } })
  const settings = useQuery({ queryKey: ['recovery-settings'], queryFn: getRecoverySettings })
  const recoveries = useQuery({ queryKey: ['runner-recoveries'], queryFn: getRecoveries, refetchInterval: settings.data?.settings.enabled ? 15000 : false })
  const library = useQuery({ queryKey: ['library', 'all'], queryFn: () => getLibrary() })
  const [recoveryKey, setRecoveryKey] = useState(''), [itemId, setItemId] = useState(0), [copyId, setCopyId] = useState(0)
  const [titleQuery, setTitleQuery] = useState('')
  const [episodeText, setEpisodeText] = useState<Record<string, string>>({})
  const [unverified, setUnverified] = useState(false)
  const selectionVersion = useRef(0)
  const [preview, setPreview] = useState<RecoveryPreview | null>(null), [importId, setImportId] = useState('')
  const [previewId, setPreviewId] = useState('')
  const previewTask = useQuery({ queryKey: ['recovery-preview', previewId], queryFn: () => getRecoveryPreview(previewId), enabled: !!previewId, refetchInterval: query => ['ready', 'failed'].includes(query.state.data?.state ?? '') ? false : 2000 })
  useEffect(() => { if (previewTask.data?.preview) setPreview(previewTask.data.preview) }, [previewTask.data])
  const item = useQuery({ queryKey: ['library', itemId], queryFn: () => getLibraryItem(itemId), enabled: itemId > 0 })
  const status = useQuery({ queryKey: ['recovery-import', importId], queryFn: () => getRecoveryImport(importId), enabled: !!importId, refetchInterval: importId ? 5000 : false })

  const rows = recoveries.data ?? []
  const clientErrors = rows.filter(r => !r.id && r.error)
  const handoffs = rows.filter(r => r.id)
  const recovery = handoffs.find(r => r.client_id + ':' + r.id === recoveryKey)
  const advisory = settings.data?.advisory
  const mount: boolean | null = advisory ? advisory.local_mount_available === true : null
  const runnerOk: boolean | null = recoveries.isLoading ? null : !recoveries.error && clientErrors.length === 0
  const titles = useMemo(() => filterTitles(library.data, titleQuery), [library.data, titleQuery])
  const episodeTargets = useMemo(() => {
    const out: Record<string, { season: number; episodes: number[] }> = {}
    for (const [id, text] of Object.entries(episodeText)) { const t = parseEpisodeTarget(text); if (t) out[id] = t }
    return out
  }, [episodeText])

  const inspect = useMutation({ mutationFn: async (request: Parameters<typeof previewRecovery>[0]) => { const version = selectionVersion.current; const task = await previewRecovery(request); if (version === selectionVersion.current) setPreviewId(task.id); return task } })
  const queue = useMutation({ mutationFn: queueRecovery, onSuccess: job => { setImportId(job.id); void qc.invalidateQueries({ queryKey: ['recovery-imports'] }) } })
  const changed = () => { selectionVersion.current++; setPreview(null); setImportId(''); setPreviewId('') }
  const blocker = previewBlocker({ mount, handoff: recovery, itemId, pending: inspect.isPending })
  const chooseHandoff = (key: string) => { setRecoveryKey(key); setEpisodeText({}); changed() }
  const chooseTitle = (id: number) => { setItemId(id); setCopyId(0); setEpisodeText({}); changed() }
  const isSeries = item.data?.kind === 'series'
  const copies = item.data?.copies ?? []

  return <section className="panel recovery">
    <h2>Runner recovery imports</h2>
    <p className="muted">Media that Runner is holding — a failed download it parked, or a folder it adopted — comes into the library in three steps: Runner stages a verified copy (its <b>Files</b> tab: open the folder’s details, tick the files, <i>Preview recovery copy</i>, <i>Stage</i>), you point that handoff at a library title here, and Curator copies it into place and sends Runner the receipt.</p>

    <div className="recovery-readiness" aria-label="Recovery readiness">
      <Readiness label="Runner" ok={runnerOk} detail={runnerOk === false ? (recoveries.error?.message ?? clientErrors[0]?.error ?? 'unreachable') : handoffs.length + (handoffs.length === 1 ? ' handoff' : ' handoffs')} />
      <Readiness label="Recovery mount" ok={mount} detail={settings.data?.settings.local_root || 'not set'} />
      <Readiness label="Credential" ok={advisory ? advisory.credential_configured === true : null} detail={advisory?.credential_configured === true ? 'configured' : 'set the shared recovery credential under Settings → Dev'} />
    </div>
    {mount === false && <div className="banner warning" role="alert">Curator cannot see the recovery mount at <code>{settings.data?.settings.local_root || '(unset)'}</code>. Previews and imports will fail until Runner’s <code>published</code> directory is mounted read-only there. Fix the mount, or the path under Settings → Dev.</div>}
    {clientErrors.map((r, i) => <div className="banner warning" role="alert" key={i}>Runner client {r.client_id}: {r.error}</div>)}
    {recoveries.error && <div className="banner warning" role="alert">{recoveries.error.message}</div>}

    <ol className="recovery-steps">
      <li className="recovery-step" data-done={String(!!recovery)}>
        <div className="recovery-step-head">
          <span className="recovery-step-no">1</span>
          <h3>Choose a handoff</h3>
          <button className="small" onClick={() => void recoveries.refetch()} disabled={recoveries.isFetching}>{recoveries.isFetching ? 'Refreshing…' : 'Refresh'}</button>
        </div>
        {recoveries.isLoading && <p className="muted">Asking Runner for staged handoffs…</p>}
        {!recoveries.isLoading && !handoffs.length && <p className="muted recovery-empty">Nothing staged. Stage a copy in Runner’s Files tab first — it appears here within a few seconds.</p>}
        {handoffs.length > 0 && <table className="recovery-handoffs">
          <thead><tr><th></th><th>Folder</th><th>Status</th><th>Files</th><th>Size</th><th>Staged</th></tr></thead>
          <tbody>{handoffs.map(r => {
            const key = r.client_id + ':' + r.id, st = handoffStatus(r), on = key === recoveryKey
            return <tr key={key} className={'recovery-handoff' + (on ? ' is-selected' : '') + (st.selectable ? '' : ' is-unavailable')} onClick={() => st.selectable && chooseHandoff(key)}>
              <td className="recovery-pick"><input type="radio" name="recovery-handoff" aria-label={'Select ' + handoffName(r)} checked={on} disabled={!st.selectable} onChange={() => chooseHandoff(key)} /></td>
              <td><div className="recovery-name" title={r.source || r.id}>{handoffName(r)}</div><div className="muted recovery-sub">{r.id.slice(0, 8)}… · {st.note}</div></td>
              <td><span className={'pill pill-' + st.pill}>{st.label}</span></td>
              <td className="num">{r.files.length}</td>
              <td className="num">{fmtBytes(handoffBytes(r))}</td>
              <td className="muted">{fmtAgo(r.created_at) || '—'}</td>
            </tr>
          })}</tbody>
        </table>}
      </li>

      <li className="recovery-step" data-done={String(itemId > 0)}>
        <div className="recovery-step-head"><span className="recovery-step-no">2</span><h3>Pick the library title</h3></div>
        <div className="form-row">
          <input type="search" placeholder="Search titles…" aria-label="Search library titles" value={titleQuery} onChange={e => setTitleQuery(e.target.value)} disabled={!recovery} />
          <select aria-label="Library title" value={itemId} onChange={e => chooseTitle(Number(e.target.value))} disabled={!recovery}>
            <option value="0">{titles.length ? 'Select a title' : library.data?.length ? 'No title matches' : 'Loading titles…'}</option>
            {titles.map(i => <option key={i.id} value={i.id}>{i.title}{i.year ? ' (' + i.year + ')' : ''} · {i.kind}</option>)}
          </select>
          {copies.length > 1 && <select aria-label="Library copy" value={copyId} onChange={e => { setCopyId(Number(e.target.value)); changed() }}>
            <option value="0">Primary copy</option>
            {copies.map(c => <option key={c.id} value={c.id}>{c.name || c.path || 'Additional copy'}</option>)}
          </select>}
        </div>
        {recovery && isSeries && <div className="recovery-episodes">
          <p className="muted">Series: say which episode each file is, as <code>season:episodes</code> (for example <code>1:2</code>, or <code>1:2,3</code> for a double). Leave blank to let the filename decide.</p>
          <table className="recovery-files"><tbody>{recovery.files.map(f => <tr key={f.id}>
            <td className="recovery-path">{f.path}</td>
            <td className="num">{fmtBytes(f.bytes)}</td>
            <td><input type="text" className="recovery-episode" aria-label={'Episode mapping for ' + f.path} placeholder="season:episodes" value={episodeText[f.id] ?? ''} onChange={e => { setEpisodeText({ ...episodeText, [f.id]: e.target.value }); changed() }} />
              {episodeText[f.id] && !parseEpisodeTarget(episodeText[f.id]) && <span className="error-text"> not season:episodes</span>}</td>
          </tr>)}</tbody></table>
        </div>}
        {recovery && !isSeries && <ul className="recovery-filelist">{recovery.files.map(f => <li key={f.id}><span className="recovery-path">{f.path}</span> <span className="muted">{fmtBytes(f.bytes)}</span></li>)}</ul>}
      </li>

      <li className="recovery-step" data-done={String(!!preview)}>
        <div className="recovery-step-head"><span className="recovery-step-no">3</span><h3>Preview, then import</h3></div>
        <label className="inline"><input type="checkbox" checked={unverified} onChange={e => { setUnverified(e.target.checked); changed() }} /> Accept recognized media whose quality is unverified <span className="muted">(container recognized, completeness not proven)</span></label>
        <div className="form-row">
          <button className="btn-accent" disabled={!!blocker || inspect.isPending} onClick={() => recovery && inspect.mutate({ client_id: recovery.client_id, recovery_id: recovery.id, media_item_id: itemId, copy_id: copyId, file_ids: recovery.files.map(f => f.id), episode_targets: episodeTargets, target_generation: '', accept_unverified: unverified })}>{inspect.isPending ? 'Verifying staged files…' : 'Preview import'}</button>
          {blocker && <span className="muted recovery-blocker">{blocker}</span>}
        </div>
        {inspect.error && <div className="banner warning" role="alert">{inspect.error.message}</div>}
        {previewTask.data && previewTask.data.state !== 'ready' && <p role="status" className={previewTask.data.state === 'failed' ? 'error-text' : 'muted'}>Preview {previewTask.data.state}{previewTask.data.error ? ': ' + previewTask.data.error : '…'}</p>}
        {previewTask.error && <div className="banner warning" role="alert">{previewTask.error.message}</div>}
        {preview && <div className="recovery-preview">
          <h4>Target: {preview.target}</h4>
          <p className="muted">Copies {fmtBytes(preview.copy_bytes)}. A matching digest proves the copy equals its source, not that the source is healthy.</p>
          <table className="recovery-files"><thead><tr><th>File</th><th>Destination</th><th>Status</th></tr></thead><tbody>{preview.files.map(f => <tr key={f.id}>
            <td className="recovery-path">{f.path}{f.episodes?.length ? <div className="muted">season {f.season}, episodes {f.episodes.join(', ')}</div> : null}</td>
            <td className="recovery-path">{f.destination}{f.existing?.length ? <div className="error-text">would replace: {f.existing.join('; ')}</div> : null}</td>
            <td><span className={'pill ' + (f.usable ? 'pill-ok' : 'pill-error')}>{f.usable ? 'ready' : 'blocked'}</span>{!f.usable && <div className="error-text">{f.reason}</div>}</td>
          </tr>)}</tbody></table>
          <div className="form-row">
            <button className="btn-accent" disabled={queue.isPending || preview.files.some(f => !f.usable)} onClick={() => queue.mutate(preview.request)}>{queue.isPending ? 'Queueing…' : 'Import into library'}</button>
            {preview.files.some(f => !f.usable) && <span className="muted">Fix the blocked files (or accept unverified media) and preview again.</span>}
          </div>
          {queue.error && <div className="banner warning" role="alert">{queue.error.message}</div>}
          {status.data && <p role="status">Import {status.data.state}{status.data.error ? ': ' + status.data.error : ''}</p>}
        </div>}
      </li>
    </ol>

    <h3 className="muted">Import activity</h3>
    {jobs.error && <div className="banner warning" role="alert">{jobs.error.message}</div>}
    {!(jobs.data ?? []).length && <p className="muted">No imports yet.</p>}
    {(jobs.data ?? []).length > 0 && <table className="recovery-jobs"><thead><tr><th>Import</th><th>State</th><th>Progress</th><th></th></tr></thead><tbody>{(jobs.data ?? []).map(job => {
      const total = job.total_bytes ?? 0, done = job.copied_bytes ?? 0, active = ['queued', 'importing', 'review'].includes(job.state)
      return <tr key={job.id}>
        <td><code>{job.id.slice(0, 12)}</code></td>
        <td><span className={'pill ' + (job.state === 'imported' ? 'pill-ok' : job.error ? 'pill-error' : active ? 'pill-warning' : 'pill-neutral')}>{job.state}</span>{job.error && <div className="error-text">{job.error}</div>}</td>
        <td>{job.current_file ? <div><div className="recovery-path">{job.current_file}</div><div className="progress-track" aria-label="copy progress"><i style={{ width: (total ? Math.min(100, Math.round(done / total * 100)) : 0) + '%' }} /></div><div className="muted">{fmtBytes(done)} / {fmtBytes(total)}</div></div> : <span className="muted">—</span>}</td>
        <td>{active && <button className="small" disabled={cancel.isPending} onClick={() => cancel.mutate(job.id)}>Cancel import</button>}</td>
      </tr>
    })}</tbody></table>}
    {cancel.error && <div className="banner warning" role="alert">{cancel.error.message}</div>}
    <p className="muted">Cancellation waits for the active file operation to stop. Files already committed to the library remain recorded; Runner retains the source for review.</p>
  </section>
}
