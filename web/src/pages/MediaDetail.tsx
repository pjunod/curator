import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import {
  ACTIVE_DOWNLOAD_STATES,
  autoSearchItem,
  completeness,
  deleteLibraryItem,
  fmtBytes,
  fmtRatingValue,
  getLibraryItem,
  getProfiles,
  getQueue,
  getRootFolders,
  posterUrl,
  RATING_SOURCE_LABELS,
  refreshLibraryItem,
  updateLibraryItem,
} from '../api'
import type { MediaItemDetail } from '../api'
import { ReleaseSearch } from './ReleaseSearch'

// externalLinks builds the provider pages for an item — always new-tab.
function externalLinks(m: MediaItemDetail): { label: string; href: string }[] {
  const out: { label: string; href: string }[] = []
  if (m.ids.imdb) out.push({ label: 'IMDb', href: `https://www.imdb.com/title/${m.ids.imdb}/` })
  if (m.ids.tmdb) {
    const kind = m.kind === 'series' ? 'tv' : 'movie'
    if (m.kind !== 'book')
      out.push({ label: 'TMDB', href: `https://www.themoviedb.org/${kind}/${m.ids.tmdb}` })
  }
  if (m.ids.tvdb)
    out.push({ label: 'TVDB', href: `https://thetvdb.com/dereferrer/series/${m.ids.tvdb}` })
  if (m.ids.olid)
    out.push({ label: 'Open Library', href: `https://openlibrary.org/works/${m.ids.olid}` })
  return out
}

// EditPanel: per-item edits — monitoring, quality profile, and location.
function EditPanel(props: { item: MediaItemDetail; onClose: () => void }) {
  const { item } = props
  const qc = useQueryClient()
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })

  const [monitored, setMonitored] = useState(item.monitored)
  const [profileId, setProfileId] = useState(item.qualityProfileId)
  const [rootId, setRootId] = useState<number>(item.rootFolderId)
  const [path, setPath] = useState(item.path)

  const save = useMutation({
    mutationFn: () => {
      const req: Parameters<typeof updateLibraryItem>[1] = {
        monitored,
        qualityProfileId: profileId,
      }
      if (rootId !== item.rootFolderId) req.rootFolderId = rootId
      // An explicit path edit wins over the root-folder recompute.
      if (path !== item.path) req.path = path
      return updateLibraryItem(item.id, req)
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['library-item', String(item.id)] })
      void qc.invalidateQueries({ queryKey: ['library'] })
      props.onClose()
    },
  })

  return (
    <section className="panel">
      <h2>
        Edit
        <button style={{ marginLeft: 'auto' }} onClick={props.onClose}>
          Close
        </button>
      </h2>
      {save.isError && <div className="banner warning">{String((save.error as Error).message)}</div>}
      <div className="form-grid">
        <label>
          Monitored
          <input
            type="checkbox"
            checked={monitored}
            onChange={(e) => setMonitored(e.target.checked)}
          />
        </label>
        <label>
          Quality profile
          <select value={profileId} onChange={(e) => setProfileId(Number(e.target.value))}>
            {profiles.data?.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          Root folder
          <select
            value={rootId}
            onChange={(e) => {
              const id = Number(e.target.value)
              setRootId(id)
              // Recomputing happens server-side; clear the manual path so
              // the root choice takes effect unless the user retypes one.
              if (id !== item.rootFolderId) setPath('')
            }}
          >
            <option value={0}>(none)</option>
            {roots.data?.map((rf) => (
              <option key={rf.id} value={rf.id}>
                {rf.path}
              </option>
            ))}
          </select>
        </label>
        <label>
          Folder path
          <input
            type="text"
            placeholder={rootId !== item.rootFolderId ? '(recomputed from root folder)' : ''}
            value={path}
            onChange={(e) => setPath(e.target.value)}
          />
        </label>
      </div>
      <p className="muted">Files already on disk are never moved by an edit.</p>
      <div className="head-actions">
        <button className="btn-accent" disabled={save.isPending} onClick={() => save.mutate()}>
          Save
        </button>
        <button onClick={props.onClose}>Cancel</button>
      </div>
    </section>
  )
}

export function MediaDetailPage() {
  const { id } = useParams({ from: '/library/$id' })
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const [editing, setEditing] = useState(false)
  // Interactive search target: null = closed; {season?, episode?} = open.
  const [searching, setSearching] = useState<{ season?: number; episode?: number } | null>(null)

  const item = useQuery({
    queryKey: ['library-item', id],
    queryFn: () => getLibraryItem(Number(id)),
  })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const queue = useQuery({ queryKey: ['queue'], queryFn: getQueue, refetchInterval: 15_000 })

  const del = useMutation({
    mutationFn: () => deleteLibraryItem(Number(id)),
    onSuccess: () => navigate({ to: '/' }),
  })
  const [autoMsg, setAutoMsg] = useState('')
  const auto = useMutation({
    mutationFn: () => autoSearchItem(Number(id)),
    onSuccess: () => setAutoMsg('Searching in the background — grabs appear under Activity.'),
    onError: (e) => setAutoMsg(`✕ ${(e as Error).message}`),
  })
  const refresh = useMutation({
    mutationFn: () => refreshLibraryItem(Number(id)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['library-item', id] })
      void qc.invalidateQueries({ queryKey: ['library'] })
      setAutoMsg('Metadata refreshed from the provider.')
    },
    onError: (e) => setAutoMsg(`✕ ${(e as Error).message}`),
  })

  if (item.isLoading) return <p className="muted">Loading…</p>
  if (item.isError || !item.data) return <div className="banner warning">Item not found.</div>

  const m = item.data
  const fileCount = m.files.length
  const profileName = profiles.data?.find((p) => p.id === m.qualityProfileId)?.name
  const inFlight =
    queue.data?.filter(
      (q) => q.mediaItemId === m.id && ACTIVE_DOWNLOAD_STATES.includes(q.state),
    ).length ?? 0
  const links = externalLinks(m)

  // Completeness: for series count monitored episodes aired to date.
  const today = new Date().toISOString().slice(0, 10)
  let epAired = 0
  let epHave = 0
  for (const season of m.seasons) {
    for (const e of season.episodes) {
      if (e.monitored && e.airDate && e.airDate <= today) {
        epAired++
        if (e.hasFile) epHave++
      }
    }
  }
  const comp = completeness(m.kind, epHave, epAired, fileCount)

  return (
    <>
      <p style={{ marginTop: 0 }}>
        <Link to="/" className="muted">
          ← Library
        </Link>
      </p>
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
            {m.author && <span>by {m.author}</span>}
            {m.status && <span>{m.status}</span>}
            {m.runtime > 0 && <span>{m.runtime} min</span>}
            {m.genres.length > 0 && <span>{m.genres.join(', ')}</span>}
          </div>
          <p className="detail-overview">{m.overview}</p>

          <div className="fact-grid">
            <div className="fact-label">Location</div>
            <div>
              {m.path ? (
                <code className="path-chip" title="The folder this item's files live in (or will land in on import)">
                  {m.path}
                </code>
              ) : (
                <span className="muted">no folder assigned — set one via Edit</span>
              )}
            </div>

            <div className="fact-label">Status</div>
            <div className="fact-pills">
              <span className={`pill ${m.monitored ? 'pill-ok' : 'pill-neutral'}`}>
                {m.monitored ? 'monitored' : 'unmonitored'}
              </span>
              <span
                className={`pill ${comp.cls}`}
                title={
                  m.kind === 'series'
                    ? 'Monitored episodes aired to date that are on disk'
                    : 'Whether the item is on disk'
                }
              >
                {comp.total === 0 ? 'nothing aired yet' : `${comp.have}/${comp.total} on disk`}
              </span>
              {inFlight > 0 && (
                <Link to="/activity" className="pill pill-info" title="Downloads in flight for this item">
                  ↓ {inFlight} downloading
                </Link>
              )}
            </div>

            <div className="fact-label">Profile</div>
            <div>
              {profileName ?? `#${m.qualityProfileId}`}
              <span className="muted"> — quality target for grabs and upgrades</span>
            </div>

            {(m.ratings.length > 0 || m.ratingVotes > 0) && (
              <>
                <div className="fact-label">Ratings</div>
                <div className="fact-pills">
                  {(m.ratings.length > 0
                    ? m.ratings
                    : [{ source: m.kind === 'book' ? 'openlibrary' : 'tmdb', value: m.rating, votes: m.ratingVotes, scale: m.kind === 'book' ? 5 : 10 }]
                  ).map((r) => (
                    <span
                      key={r.source}
                      className="rating-chip"
                      title={r.votes ? `${r.votes.toLocaleString()} votes` : undefined}
                    >
                      <span className="rating-source">{RATING_SOURCE_LABELS[r.source] ?? r.source}</span>{' '}
                      ★ {fmtRatingValue(r)}
                      {r.votes ? <span className="muted"> ({r.votes.toLocaleString()})</span> : null}
                    </span>
                  ))}
                </div>
              </>
            )}

            {(links.length > 0 || m.ids.isbn13) && (
              <>
                <div className="fact-label">Links</div>
                <div className="fact-pills">
                  {links.map((l) => (
                    <a key={l.label} href={l.href} target="_blank" rel="noreferrer">
                      {l.label} ↗
                    </a>
                  ))}
                  {m.ids.isbn13 && <span className="muted mono">ISBN {m.ids.isbn13}</span>}
                </div>
              </>
            )}
          </div>

          <div className="detail-actions">
            <button
              className="btn-accent"
              title="Search all indexers and grab the best accepted release automatically"
              disabled={auto.isPending}
              onClick={() => auto.mutate()}
            >
              Auto search
            </button>
            {(m.kind === 'movie' || m.kind === 'book') && (
              <button onClick={() => setSearching({})}>Interactive search</button>
            )}
            <button onClick={() => setEditing(true)}>Edit</button>
            <button
              title="Re-fetch metadata from the provider (new episodes, poster, rating, …)"
              disabled={refresh.isPending}
              onClick={() => refresh.mutate()}
            >
              {refresh.isPending ? 'Refreshing…' : 'Refresh metadata'}
            </button>
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

      {autoMsg && <div className="banner">{autoMsg}</div>}

      {editing && <EditPanel item={m} onClose={() => setEditing(false)} />}

      {searching && (
        <ReleaseSearch
          mediaItemId={m.id}
          season={searching.season}
          episode={searching.episode}
          onClose={() => setSearching(null)}
        />
      )}

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
                  <button
                    className="summary-action"
                    onClick={(e) => {
                      e.preventDefault()
                      setSearching({ season: s.number })
                    }}
                  >
                    Search pack
                  </button>
                </summary>
                <table>
                  <thead>
                    <tr>
                      <th>#</th>
                      <th>Title</th>
                      <th>Air date</th>
                      <th>File</th>
                      <th></th>
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
                        <td>
                          <button
                            onClick={() => setSearching({ season: e.seasonNumber, episode: e.episodeNumber })}
                          >
                            Search
                          </button>
                        </td>
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
