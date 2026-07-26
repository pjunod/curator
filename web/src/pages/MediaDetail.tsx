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
  reprobeLibraryItem,
  rescanManualEntry,
  setEpisodeMonitored,
  setSeasonMonitored,
  updateLibraryItem,
  updateMediaCopy,
} from '../api'
import type { MediaFileInfo, MediaItemDetail } from '../api'
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
function EditPanel(props: {
  item: MediaItemDetail
  onClose: () => void
  onSaved?: (message: string) => void
}) {
  const { item } = props
  const qc = useQueryClient()
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })

  const [monitored, setMonitored] = useState(item.monitored)
  const [profileId, setProfileId] = useState(item.qualityProfileId)
  const [rootId, setRootId] = useState<number>(item.rootFolderId)
  const [path, setPath] = useState(item.path)

  const profileChanged = profileId !== item.qualityProfileId
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
      void qc.invalidateQueries({ queryKey: ['wanted'] })
      // A profile change starts a search server-side. Saying so is the whole
      // difference between "monarr is working on it" and "nothing happened".
      if (profileChanged && monitored) {
        props.onSaved?.('Profile changed — searching for the new target in the background.')
      }
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
          <select
            aria-label="Quality profile"
            value={profileId}
            onChange={(e) => setProfileId(Number(e.target.value))}
          >
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

// QualityFacts: what is on disk, how we know, and whether anything more is
// being sought.
//
// The target deliberately is NOT here. Shown on this row it was an orphan
// number — "TARGET 1080p" next to a profile named "Any" reads as arriving
// from nowhere, and the honest question it prompts is "where did that come
// from?". It sits on the Profile row, beside the thing that sets it, so the
// number appears exactly once and next to its source.
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
  // Whether the SOURCE half of what is on disk was measured or merely guessed
  // changes what "met" means, so it changes what this row says (ADR 0013).
  const unverified = m.quality ? m.qualityVerified === false : false
  const state: Record<string, { word: string; cls: string; why: string }> = {
    met: {
      word: unverified
        ? `at target (source unverified)`
        : `at or above ${target || 'the target'}`,
      cls: 'qf-met',
      why: unverified
        ? `The resolution on disk matches ${target || 'the target'}, but monarr could only guess at the source. Rather than replace a file that may already be perfect on a guess, it stops here. Interactive search still lets you grab anything you like.`
        : `What is on disk is at or above ${target || 'the target'}, so nothing better will be sought.`,
    },
    seeking: {
      word: `upgrading to ${target || 'better'}`,
      cls: 'qf-seeking',
      why: `Below ${target || 'the target'}, and the profile allows upgrades — monarr is still looking for better.`,
    },
    capped: {
      word: `below ${target || 'the target'} · upgrades off`,
      cls: 'qf-capped',
      why: `Below ${target || 'the target'} and staying there: this profile has upgrades switched off.`,
    },
  }
  const s = m.upgrade ? state[m.upgrade] : undefined

  // "Nothing on disk" is a fact about FILES. Reading it off an empty quality
  // string made this row contradict the Files table two panels down: an
  // adopted file whose name carries no quality tag has nothing recorded, and
  // that is not the same as not existing.
  if (m.upgrade === 'missing') {
    return (
      <div className="quality-facts">
        <span className="muted">nothing on disk yet</span>
      </div>
    )
  }

  // The measured facts for the primary copy's files, deduped: this is what
  // replaced "quality not recorded — the filename does not say".
  const facts = Array.from(
    new Set(m.files.filter((f) => !f.copyId && f.facts).map((f) => f.facts as string)),
  )

  return (
    <div className="quality-facts">
      <span className="qf-pair">
        <span className="qf-label">on disk</span>
        {m.quality ? (
          <span
            className="pill pill-neutral"
            title="The weakest quality among this item's files — the one that decides whether it is still being hunted"
          >
            {m.quality}
          </span>
        ) : (
          <span
            className="muted"
            title="The file is on disk; monarr could not read it. That is not the same as the file being missing, and nothing here guesses."
          >
            on disk — monarr could not read this file
          </span>
        )}
      </span>

      {facts.length > 0 && (
        <span className="qf-facts" title="Measured from the file itself, not from its name">
          {facts.join('  ·  ')}
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

// ProvenanceBadge says where a file's recorded quality came from. A quality
// with no provenance is exactly the ambiguity ADR 0013 set out to remove, so
// every file row carries one.
function ProvenanceBadge({ f }: { f: MediaFileInfo }) {
  if (!f.provenanceLabel) return <span className="muted">—</span>
  const titles: Record<string, string> = {
    probe: 'Measured from the file itself.',
    filename: "Taken from the file's name, which nothing measured contradicted.",
    release: "Taken from the grabbed release's name, which nothing measured contradicted.",
    manual: 'Set by hand.',
    failed: 'monarr could not read this file. It is on disk; its quality is unknown.',
  }
  const cls = f.provenance === 'probe' ? 'prov-measured' : f.provenance === 'failed' ? 'prov-failed' : 'prov-claimed'
  return (
    <span className={`prov-badge ${cls}`} title={titles[f.provenance ?? ''] ?? ''}>
      {f.provenanceLabel}
    </span>
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
  // Season disclosure state. Season 1 opens by default; anything the user
  // toggles is remembered for the life of the page.
  const [openSeasons, setOpenSeasons] = useState<Record<number, boolean>>({})

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
  // Re-measure the files. The scan's already-probed cache is right for a
  // routine sweep, but it leaves no way back for a file whose probe failed
  // for a reason outside the file — a permission since fixed, a mount that
  // was not up. "Try again" has to be something a person can ask for.
  const reprobe = useMutation({
    mutationFn: () => reprobeLibraryItem(Number(id)),
    onSuccess: (res) => {
      setAutoMsg(
        res.files === 0
          ? 'No files to measure yet — run a disk scan first.'
          : `Measuring ${res.files} file(s) in the background; the Files table updates as they land.`,
      )
      void qc.invalidateQueries({ queryKey: ['library-item', id] })
    },
    onError: (e) => setAutoMsg(`✕ ${(e as Error).message}`),
  })
  const rescan = useMutation({
    mutationFn: () => rescanManualEntry(Number(id)),
    onSuccess: (item) => {
      void qc.invalidateQueries({ queryKey: ['library-item', id] })
      void qc.invalidateQueries({ queryKey: ['library'] })
      const eps = item.seasons.reduce((n, s) => n + s.episodes.length, 0)
      setAutoMsg(`Folder re-read — ${eps} episode${eps === 1 ? '' : 's'} known.`)
    },
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

  // Granular monitoring. These checkboxes are OPTIMISTIC on purpose: they are
  // controlled by server state, so without this the box does not move until a
  // round-trip completes, and a control that does not respond to a click reads
  // as a control that does not work. The old version also swallowed errors
  // entirely — a failed request left the box exactly where it started with no
  // message, which is indistinguishable from "this feature is missing".
  const monitorSeason = useMutation({
    mutationFn: (v: { season: number; monitored: boolean }) =>
      setSeasonMonitored(Number(id), v.season, v.monitored),
    onMutate: async (v) => {
      const key = ['library-item', id]
      // Cancel in-flight refetches first: one landing after this write would
      // overwrite the optimistic value with the state we are changing away
      // from, and the box would visibly snap back.
      await qc.cancelQueries({ queryKey: key })
      const previous = qc.getQueryData<MediaItemDetail>(key)
      if (previous) {
        qc.setQueryData<MediaItemDetail>(key, {
          ...previous,
          seasons: previous.seasons.map((s) =>
            s.number === v.season
              ? {
                  ...s,
                  monitored: v.monitored,
                  // Unmonitoring a season cascades to its episodes server-side;
                  // show that immediately rather than half a state.
                  episodes: s.episodes.map((e) => ({ ...e, monitored: v.monitored })),
                }
              : s,
          ),
        })
      }
      return { previous }
    },
    onSuccess: (detail) => {
      qc.setQueryData(['library-item', id], detail)
      void qc.invalidateQueries({ queryKey: ['wanted'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
    onError: (e, _v, ctx) => {
      if (ctx?.previous) qc.setQueryData(['library-item', id], ctx.previous)
      setAutoMsg(`✕ could not change season monitoring: ${(e as Error).message}`)
    },
  })
  const monitorEpisode = useMutation({
    mutationFn: (v: { episodeId: number; monitored: boolean }) =>
      setEpisodeMonitored(Number(id), v.episodeId, v.monitored),
    onMutate: async (v) => {
      const key = ['library-item', id]
      // Cancel in-flight refetches first: one landing after this write would
      // overwrite the optimistic value with the state we are changing away
      // from, and the box would visibly snap back.
      await qc.cancelQueries({ queryKey: key })
      const previous = qc.getQueryData<MediaItemDetail>(key)
      if (previous) {
        qc.setQueryData<MediaItemDetail>(key, {
          ...previous,
          seasons: previous.seasons.map((s) => ({
            ...s,
            episodes: s.episodes.map((e) =>
              e.id === v.episodeId ? { ...e, monitored: v.monitored } : e,
            ),
          })),
        })
      }
      return { previous }
    },
    onSuccess: (detail) => {
      qc.setQueryData(['library-item', id], detail)
      void qc.invalidateQueries({ queryKey: ['wanted'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
    },
    onError: (e, _v, ctx) => {
      if (ctx?.previous) qc.setQueryData(['library-item', id], ctx.previous)
      setAutoMsg(`✕ could not change episode monitoring: ${(e as Error).message}`)
    },
  })

  if (item.isLoading) return <p className="muted">Loading…</p>
  if (item.isError || !item.data) return <div className="banner warning">Item not found.</div>

  const m = item.data
  const fileCount = m.files.length
  const profile = profiles.data?.find((p) => p.id === m.qualityProfileId)
  const profileName = profile?.name
  const profileSentence = profile?.sentence
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
              {/* The sentence comes from the server, rendered from the profile
                  itself (ADR 0014). The old model needed an apology here —
                  a profile called "Any" that stopped upgrading at 1080p had to
                  be explained — and a model the UI has to apologise for is the
                  wrong model. Deleting the apology was part of the fix. */}
              {profileSentence ? <span className="muted"> — {profileSentence}</span> : null}
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
            {m.kind !== 'book' && (
              <button
                title="Read the files again and record what is actually in them — resolution, codec, HDR, audio. Use this after fixing a permission or a mount that made a probe fail."
                disabled={reprobe.isPending}
                onClick={() => reprobe.mutate()}
              >
                {reprobe.isPending ? 'Measuring…' : 'Re-measure files'}
              </button>
            )}
            {/* A manual entry has no provider to refresh against (ADR 0012
                §3), so the button is absent rather than present-and-failing.
                What replaces it for a series is a folder rescan: for a record
                with no metadata, the files are the metadata. */}
            {m.source === 'manual' ? (
              m.kind === 'series' && (
                <button
                  title="Re-read this folder for episodes that have appeared. Never removes any — a missing file is the normal state of a monitored episode."
                  disabled={rescan.isPending}
                  onClick={() => rescan.mutate()}
                >
                  {rescan.isPending ? 'Rescanning…' : 'Rescan folder'}
                </button>
              )
            ) : (
              <button
                title="Re-fetch metadata from the provider (new episodes, poster, rating, …)"
                disabled={refresh.isPending}
                onClick={() => refresh.mutate()}
              >
                {refresh.isPending ? 'Refreshing…' : 'Refresh metadata'}
              </button>
            )}
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

      {editing && (
        <EditPanel item={m} onClose={() => setEditing(false)} onSaved={setAutoMsg} />
      )}

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
          {/* Deliberately NOT <details>/<summary>. The season row carries two
              controls — the monitor checkbox and Search pack — and interactive
              content inside a <summary> is invalid HTML (a <summary> is
              exposed as a button, and you cannot nest a checkbox in a button).
              Chromium tolerated it; WebKit does not, so on Safari the season
              monitor toggle simply could not be clicked. A disclosure we own
              behaves the same everywhere. */}
          {m.seasons.map((s) => {
            const have = s.episodes.filter((e) => e.hasFile).length
            const open = openSeasons[s.number] ?? s.number === 1
            const label = s.number === 0 ? 'Specials' : `Season ${s.number}`
            return (
              <div key={s.number} className="season">
                <div className="season-head">
                  <input
                    type="checkbox"
                    className="monitor-box"
                    title={
                      s.monitored
                        ? 'Monitored — untick to stop wanting this season'
                        : 'Unmonitored — tick to want this season'
                    }
                    aria-label={`Monitor ${label}`}
                    checked={s.monitored}
                    onChange={(e) =>
                      monitorSeason.mutate({ season: s.number, monitored: e.target.checked })
                    }
                  />
                  <button
                    type="button"
                    className="season-toggle"
                    aria-expanded={open}
                    onClick={() => setOpenSeasons((prev) => ({ ...prev, [s.number]: !open }))}
                  >
                    <span className="season-caret" aria-hidden="true">
                      {open ? '▾' : '▸'}
                    </span>{' '}
                    {label}{' '}
                    <span className="muted">
                      {have}/{s.episodes.length} on disk{s.monitored ? '' : ' · unmonitored'}
                    </span>
                  </button>
                  <button onClick={() => setSearching({ season: s.number })}>Search pack</button>
                </div>
                {open && (
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
                              onChange={(ev) =>
                                monitorEpisode.mutate({
                                  episodeId: e.id,
                                  monitored: ev.target.checked,
                                })
                              }
                            />
                          </td>
                          <td className="mono">
                            {e.seasonNumber}x{String(e.episodeNumber).padStart(2, '0')}
                          </td>
                          <td>{e.title || <span className="muted">TBA</span>}</td>
                          <td className="muted">{e.airDate || '—'}</td>
                          <td>
                            {e.hasFile ? (
                              <span className="pill pill-ok">✓</span>
                            ) : (
                              <span className="muted">—</span>
                            )}
                          </td>
                          <td>
                            <button
                              onClick={() =>
                                setSearching({ season: e.seasonNumber, episode: e.episodeNumber })
                              }
                            >
                              Search
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
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
                <th>Quality</th>
                <th>How we know</th>
                <th>Size</th>
                <th>Episodes</th>
                {m.copies.length > 0 && <th>Copy</th>}
              </tr>
            </thead>
            <tbody>
              {m.files.map((f) => (
                <tr key={f.id}>
                  <td className="mono">
                    {f.path}
                    {f.facts ? <div className="file-facts">{f.facts}</div> : null}
                  </td>
                  <td>
                    {f.quality ? (
                      <span className="pill pill-neutral">{f.quality}</span>
                    ) : (
                      <span className="muted">unknown</span>
                    )}
                  </td>
                  <td>
                    <ProvenanceBadge f={f} />
                  </td>
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
