import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { RootKind } from '../api'
import {
  addRootFolder,
  deleteLibraryItem,
  deleteRootFolder,
  fmtBytes,
  fmtRelative,
  getIgnoredDirs,
  getRootFolders,
  getReviewQueue,
  getScanReport,
  getSettings,
  runAdoption,
  setRootAutoAdopt,
  triggerScan,
  unignoreDir,
  updateRootFolderKind,
  updateSettings,
} from '../api'
import { AcquisitionSettings } from './SettingsAcquisition'
import { QualityProfileSettings } from './SettingsProfiles'
import { CustomFormatSettings, ImportListSettings } from './SettingsDepth'
import { NotifierSettings } from './SettingsNotifiers'
import { PathInput } from '../PathInput'
import { ReviewWindow } from '../ReviewWindow'

export function SettingsPage() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const report = useQuery({ queryKey: ['scan-report'], queryFn: getScanReport })
  // The button's number comes from the queue the window will actually show,
  // not from the scan report. They are different facts with different update
  // triggers, and a button reading one to describe the other said "Review 4
  // folders…" over a window listing one. Asking for a single row is enough:
  // the counts are computed over the whole queue.
  const reviewCount = useQuery({
    queryKey: ['review', 'count'],
    queryFn: () => getReviewQueue(undefined, '', 1, 0),
    select: (page) => page.counts.total,
  })
  const ignored = useQuery({ queryKey: ['ignored-dirs'], queryFn: getIgnoredDirs })

  const [key, setKey] = useState('')
  const [omdbKey, setOmdbKey] = useState('')
  const [traktId, setTraktId] = useState('')
  const [newRoot, setNewRoot] = useState('')
  const [newRootKind, setNewRootKind] = useState<RootKind>('movie')
  const [skipPatterns, setSkipPatterns] = useState<string | null>(null)
  const [retentionDays, setRetentionDays] = useState<number | null>(null)
  const [adoptSummary, setAdoptSummary] = useState<string | null>(null)
  const [reviewOpen, setReviewOpen] = useState(false)

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
  const saveTraktId = useMutation({
    mutationFn: () => updateSettings({ traktClientId: traktId }),
    onSuccess: () => {
      setTraktId('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
      // The catalogue gains five rows the moment this lands, and Discover
      // has no reason to refetch it on its own.
      void qc.invalidateQueries({ queryKey: ['discover-lists'] })
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
  const restore = useMutation({
    mutationFn: (path: string) => unignoreDir(path),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['ignored-dirs'] })
      void qc.invalidateQueries({ queryKey: ['review'] })
    },
  })
  const saveSkips = useMutation({
    mutationFn: () => updateSettings({ scanSkipPatterns: skipPatterns ?? '' }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['settings'] }),
  })
  const saveRetention = useMutation({
    mutationFn: (days: number) => updateSettings({ activityRetentionDays: days }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['settings'] }),
  })
  const adopt = useMutation({
    mutationFn: runAdoption,
    onSuccess: (res) => {
      setAdoptSummary(
        `${res.adopted.length} adopted · ${res.review.length} need review` +
          (res.failures.length ? ` · ${res.failures.length} failed` : ''),
      )
      void qc.invalidateQueries({ queryKey: ['scan-report'] })
      void qc.invalidateQueries({ queryKey: ['review'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
  })
  const forgetItem = useMutation({
    mutationFn: deleteLibraryItem,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['scan-report'] })
      void qc.invalidateQueries({ queryKey: ['review'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
  })
  const autoAdopt = useMutation({
    mutationFn: ({ id, on }: { id: number; on: boolean }) => setRootAutoAdopt(id, on),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['rootfolders'] }),
  })
  const scan = useMutation({
    mutationFn: triggerScan,
    onSuccess: () => {
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: ['scan-report'] })
        void qc.invalidateQueries({ queryKey: ['review'] })
      }, 1500)
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

        <h2 style={{ marginTop: 18 }}>Trakt (optional)</h2>
        <p className="muted">
          A free Trakt app client id adds five rows to <Link to="/discover">Discover</Link> that
          TMDB cannot answer: what is being played right now (Trakt counts actual playback,
          not lookups), what is most anticipated, and last weekend's box office. Create an app
          at trakt.tv/oauth/applications and paste its client id — no account link, no OAuth.
          Import lists keep their own per-list client id and are unaffected.
        </p>
        <div className="form-row">
          <input
            type="password"
            placeholder={
              settings.data?.traktClientIdConfigured
                ? `Configured (${settings.data.traktClientIdHint}) — paste to replace`
                : 'Paste your Trakt client id'
            }
            value={traktId}
            onChange={(e) => setTraktId(e.target.value)}
          />
          <button
            className="btn-accent"
            disabled={traktId.trim() === '' || saveTraktId.isPending}
            onClick={() => saveTraktId.mutate()}
          >
            Save Trakt client id
          </button>
        </div>
        {saveTraktId.isSuccess && <p className="ok-text">Saved.</p>}
        {saveTraktId.isError && <div className="banner warning">{String((saveTraktId.error as Error).message)}</div>}
      </section>

      <section className="panel" id="library-folders">
        <h2>Library folders</h2>
        <p className="muted">
          Root folders are the only places Curator looks. A scan reconciles what is on disk
          against the library, and anything it finds that no item claims is listed below as a
          candidate to match.
        </p>
        <table>
          <thead>
            <tr>
              <th>Path</th>
              <th>Holds</th>
              <th>Adoption</th>
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
                <td>
                  {rf.kind === 'mixed' ? (
                    <span className="muted" title="A mixed root always asks — set a kind first">
                      always asks
                    </span>
                  ) : rf.autoAdopt ? (
                    <>
                      <span className="pill pill-ok">auto</span>{' '}
                      <button
                        className="link-btn"
                        disabled={autoAdopt.isPending}
                        title="Go back to proposing matches for review"
                        onClick={() => autoAdopt.mutate({ id: rf.id, on: false })}
                      >
                        undo
                      </button>
                    </>
                  ) : (
                    <button
                      disabled={autoAdopt.isPending}
                      title="Let later scans adopt confident matches into this root without asking"
                      onClick={() => autoAdopt.mutate({ id: rf.id, on: true })}
                    >
                      Confirm
                    </button>
                  )}
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
                <td colSpan={6} className="muted">
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
          <PathInput value={newRoot} onChange={setNewRoot} />
          <select
            aria-label="What the new root folder holds"
            value={newRootKind}
            onChange={(e) => setNewRootKind(e.target.value as RootKind)}
          >
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
          <button onClick={() => adopt.mutate()} disabled={adopt.isPending}>
            {adopt.isPending ? 'Matching…' : 'Match unmatched folders'}
          </button>
          {adoptSummary && <span className="ok-text">{adoptSummary}</span>}
          {adopt.isError && (
            <span className="error-text">{String((adopt.error as Error).message)}</span>
          )}
          {report.data && (
            <span className="muted">
              Last scan {fmtRelative(report.data.scannedAt)} · {report.data.itemsScanned} items ·{' '}
              {report.data.filesLinked} files linked · {report.data.filesRemoved} removed
            </span>
          )}
          {report.data === null && <span className="muted">No scan has run yet.</span>}
        </div>

        {(reviewCount.data ?? 0) > 0 && (
          <div className="form-row">
            <button className="btn-accent" onClick={() => setReviewOpen(true)}>
              Review {reviewCount.data} folders…
            </button>
            <span className="muted">
              Opens in a window with pages, so a folder of several hundred movies does not turn
              this page into a mile of rows.
            </span>
          </div>
        )}
        {report.data && (report.data.skippedDirs || report.data.ignoredDirs) ? (
          <p className="muted">
            Not offered: {report.data.skippedDirs ?? 0} skipped by pattern ·{' '}
            {report.data.ignoredDirs ?? 0} dismissed. Counted rather than listed, so an
            exclusion is never silent.
          </p>
        ) : null}
        {report.data && report.data.missingPaths.length > 0 && (
          <details>
            <summary className="muted">
              Items whose folder is missing on disk ({report.data.missingPaths.length})
            </summary>
            <p className="muted">
              Usually an item added by title that was never attached to a folder — the path
              below is the one the naming rules would have used, not a folder that exists.
              Removing the entry leaves any files alone.
            </p>
            <ul className="unmatched-list">
              {(report.data.missingItems ?? []).slice(0, 100).map((m) => (
                <li key={m.id}>
                  <span className="mono">{m.path}</span>
                  <span className="match-as">
                    <strong>{m.title}</strong>{' · '}
                    <button
                      className="link-btn"
                      disabled={forgetItem.isPending}
                      title="Remove this library entry (files on disk are untouched)"
                      onClick={() => forgetItem.mutate(m.id)}
                    >
                      remove entry
                    </button>
                  </span>
                </li>
              ))}
              {/* Reports written before missingItems existed only carry paths. */}
              {(report.data.missingItems ?? []).length === 0 &&
                report.data.missingPaths.slice(0, 100).map((p) => (
                  <li key={p}>
                    <span className="mono">{p}</span>
                  </li>
                ))}
            </ul>
            {report.data.missingPaths.length > 100 && (
              <p className="muted">
                Showing the first 100 of {report.data.missingPaths.length}.
              </p>
            )}
            {forgetItem.isError && (
              <div className="banner warning">
                {String((forgetItem.error as Error).message)}
              </div>
            )}
          </details>
        )}

        {ignored.data && ignored.data.length > 0 && (
          <details>
            <summary className="muted">Dismissed folders ({ignored.data.length})</summary>
            <ul className="unmatched-list">
              {ignored.data.slice(0, 100).map((ip) => (
                <li key={ip.path}>
                  <span className="mono">{ip.path}</span>
                  <span className="match-as">
                    <button
                      className="link-btn"
                      disabled={restore.isPending}
                      onClick={() => restore.mutate(ip.path)}
                    >
                      offer it again
                    </button>
                  </span>
                </li>
              ))}
            </ul>
          </details>
        )}

        <h3 style={{ marginTop: 18 }}>Skip patterns</h3>
        <p className="muted">
          Folder names never offered as candidates, one per line. Dot-directories and the usual
          NAS clutter (@eaDir, #recycle, lost+found, extras, samples) are built in — these are
          on top of those. Glob syntax works: <span className="mono">_incoming</span>,{' '}
          <span className="mono">*.tmp</span>.
        </p>
        <textarea
          rows={4}
          className="mono"
          placeholder={'_incoming\n*.partial'}
          value={skipPatterns ?? settings.data?.scanSkipPatterns ?? ''}
          onChange={(e) => setSkipPatterns(e.target.value)}
        />
        <div className="form-row">
          <button
            disabled={skipPatterns === null || saveSkips.isPending}
            onClick={() => saveSkips.mutate()}
          >
            Save skip patterns
          </button>
          {saveSkips.isSuccess && <span className="ok-text">Saved — applies on the next scan.</span>}
        </div>

        <h3 style={{ marginTop: 18 }}>Activity retention</h3>
        <p className="muted">
          How long finished and failed downloads, and the events behind them, are kept before
          the daily sweep ages them out. Anything still moving is never touched, however old it
          looks — a download stuck for six weeks is something to look at, not something to hide.
          Set 0 to keep everything.
        </p>
        <div className="form-row">
          <input
            type="number"
            min={0}
            style={{ width: 110 }}
            aria-label="Activity retention in days"
            value={retentionDays ?? settings.data?.activityRetentionDays ?? 30}
            onChange={(e) => setRetentionDays(Number(e.target.value))}
          />
          <span className="muted">days</span>
          <button
            disabled={retentionDays === null || saveRetention.isPending}
            onClick={() => saveRetention.mutate(retentionDays ?? 30)}
          >
            Save retention
          </button>
          {saveRetention.isSuccess && <span className="ok-text">Saved.</span>}
        </div>
      </section>

      <QualityProfileSettings />

      <AcquisitionSettings />
      <CustomFormatSettings />
      <ImportListSettings />
      <NotifierSettings />

      {reviewOpen && <ReviewWindow onClose={() => setReviewOpen(false)} />}
    </>
  )
}
