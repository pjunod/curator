import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRecoveryPreview, cancelRecoveryImport, getRecoveryImports, fmtBytes, getLibrary, getLibraryItem, getRecoveries, getRecoveryImport, getRecoverySettings, previewRecovery, putRecoverySettings, queueRecovery } from '../api'
import type { RecoveryPreview, RecoverySettings } from '../api'

export function RecoveryEnableSettings() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['recovery-settings'], queryFn: getRecoverySettings })
  const [draft, setDraft] = useState<RecoverySettings | null>(null)
  const value = draft ?? settings.data?.settings
  const save = useMutation({ mutationFn: putRecoverySettings, onSuccess: () => { setDraft(null); void qc.invalidateQueries({ queryKey: ['recovery-settings'] }) } })
  if (!value) return <p>{settings.error?.message ?? 'Loading recovery settings…'}</p>
  const edit = (patch: Partial<RecoverySettings>) => setDraft({ ...value, ...patch })
  return <section className="panel">
    <h2>Enable Runner recovery</h2>
    <p>Readiness is advisory. Explicit imports and committed receipt delivery remain available.</p>
    <label><input type="checkbox" checked={value.enabled} onChange={e => edit({ enabled: e.target.checked })} /> Enable automatic recovery status refresh</label>
    <label>Runner published prefix <input value={value.remote_root} onChange={e => edit({ remote_root: e.target.value })} /></label>
    <label>Curator recovery mount <input value={value.local_root} onChange={e => edit({ local_root: e.target.value })} /></label>
    <label>Recovery credential <input type="password" autoComplete="new-password" value={value.consumer_token ?? ''} placeholder={value.credential_configured ? 'Configured — blank keeps existing' : 'Same recovery credential as Runner'} onChange={e => edit({ consumer_token: e.target.value })} /></label>
    <button disabled={save.isPending} onClick={() => save.mutate(value)}>Save recovery settings</button>
    {save.error && <p role="alert">{save.error.message}</p>}
    <ul>{Object.entries(settings.data?.advisory ?? {}).map(([key, status]) => <li key={key}>{key.replaceAll('_', ' ')}: {status === true ? 'met' : status === false ? 'unmet' : String(status)}</li>)}</ul>
    <p>Mount only Runner’s published directory read-only here, outside library and completed folders. Capacity may need the original, staged copy, library copy and replacement backup at once.</p>
  </section>
}
export function RecoveryImports() {
  const qc = useQueryClient()
  const jobs = useQuery({ queryKey: ['recovery-imports'], queryFn: getRecoveryImports, refetchInterval: 5000 })
  const cancel = useMutation({ mutationFn: cancelRecoveryImport, onSuccess: () => { void qc.invalidateQueries({ queryKey: ['recovery-imports'] }) } })
  const settings = useQuery({ queryKey: ['recovery-settings'], queryFn: getRecoverySettings })
  const recoveries = useQuery({ queryKey: ['runner-recoveries'], queryFn: getRecoveries, refetchInterval: settings.data?.settings.enabled ? 15000 : false })
  const library = useQuery({ queryKey: ['library', 'all'], queryFn: () => getLibrary() })
  const [recoveryKey, setRecoveryKey] = useState(''), [itemId, setItemId] = useState(0), [copyId, setCopyId] = useState(0)
  const [episodeTargets, setEpisodeTargets] = useState<Record<string, { season: number; episodes: number[] }>>({})
  const [unverified, setUnverified] = useState(false)
  const selectionVersion = useRef(0)
  const [preview, setPreview] = useState<RecoveryPreview | null>(null), [importId, setImportId] = useState('')
  const [previewId, setPreviewId] = useState('')
  const previewTask = useQuery({ queryKey: ['recovery-preview', previewId], queryFn: () => getRecoveryPreview(previewId), enabled: !!previewId, refetchInterval: query => ['ready', 'failed'].includes(query.state.data?.state ?? '') ? false : 2000 })
  useEffect(() => { if (previewTask.data?.preview) setPreview(previewTask.data.preview) }, [previewTask.data])
  const item = useQuery({ queryKey: ['library', itemId], queryFn: () => getLibraryItem(itemId), enabled: itemId > 0 })
  const status = useQuery({ queryKey: ['recovery-import', importId], queryFn: () => getRecoveryImport(importId), enabled: !!importId, refetchInterval: importId ? 5000 : false })
  const recovery = recoveries.data?.find(r => r.client_id + ':' + r.id === recoveryKey)
  const inspect = useMutation({ mutationFn: async (request: Parameters<typeof previewRecovery>[0]) => { const version=selectionVersion.current; const task=await previewRecovery(request); if(version===selectionVersion.current) setPreviewId(task.id); return task } })
  const queue = useMutation({ mutationFn: queueRecovery, onSuccess: job => { setImportId(job.id); void qc.invalidateQueries({ queryKey: ['recovery-imports'] }) } })
  const changed = () => { selectionVersion.current++; setPreview(null); setImportId(''); setPreviewId('') }
  return <section className="panel">
    <h2>Runner recovery imports</h2><p>Choose the files to stage in Runner’s Files view, then select their library target here. Each handoff imports all its staged files together. Failed or partial imports keep the original on hold.</p>
    <button onClick={() => void recoveries.refetch()}>Refresh handoffs</button>
    {recoveries.error && <p role="alert">{recoveries.error.message}</p>}
    {(recoveries.data ?? []).filter(r => r.error).map((r, i) => <p role="alert" key={i}>{r.error}</p>)}
    <label>Recovery <select value={recoveryKey} onChange={e => { setRecoveryKey(e.target.value); setEpisodeTargets({}); changed() }}><option value="">Select staged recovery</option>{(recoveries.data ?? []).filter(r => r.id).map(r => <option key={r.client_id + ':' + r.id} value={r.client_id + ':' + r.id}>{r.files[0]?.path ?? r.id} · {r.state} · {r.files.length} files</option>)}</select></label>
    <label>Library title <select value={itemId} onChange={e => { setItemId(Number(e.target.value)); setCopyId(0); setEpisodeTargets({}); changed() }}><option value="0">Select title</option>{library.data?.map(i => <option key={i.id} value={i.id}>{i.title} ({i.kind})</option>)}</select></label>
    <label>Library copy <select value={copyId} onChange={e => { setCopyId(Number(e.target.value)); changed() }}><option value="0">Primary</option>{item.data?.copies?.map(c => <option key={c.id} value={c.id}>{c.name || c.path || 'Additional copy'}</option>)}</select></label>
    {recovery?.files.map(f => <label key={f.id} style={{ display: 'block' }}> {f.path} · {fmtBytes(f.bytes)} {item.data?.kind === 'series' && <input key={recoveryKey + ':' + itemId + ':' + f.id} aria-label={'Episode mapping for ' + f.path} placeholder="Season:episodes, e.g. 1:2,3 (blank uses filename)" onChange={e => { const [season, episodes] = e.target.value.split(':'); const next = { ...episodeTargets }; if (!e.target.value) delete next[f.id]; else next[f.id] = { season: Number(season), episodes: (episodes ?? '').split(',').filter(Boolean).map(Number) }; setEpisodeTargets(next); changed() }} />}</label>)}
    <label><input type="checkbox" checked={unverified} onChange={e => { setUnverified(e.target.checked); changed() }} /> Accept recognized media whose quality is unverified</label>
    <button disabled={!recovery || !itemId || !recovery.files.length || inspect.isPending} onClick={() => recovery && inspect.mutate({ client_id: recovery.client_id, recovery_id: recovery.id, media_item_id: itemId, copy_id: copyId, file_ids: recovery.files.map(f => f.id), episode_targets: episodeTargets, target_generation: '', accept_unverified: unverified })}>{inspect.isPending ? 'Verifying staged files…' : 'Preview import'}</button>
    {inspect.error && <p role="alert">{inspect.error.message}</p>}
    {previewTask.data && <p role="status">Preview {previewTask.data.state}{previewTask.data.error ? ": " + previewTask.data.error : ""}</p>}
    {previewTask.error && <p role="alert">{previewTask.error.message}</p>}
    {preview && <div><h3>Target: {preview.target}</h3><p>Copy {fmtBytes(preview.copy_bytes)}. Media recognized; completeness not proven. A matching digest proves that the copy matches its source, not that the source is healthy.</p><ul>{preview.files.map(f => <li key={f.id}>{f.path} → {f.destination} · {f.usable ? 'ready' : f.reason}{f.existing?.length ? ` · Existing files in this copy: ${f.existing.join('; ')}` : ''}{f.episodes?.length ? ` · season ${f.season}, episodes ${f.episodes.join(', ')}` : ''}</li>)}</ul><button disabled={queue.isPending || preview.files.some(f => !f.usable)} onClick={() => queue.mutate(preview.request)}>Queue handoff import</button></div>}
    {queue.error && <p role="alert">{queue.error.message}</p>}
    <h3>Import activity</h3>
    {jobs.error && <p role="alert">{jobs.error.message}</p>}
    <ul>{(jobs.data ?? []).map(job => <li key={job.id}>{job.id.slice(0, 12)} · {job.state}{job.current_file ? ` · ${job.current_file}: ${fmtBytes(job.copied_bytes ?? 0)} / ${fmtBytes(job.total_bytes ?? 0)}` : ''}{job.error ? ': ' + job.error : ''} {['queued', 'importing', 'review'].includes(job.state) && <button disabled={cancel.isPending} onClick={() => cancel.mutate(job.id)}>Cancel import</button>}</li>)}</ul>
    {cancel.error && <p role="alert">{cancel.error.message}</p>}
    <p>Cancellation waits for the active file operation to stop. Files already committed to the library remain recorded; Runner retains the source for review.</p>
    {status.data && <p role="status">Import {status.data.state}{status.data.error ? ': ' + status.data.error : ''}</p>}
  </section>
}
