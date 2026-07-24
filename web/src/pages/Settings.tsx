import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { RootKind } from '../api'
import {
  addRootFolder,
  deleteRootFolder,
  fmtBytes,
  fmtRelative,
  getRootFolders,
  getScanReport,
  getSettings,
  triggerScan,
  updateRootFolderKind,
  updateSettings,
} from '../api'
import { AcquisitionSettings } from './SettingsAcquisition'
import { CustomFormatSettings, ImportListSettings, SecuritySettings } from './SettingsDepth'
import { NotifierSettings } from './SettingsNotifiers'

export function SettingsPage() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const report = useQuery({ queryKey: ['scan-report'], queryFn: getScanReport })

  const [key, setKey] = useState('')
  const [omdbKey, setOmdbKey] = useState('')
  const [newRoot, setNewRoot] = useState('')
  const [newRootKind, setNewRootKind] = useState<RootKind>('movie')

  const saveKey = useMutation({
    mutationFn: () => updateSettings({ tmdbApiKey: key }),
    onSuccess: () => {
      setKey('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
      void qc.invalidateQueries({ queryKey: ['health'] })
    },
  })
  const saveOmdbKey = useMutation({
    mutationFn: () => updateSettings({ omdbApiKey: omdbKey }),
    onSuccess: () => {
      setOmdbKey('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    },
  })
  const addRoot = useMutation({
    mutationFn: () => addRootFolder(newRoot, newRootKind),
    onSuccess: () => {
      setNewRoot('')
      void qc.invalidateQueries({ queryKey: ['rootfolders'] })
    },
  })
  const retypeRoot = useMutation({
    mutationFn: ({ id, kind }: { id: number; kind: RootKind }) => updateRootFolderKind(id, kind),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['rootfolders'] }),
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

      <section className="panel" id="metadata">
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

        <h2 style={{ marginTop: 18 }}>Extra ratings (OMDb — optional)</h2>
        <p className="muted">
          An OMDb key adds Rotten Tomatoes, IMDb, and Metacritic scores next to the TMDB
          rating. Free key at omdbapi.com (1000 requests/day). Ratings arrive on add and on
          metadata refresh.
        </p>
        <div className="form-row">
          <input
            type="password"
            placeholder={
              settings.data?.omdbApiKeyConfigured
                ? `Configured (${settings.data.omdbApiKeyHint}) — paste to replace`
                : 'Paste your OMDb API key'
            }
            value={omdbKey}
            onChange={(e) => setOmdbKey(e.target.value)}
          />
          <button
            className="btn-accent"
            disabled={omdbKey.trim() === '' || saveOmdbKey.isPending}
            onClick={() => saveOmdbKey.mutate()}
          >
            Save OMDb key
          </button>
        </div>
        {saveOmdbKey.isSuccess && <p className="ok-text">Saved.</p>}
        {saveOmdbKey.isError && <div className="banner warning">{String((saveOmdbKey.error as Error).message)}</div>}
      </section>

      <section className="panel" id="library-folders">
        <h2>Library folders</h2>
        <p className="muted">
          Root folders are the only places Monarr looks. A scan reconciles what is on disk
          against the library, and anything it finds that no item claims is listed below as a
          candidate to match.
        </p>
        <table>
          <thead>
            <tr>
              <th>Path</th>
              <th>Holds</th>
              <th>Free</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {roots.data?.map((rf) => (
              <tr key={rf.id}>
                <td className="mono">{rf.path}</td>
                <td>
                  <select
                    value={rf.kind}
                    disabled={retypeRoot.isPending}
                    onChange={(e) =>
                      retypeRoot.mutate({ id: rf.id, kind: e.target.value as RootKind })
                    }
                  >
                    <option value="movie">Movies</option>
                    <option value="series">TV series</option>
                    <option value="book">Books</option>
                    <option value="mixed">Mixed — ask</option>
                  </select>
                </td>
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
                <td colSpan={5} className="muted">
                  No root folders yet — add the folder where your media lives.
                </td>
              </tr>
            )}
          </tbody>
        </table>
        {roots.data?.some((rf) => rf.kind === 'mixed') && (
          <p className="muted">
            A folder set to <em>Mixed</em> has to ask what every unmatched folder is. Setting it
            to a single kind is what lets a scan match on its own.
          </p>
        )}
        {retypeRoot.isError && (
          <div className="banner warning">{String((retypeRoot.error as Error).message)}</div>
        )}
        <div className="form-row">
          <input
            type="text"
            placeholder="/absolute/path/to/media"
            value={newRoot}
            onChange={(e) => setNewRoot(e.target.value)}
          />
          <select value={newRootKind} onChange={(e) => setNewRootKind(e.target.value as RootKind)}>
            <option value="movie">Movies</option>
            <option value="series">TV series</option>
            <option value="book">Books</option>
            <option value="mixed">Mixed — ask</option>
          </select>
          <button
            className="btn-accent"
            disabled={newRoot.trim() === '' || addRoot.isPending}
            onClick={() => addRoot.mutate()}
          >
            Add root folder
          </button>
        </div>
        {addRoot.isError && <div className="banner warning">{String((addRoot.error as Error).message)}</div>}

        <h3 style={{ marginTop: 18 }}>Disk scan</h3>
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
                  <span className="match-as">
                    Match as{' '}
                    <Link to="/add" search={{ q: d.name, kind: 'movie' }}>
                      movie
                    </Link>{' '}
                    ·{' '}
                    <Link to="/add" search={{ q: d.name, kind: 'series' }}>
                      series
                    </Link>{' '}
                    ·{' '}
                    <Link to="/add" search={{ q: d.name, kind: 'book' }}>
                      book
                    </Link>
                  </span>
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

      <AcquisitionSettings />
      <CustomFormatSettings />
      <ImportListSettings />
      <NotifierSettings />
      <SecuritySettings />
    </>
  )
}
