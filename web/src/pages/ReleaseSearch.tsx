import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ReleaseCandidate } from '../api'
import { fmtBytes, grabRelease, searchReleases } from '../api'

// ReleaseSearch is the interactive search panel: every candidate is shown —
// rejected ones carry their reasons instead of being hidden.
export function ReleaseSearch(props: {
  mediaItemId: number
  season?: number
  episode?: number
  onClose: () => void
}) {
  const { mediaItemId, season, episode } = props
  const [grabbed, setGrabbed] = useState<string | null>(null)

  const search = useQuery({
    queryKey: ['releases', mediaItemId, season ?? -1, episode ?? -1],
    queryFn: () => searchReleases(mediaItemId, season, episode),
    retry: false,
    staleTime: 60_000,
  })

  const grab = useMutation({
    mutationFn: (c: ReleaseCandidate) =>
      grabRelease({
        mediaItemId, season, episode,
        title: c.title, downloadUrl: c.downloadUrl, indexer: c.indexer,
        protocol: c.protocol, size: c.size,
      }),
    onSuccess: (_res, c) => setGrabbed(c.title),
  })

  const label =
    season !== undefined && episode !== undefined
      ? `S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`
      : season !== undefined
        ? `Season ${season} pack`
        : 'movie'

  return (
    <section className="panel">
      <h2>
        Interactive search — {label}
        <button style={{ marginLeft: 'auto' }} onClick={props.onClose}>
          Close
        </button>
      </h2>

      {search.isFetching && <p className="muted">Searching indexers…</p>}
      {search.isError && <div className="banner warning">{String((search.error as Error).message)}</div>}
      {grab.isError && <div className="banner warning">{String((grab.error as Error).message)}</div>}
      {grabbed && (
        <div className="banner">
          Grabbed <span className="mono">{grabbed}</span> — follow it on the{' '}
          <Link to="/activity">Activity page</Link>.
        </div>
      )}

      {search.data && search.data.length === 0 && (
        <p className="muted">No releases found on any enabled indexer.</p>
      )}
      {search.data && search.data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Release</th>
              <th>Quality</th>
              <th>Size</th>
              <th>Age</th>
              <th>Seeders</th>
              <th>Indexer</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {search.data.map((c) => (
              <tr key={`${c.indexer}-${c.title}`} className={c.accepted ? '' : 'row-rejected'}>
                <td className="mono">
                  {c.title}
                  {c.rejections.map((r) => (
                    <div key={r.code} className="error-text">
                      ✕ {r.reason}
                    </div>
                  ))}
                  {c.isUpgrade && <div className="ok-text">upgrade</div>}
                </td>
                <td className="muted">
                  {c.quality}
                  {c.score !== 0 && (
                    <div title={(c.formats ?? []).join(', ')}>score {c.score > 0 ? '+' : ''}{c.score}</div>
                  )}
                </td>
                <td className="muted">{fmtBytes(c.size)}</td>
                <td className="muted">{c.age || '—'}</td>
                <td className="muted">{c.protocol === 'torrent' ? c.seeders : '—'}</td>
                <td className="muted">{c.indexer}</td>
                <td>
                  <button
                    className={c.accepted ? 'btn-accent' : ''}
                    disabled={grab.isPending}
                    onClick={() => grab.mutate(c)}
                    title={c.accepted ? 'Send to download client' : 'Grab anyway (override rejections)'}
                  >
                    Grab
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
