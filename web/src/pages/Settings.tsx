import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  addRootFolder,
  deleteRootFolder,
  fmtBytes,
  fmtRelative,
  getRootFolders,
  getScanReport,
  getSettings,
  triggerScan,
  updateSettings,
} from '../api'
import { AcquisitionSettings } from './SettingsAcquisition'
import { NotifierSettings } from './SettingsNotifiers'

export function SettingsPage() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const report = useQuery({ queryKey: ['scan-report'], queryFn: getScanReport })

  const [key, setKey] = useState('')
  const [newRoot, setNewRoot] = useState('')

  const saveKey = useMutation({
    mutationFn: () => updateSettings({ tmdbApiKey: key }),
    onSuccess: () => {
      setKey('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
      void qc.invalidateQueries({ queryKey: ['health'] })
    },
  })
  const addRoot = useMutation({
    mutationFn: () => addRootFolder(newRoot),
    onSuccess: () => {
      setNewRoot('')
      void qc.invalidateQueries({ queryKey: ['rootfolders'] })
    },
  })
  const delRoot = useMutation({
    mutationFn: deleteRootFolder,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['rootfolders'] }),
  })
  const scan = useMutation({
    mutationFn: triggerScan,
    onSuccess: () => {
      setTimeout(() => void qc.invalidateQueries({ queryKey: ['scan-report'] }), 1500)
    },
  })

  return (
    <>
      <header className="page-head">
        <h1>Settings</h1>
      </header>

      <section className="panel">
        <h2>Metadata provider (TMDB)</h2>
        <p className="muted">
          One key serves movies and TV. Create a free API key at themoviedb.org → Settings → API
          (v3 key or v4 read token — both work).
        </p>
        <div className="form-row">
          <input
            type="password"
            placeholder={
              settings.data?.tmdbApiKeyConfigured
                ? `Configured (${settings.data.tmdbApiKeyHint}) — paste to replace`
                : 'Paste your TMDB API key'
            }
            value={key}
            onChange={(e) => setKey(e.target.value)}
          />
          <button
            className="btn-accent"
            disabled={key.trim() === '' || saveKey.isPending}
            onClick={() => saveKey.mutate()}
          >
            Save key
          </button>
        </div>
        {saveKey.isSuccess && <p className="ok-text">Saved.</p>}
        {saveKey.isError && <div className="banner warning">{String((saveKey.error as Error).message)}</div>}
      </section>

      <section className="panel">
        <h2>Root folders</h2>
        <table>
          <thead>
            <tr>
              <th>Path</th>
              <th>Free</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {roots.data?.map((rf) => (
              <tr key={rf.id}>
                <td className="mono">{rf.path}</td>
                <td className="muted">{fmtBytes(rf.freeBytes)}</td>
                <td>
                  {rf.accessible ? (
                    <span className="pill pill-ok">ok</span>
                  ) : (
                    <span className="pill pill-error">unreachable</span>
                  )}
                </td>
                <td>
                  <button onClick={() => delRoot.mutate(rf.id)}>Remove</button>
                </td>
              </tr>
            ))}
            {roots.data?.length === 0 && (
              <tr>
                <td colSpan={4} className="muted">
                  No root folders yet — add the folder where your media lives.
                </td>
              </tr>
            )}
          </tbody>
        </table>
        <div className="form-row">
          <input
            type="text"
            placeholder="/absolute/path/to/media"
            value={newRoot}
            onChange={(e) => setNewRoot(e.target.value)}
          />
          <button
            className="btn-accent"
            disabled={newRoot.trim() === '' || addRoot.isPending}
            onClick={() => addRoot.mutate()}
          >
            Add root folder
          </button>
        </div>
        {addRoot.isError && <div className="banner warning">{String((addRoot.error as Error).message)}</div>}
      </section>

      <AcquisitionSettings />
      <NotifierSettings />

      <section className="panel">
        <h2>Disk scan</h2>
        <div className="form-row">
          <button onClick={() => scan.mutate()} disabled={scan.isPending}>
            Scan now
          </button>
          {report.data && (
            <span className="muted">
              Last scan {fmtRelative(report.data.scannedAt)} · {report.data.itemsScanned} items ·{' '}
              {report.data.filesLinked} files linked · {report.data.filesRemoved} removed
            </span>
          )}
          {report.data === null && <span className="muted">No scan has run yet.</span>}
        </div>

        {report.data && report.data.unmatchedDirs.length > 0 && (
          <>
            <h3 className="muted">Unmatched folders</h3>
            <ul className="unmatched-list">
              {report.data.unmatchedDirs.map((d) => (
                <li key={d.path}>
                  <span className="mono">{d.path}</span>
                  <Link to="/add" search={{ q: d.name, kind: 'series' }}>
                    Match…
                  </Link>
                </li>
              ))}
            </ul>
          </>
        )}
        {report.data && report.data.missingPaths.length > 0 && (
          <>
            <h3 className="muted">Items whose folder is missing on disk</h3>
            <ul className="unmatched-list">
              {report.data.missingPaths.map((p) => (
                <li key={p}>
                  <span className="mono">{p}</span>
                </li>
              ))}
            </ul>
          </>
        )}
      </section>
    </>
  )
}
