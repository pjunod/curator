import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { deleteCompletedEntry, fmtBytes, getCompletedInventory, getSettings, runTask, updateSettings } from './api'

export function CompletedFolders({ onImport }: { onImport: (path: string) => void }) {
  const inventory = useQuery({
    queryKey: ['completed-inventory'], queryFn: getCompletedInventory, refetchInterval: 10_000,
  })
  const scan = useMutation({ mutationFn: () => runTask('downloads.inventory') })
  const cleanup = useMutation({ mutationFn: () => runTask('downloads.cleanup') })
  const qc = useQueryClient()
  const [confirm, setConfirm] = useState<string | null>(null)
  const remove = useMutation({
    mutationFn: ({ path, fingerprint }: { path: string; fingerprint: string }) => deleteCompletedEntry(path, fingerprint),
    onSuccess: () => { setConfirm(null); void qc.invalidateQueries({ queryKey: ['completed-inventory'] }) },
  })
  const [shown, setShown] = useState(50)
  const data = inventory.data
  const entries = data?.roots.flatMap((root) => root.entries.map((entry) => ({ root, entry }))) ?? []
  return <section className="panel">
    <h2>Download storage</h2>
    <p className="muted">
      {data ? `${fmtBytes(data.bytes)} on disk · ${data.attention} entries need attention` : 'Loading disk inventory…'}
      {' '}· Logical file sizes, measured every 5 minutes.
    </p>
    <div className="form-row">
      <button disabled={scan.isPending} onClick={() => scan.mutate()}>Scan download storage</button>
      <button disabled={cleanup.isPending} onClick={() => cleanup.mutate()}>Retry imported cleanup</button>
    </div>
    {(scan.isSuccess || cleanup.isSuccess) && <p className="muted">Task queued. The next inventory scan will show the result.</p>}
    {(inventory.error || scan.error || cleanup.error || remove.error) && <p className="error-text">{(inventory.error || scan.error || cleanup.error || remove.error)?.message}</p>}
    {data?.roots.length === 0 && <p className="muted">No scan yet. Configure download storage folders in Settings if needed.</p>}
    {data?.roots.map((root) => <p key={root.path} className={root.error ? 'error-text' : 'muted'}>
      <span className="mono">{root.path}</span> · {fmtBytes(root.bytes)} ·{' '}
      {root.lastCompleteAt.startsWith('0001') ? 'No complete scan' : `Last complete scan: ${new Date(root.lastCompleteAt).toLocaleString()}`}
      {root.error && ` · Scan incomplete: ${root.error}. Counts may be stale or partial.`}
    </p>)}
    {entries.length > 0 && <details>
      <summary>Account for {entries.length} entries</summary>
      <div style={{ overflowX: 'auto' }}><table>
        <thead><tr><th>Path</th><th>Size</th><th>Explanation</th><th>Download</th><th></th></tr></thead>
        <tbody>{entries.slice(0, shown).map(({ root, entry }) => <tr key={`${root.path}:${entry.path}`}>
          <td className="mono">{entry.path}</td>
          <td>{fmtBytes(entry.bytes)}<br />{entry.files} files</td>
          <td>{entry.status.replace(/_/g, ' ')}<br /><span className="muted">{entry.reason}</span></td>
          <td>{entry.receipts.map((r, i) => <div key={`${r.downloadId}:${r.path}:${i}`}>#{r.downloadId} {r.title}{!r.live && ' (history only)'}</div>)}</td>
          <td><button disabled={!!root.error || ['active', 'symlink', 'retained', 'storage'].includes(entry.status)} onClick={() => onImport(entry.path)}>Review import</button>
            {!['active', 'awaiting_import', 'storage'].includes(entry.status) && (confirm === entry.path
              ? <div><p>Delete these {entry.files} files permanently?</p>
                  <button disabled={remove.isPending} onClick={() => remove.mutate({ path: entry.path, fingerprint: entry.fingerprint })}>Confirm delete</button>
                  <button onClick={() => setConfirm(null)}>Keep files</button></div>
              : <button disabled={!!root.error || remove.isPending} onClick={() => setConfirm(entry.path)}>Delete files…</button>)}
          </td>
        </tr>)}</tbody>
      </table></div>
      {shown < entries.length && <button onClick={() => setShown(shown + 50)}>Show more</button>}
    </details>}
  </section>
}

export function CompletedFolderSettings() {
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const qc = useQueryClient()
  const [value, setValue] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => updateSettings({ completedRoots: value ?? settings.data?.completedRoots ?? '' }),
    onSuccess: () => { setValue(null); void qc.invalidateQueries({ queryKey: ['settings'] }) },
  })
  return <>
    <h3 style={{ marginTop: 18 }}>Download storage folders</h3>
    <p className="muted">Curator-owned working folders, one absolute path per line as seen by the server.
      Every file beneath these roots is accounted for, including completed downloads.
      Leave blank to use /working/monarr when mounted, otherwise discover download folders. Unknown files remain visible for review. Saving these folders also accepts their current mount identities after a mount replacement.</p>
    <textarea rows={3} className="mono" aria-label="Download storage folders"
      value={value ?? settings.data?.completedRoots ?? ''} onChange={(e) => setValue(e.target.value)}
      placeholder="/working/monarr" />
    <button disabled={settings.isPending || save.isPending} onClick={() => save.mutate()}>Save download storage</button>
    {save.isSuccess && <p className="muted">Saved — applies on the next inventory scan.</p>}
    {save.error && <p className="error-text">{save.error.message}</p>}
  </>
}
