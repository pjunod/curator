import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate, useParams } from '@tanstack/react-router'
import { deleteLibraryItem, fmtBytes, getLibraryItem, posterUrl } from '../api'

export function MediaDetailPage() {
  const { id } = useParams({ from: '/library/$id' })
  const navigate = useNavigate()
  const [confirming, setConfirming] = useState(false)

  const item = useQuery({
    queryKey: ['library-item', id],
    queryFn: () => getLibraryItem(Number(id)),
  })

  const del = useMutation({
    mutationFn: () => deleteLibraryItem(Number(id)),
    onSuccess: () => navigate({ to: '/' }),
  })

  if (item.isLoading) return <p className="muted">Loading…</p>
  if (item.isError || !item.data) return <div className="banner warning">Item not found.</div>

  const m = item.data
  const fileCount = m.files.length

  return (
    <>
      <div className="detail-head">
        {m.posterPath ? (
          <img className="detail-poster" src={posterUrl(m.posterPath)} alt="" />
        ) : (
          <div className="poster-fallback detail-poster">{m.title.slice(0, 1)}</div>
        )}
        <div className="detail-info">
          <h1>
            {m.title} <span className="muted">({m.year || '—'})</span>
          </h1>
          <div className="detail-facts muted">
            <span className="pill pill-neutral">{m.kind}</span>
            {m.status && <span>{m.status}</span>}
            {m.runtime > 0 && <span>{m.runtime} min</span>}
            {m.genres.length > 0 && <span>{m.genres.join(', ')}</span>}
          </div>
          <p className="detail-overview">{m.overview}</p>
          <div className="muted detail-facts">
            {m.path ? <span className="mono">{m.path}</span> : <span>no folder assigned</span>}
            <span>{m.monitored ? 'monitored' : 'unmonitored'}</span>
            <span>
              {fileCount} file{fileCount === 1 ? '' : 's'}
            </span>
            {m.ids.imdb && <span className="mono">{m.ids.imdb}</span>}
          </div>
          <div className="detail-actions">
            {!confirming ? (
              <button onClick={() => setConfirming(true)}>Remove from library</button>
            ) : (
              <>
                <button className="btn-danger" onClick={() => del.mutate()} disabled={del.isPending}>
                  Confirm remove (files on disk are kept)
                </button>
                <button onClick={() => setConfirming(false)}>Cancel</button>
              </>
            )}
          </div>
        </div>
      </div>

      {m.kind === 'series' && (
        <section className="panel">
          <h2>Seasons</h2>
          {m.seasons.map((s) => {
            const have = s.episodes.filter((e) => e.hasFile).length
            return (
              <details key={s.number} open={s.number === 1}>
                <summary>
                  {s.number === 0 ? 'Specials' : `Season ${s.number}`}{' '}
                  <span className="muted">
                    {have}/{s.episodes.length} on disk{s.monitored ? '' : ' · unmonitored'}
                  </span>
                </summary>
                <table>
                  <thead>
                    <tr>
                      <th>#</th>
                      <th>Title</th>
                      <th>Air date</th>
                      <th>File</th>
                    </tr>
                  </thead>
                  <tbody>
                    {s.episodes.map((e) => (
                      <tr key={e.id}>
                        <td className="mono">
                          {e.seasonNumber}x{String(e.episodeNumber).padStart(2, '0')}
                        </td>
                        <td>{e.title || <span className="muted">TBA</span>}</td>
                        <td className="muted">{e.airDate || '—'}</td>
                        <td>{e.hasFile ? <span className="pill pill-ok">✓</span> : <span className="muted">—</span>}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </details>
            )
          })}
        </section>
      )}

      <section className="panel">
        <h2>Files</h2>
        {m.files.length === 0 ? (
          <p className="muted">
            No files on disk yet. Run a disk scan from the Library page once the folder has
            content.
          </p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Path</th>
                <th>Size</th>
                <th>Episodes</th>
              </tr>
            </thead>
            <tbody>
              {m.files.map((f) => (
                <tr key={f.id}>
                  <td className="mono">{f.path}</td>
                  <td className="muted">{fmtBytes(f.size)}</td>
                  <td className="muted">{f.episodeIds.length || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  )
}
