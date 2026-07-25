import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import {
  ACTIVE_DOWNLOAD_STATES,
  addMediaCopy,
  autoSearchItem,
  completeness,
  deleteLibraryItem,
  deleteMediaCopy,
  fmtBytes,
  fmtRatingValue,
  getLibraryItem,
  getProfiles,
  getQueue,
  getRootFolders,
  posterUrl,
  RATING_SOURCE_LABELS,
  refreshLibraryItem,
  setEpisodeMonitored,
  setSeasonMonitored,
  updateLibraryItem,
  updateMediaCopy,
} from '../api'
import type { MediaItemDetail } from '../api'
import { ReleaseSearch } from './ReleaseSearch'

// CopiesPanel: additional quality targets — the same item kept at a second
// quality, each copy with its own profile, location, and automation.
function CopiesPanel(props: { item: MediaItemDetail }) {
  const { item } = props
  const qc = useQueryClient()
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })

  const [profileId, setProfileId] = useState<number | ''>('')
  const [rootId, setRootId] = useState(0)
  const [name, setName] = useState('')
  const [msg, setMsg] = useState('')

  const refreshCache = (detail: MediaItemDetail) => {
    qc.setQueryData(['library-item', String(item.id)], detail)
    void qc.invalidateQueries({ queryKey: ['wanted'] })
  }
  const add = useMutation({
    mutationFn: () =>
      addMediaCopy(item.id, {
        qualityProfileId: Number(profileId),
        rootFolderId: rootId || undefined,
        name: name.trim() || undefined,
      }),
    onSuccess: (detail) => {
      refreshCache(detail)
      setProfileId('')
      setName('')
      setMsg('Copy added — the loops hunt it like any other wanted item.')
    },
    onError: (e) => setMsg(`✕ ${(e as Error).message}`),
  })
  const toggle = useMutation({
    mutationFn: (v: { copyId: number; monitored: boolean }) =>
      updateMediaCopy(item.id, v.copyId, { monitored: v.monitored }),
    onSuccess: refreshCache,
  })
  const del = useMutation({
    mutationFn: (copyId: number) => deleteMediaCopy(item.id, copyId),
    onSuccess: (detail) => {
      refreshCache(detail)
      setMsg('Copy removed — its files on disk were kept.')
    },
  })

  const profileName = (id: number) => profiles.data?.find((p) => p.id === id)?.name ?? `#${id}`
  const copyFiles = (copyId: number) => item.files.filter((f) => (f.copyId ?? 0) === copyId).length

  return (
    <section className="panel" id="copies">
      <h2>Quality copies</h2>
      <p className="muted">
        Keep this {item.kind} at more than one quality — e.g. the main copy at 4K plus a 720p
        copy for someone else. Each copy has its own profile and is hunted, imported, and
        upgraded independently. A copy either shares this folder (filenames carry the quality)
        or lives in its own folder under a different root.
      </p>

      {item.copies.length > 0 && (
        <table>
          <thead>
            <tr>
              <th></th>
              <th>Copy</th>
              <th>Profile</th>
              <th>Location</th>
              <th>Files</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {item.copies.map((c) => (
              <tr key={c.id} className={c.monitored ? '' : 'row-unmonitored'}>
                <td>
                  <input
                    type="checkbox"
                    className="monitor-box"
                    aria-label={`Monitor copy ${c.name || c.id}`}
                    checked={c.monitored}
                    disabled={toggle.isPending}
                    onChange={(e) => toggle.mutate({ copyId: c.id, monitored: e.target.checked })}
                  />
                </td>
                <td>{c.name || <span className="muted">unnamed</span>}</td>
                <td>{profileName(c.qualityProfileId)}</td>
                <td>
                  {c.path ? (
                    <code className="path-chip">{c.path}</code>
                  ) : (
                    <span className="muted">same folder as the main copy</span>
                  )}
                </td>
                <td className="muted">{copyFiles(c.id)}</td>
                <td>
                  <button onClick={() => del.mutate(c.id)} disabled={del.isPending}>
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="form-row">
        <select
          aria-label="Copy quality profile"
          value={profileId}
          onChange={(e) => setProfileId(e.target.value ? Number(e.target.value) : '')}
        >
          <option value="">(profile…)</option>
          {profiles.data?.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
        <select
          aria-label="Copy location"
          value={rootId}
          onChange={(e) => setRootId(Number(e.target.value))}
        >
          <option value={0}>Same folder as the main copy</option>
          {roots.data?.map((rf) => (
            <option key={rf.id} value={rf.id}>
              Own folder under {rf.path}
            </option>
          ))}
        </select>
        <input
          placeholder="Name (optional, e.g. 720p for dad)"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button
          className="btn-accent"
          disabled={profileId === '' || add.isPending}
          onClick={() => add.mutate()}
        >
          Add copy
        </button>
      </div>
      {msg && <p className={msg.startsWith('✕') ? 'error-text' : 'ok-text'}>{msg}</p>}
    </section>
  )
}

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

// QualityFacts: what is on disk, what the profile is aiming at, and whether
// anything more is being sought — one line, each number labelled.
//
// Labelling is the whole design. The first version read
// "Remux 2160p / at the target (WEB-DL 1080p)", which puts two resolutions
// next to each other with nothing saying which is which, and the parenthetical
// reads as a correction: it looked like a 2160p file being described as 1080p.
// "on disk" and "target" in front of each pill costs two words and removes the
// ambiguity entirely.
//
// The state word never restates a number, for the same reason.
function QualityFacts({ m }: { m: MediaItemDetail }) {
  const target = m.qualityTarget
  const state: Record<string, { word: string; cls: string; why: string }> = {
    met: {
      word: 'target met',
      cls: 'qf-met',
      // "met" covers at-or-above: a 2160p Remux against a 1080p cutoff is
      // finished, and saying "at the target" about it was the confusing part.
      why: `What is on disk is at or above ${target || 'the target'}, so nothing better will be sought.`,
    },
    seeking: {
      word: 'upgrading',
      cls: 'qf-seeking',
      why: `Below ${target || 'the target'}, and the profile allows upgrades — monarr is still looking for better.`,
    },
    capped: {
      word: 'upgrades off',
      cls: 'qf-capped',
      why: `Below ${target || 'the target'} and staying there: this profile has upgrades switched off.`,
    },
  }
  const s = m.upgrade ? state[m.upgrade] : undefined

  return (
    <div className="quality-facts">
      {m.upgrade === 'missing' || !m.quality ? (
        <span className="muted">nothing on disk yet</span>
      ) : (
        <span className="qf-pair">
          <span className="qf-label">on disk</span>
          <span
            className="pill pill-neutral"
            title="The weakest quality among this item's files — the one that decides whether it is still being hunted"
          >
            {m.quality}
          </span>
        </span>
      )}

      {target && (
        <span className="qf-pair">
          <span className="qf-label">target</span>
          <span className="pill pill-outline" title="The profile's cutoff: the point at which monarr stops looking for better">
            {target}
          </span>
        </span>
      )}

      {s && (
        <span className={`qf-state ${s.cls}`} title={s.why}>
          {s.word}
        </span>
      )}
    </div>
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

  // Granular monitoring: the API returns the updated item — write it
  // straight into the cache so checkboxes feel instant.
  const monitorSeason = useMutation({
    mutationFn: (v: { season: number; monitored: boolean }) =>
      setSeasonMonitored(Number(id), v.season, v.monitored),
    onSuccess: (detail) => {
      qc.setQueryData(['library-item', id], detail)
      void qc.invalidateQueries({ queryKey: ['wanted'] })
    },
  })
  const monitorEpisode = useMutation({
    mutationFn: (v: { episodeId: number; monitored: boolean }) =>
      setEpisodeMonitored(Number(id), v.episodeId, v.monitored),
    onSuccess: (detail) => {
      qc.setQueryData(['library-item', id], detail)
      void qc.invalidateQueries({ queryKey: ['wanted'] })
    },
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

            <div className="fact-label">Quality</div>
            <QualityFacts m={m} />

            <div className="fact-label">Profile</div>
            <div>
              {profileName ?? `#${m.qualityProfileId}`}
              {/* The cutoff is on the Quality row above as a concrete number,
                  so this no longer needs to explain what a target is — what
                  is left is the part the pill cannot show. */}
              <span className="muted"> — which qualities are allowed, and whether upgrades run</span>
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

      {m.kind !== 'book' && <CopiesPanel item={m} />}

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
                  <input
                    type="checkbox"
                    className="monitor-box"
                    title={s.monitored ? 'Monitored — untick to stop wanting this season' : 'Unmonitored — tick to want this season'}
                    aria-label={`Monitor season ${s.number}`}
                    checked={s.monitored}
                    disabled={monitorSeason.isPending}
                    onClick={(e) => e.stopPropagation()}
                    onChange={(e) =>
                      monitorSeason.mutate({ season: s.number, monitored: e.target.checked })
                    }
                  />{' '}
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
                      <th></th>
                      <th>#</th>
                      <th>Title</th>
                      <th>Air date</th>
                      <th>File</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    {s.episodes.map((e) => (
                      <tr key={e.id} className={e.monitored ? '' : 'row-unmonitored'}>
                        <td>
                          <input
                            type="checkbox"
                            className="monitor-box"
                            title={e.monitored ? 'Monitored' : 'Unmonitored'}
                            aria-label={`Monitor episode ${e.seasonNumber}x${e.episodeNumber}`}
                            checked={e.monitored}
                            disabled={monitorEpisode.isPending}
                            onChange={(ev) =>
                              monitorEpisode.mutate({ episodeId: e.id, monitored: ev.target.checked })
                            }
                          />
                        </td>
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
                {m.copies.length > 0 && <th>Copy</th>}
              </tr>
            </thead>
            <tbody>
              {m.files.map((f) => (
                <tr key={f.id}>
                  <td className="mono">{f.path}</td>
                  <td className="muted">{fmtBytes(f.size)}</td>
                  <td className="muted">{f.episodeIds.length || '—'}</td>
                  {m.copies.length > 0 && (
                    <td className="muted">
                      {f.copyId
                        ? (m.copies.find((c) => c.id === f.copyId)?.name || `copy ${f.copyId}`)
                        : 'main'}
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  )
}
