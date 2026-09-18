import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ReleaseCandidate } from '../api'
import { fmtBytes, grabRelease, searchReleases } from '../api'
import { PageSizePicker, Pager, clampPage, sliceForPage } from '../Pager'

// ReleaseSearch is the interactive search panel.
//
// Every candidate is shown — a rejected release carries its reasons instead of
// being hidden, because "monarr found nothing" and "monarr found eleven things
// and disliked all of them" are different problems and the user has to be able
// to tell them apart.
//
// That principle produced a list seven miles long. A busy indexer returns
// hundreds of releases for one movie, the accepted handful sat at the top of an
// endless scroll, and every rejection printed in full underneath its own row.
// So: the list pages, the rejections collapse to one line, and there is a
// filter for the common case of only wanting to see what monarr would take.
// Nothing is dropped — the counts stay visible and switching back shows
// everything again.

type Filter = 'all' | 'accepted'

export function ReleaseSearch(props: {
  mediaItemId: number
  season?: number
  episode?: number
  copyId?: number
  label?: string
  onClose: () => void
}) {
  const { mediaItemId, season, episode, copyId } = props
  const [grabbed, setGrabbed] = useState<string | null>(null)
  const [filter, setFilter] = useState<Filter>('all')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  const [size, setSize] = useState(50)

  const search = useQuery({
    queryKey: ['releases', mediaItemId, copyId ?? 0, season ?? -1, episode ?? -1],
    queryFn: () => searchReleases(mediaItemId, season, episode, copyId),
    retry: false,
    staleTime: 60_000,
  })

  const grab = useMutation({
    mutationFn: (c: ReleaseCandidate) =>
      grabRelease({
        mediaItemId, copyId, season, episode,
        title: c.title, downloadUrl: c.downloadUrl, indexer: c.indexer,
        protocol: c.protocol, size: c.size, candidateToken: c.candidateToken,
      }),
    onSuccess: (_res, c) => setGrabbed(c.title),
  })

  const all = useMemo(() => search.data?.candidates ?? [], [search.data])
  const acceptedCount = useMemo(() => all.filter((c) => c.accepted).length, [all])

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase()
    return all.filter((c) => {
      if (filter === 'accepted' && !c.accepted) return false
      if (needle && !c.title.toLowerCase().includes(needle)) return false
      return true
    })
  }, [all, filter, query])

  // A filter that shrinks the list under you should not strand you on page 9.
  const current = clampPage(page, filtered.length, size)
  const visible = sliceForPage(filtered, current, size)

  const label = props.label ?? (
    season !== undefined && episode !== undefined
      ? `S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`
      : season !== undefined
        ? `Season ${season} pack`
        : 'item'
  )

  return (
    <section className="panel">
      <h2>
        Interactive search — {label}
        <button style={{ marginLeft: 'auto' }} onClick={props.onClose}>
          Close
        </button>
      </h2>

      {search.isFetching && <p className="muted">Searching indexers…</p>}
      {search.data?.partial && (
        <div className="banner warning">
          Results are incomplete{search.data.reason ? `: ${search.data.reason}` : '.'}
        </div>
      )}
      {search.isError && <div className="banner warning">{String((search.error as Error).message)}</div>}
      {grab.isError && <div className="banner warning">{String((grab.error as Error).message)}</div>}
      {grabbed && (
        <div className="banner">
          Grabbed <span className="mono">{grabbed}</span> — follow it on the{' '}
          <Link to="/activity">Activity page</Link>.
        </div>
      )}

      {search.data && all.length === 0 && (
        <p className="muted">No releases found on any enabled indexer.</p>
      )}

      {all.length > 0 && (
        <>
          <div className="section-toolbar release-toolbar">
            <input
              type="search"
              placeholder="Filter by name…"
              value={query}
              onChange={(e) => {
                setQuery(e.target.value)
                setPage(0)
              }}
            />
            <select
              value={filter}
              onChange={(e) => {
                setFilter(e.target.value as Filter)
                setPage(0)
              }}
              title="A rejected release can still be grabbed by hand — a manual grab is never gated."
            >
              <option value="all">Everything ({all.length})</option>
              <option value="accepted">Would be grabbed ({acceptedCount})</option>
            </select>
            <PageSizePicker size={size} onChange={(n) => { setSize(n); setPage(0) }} />
            <div className="toolbar-spacer" />
            <Pager page={current} size={size} total={filtered.length} onPage={setPage} />
          </div>

          {filtered.length === 0 ? (
            <p className="muted">
              Nothing matches that filter.{' '}
              {acceptedCount === 0 &&
                'No release here passes the profile — switch to "Everything" to see why each was declined.'}
            </p>
          ) : (
            <table className="release-table">
              <thead>
                <tr>
                  <th>Release</th>
                  <th>Quality</th>
                  <th className="num">Size</th>
                  <th className="num">Age</th>
                  <th className="num">Seed</th>
                  <th>Indexer</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {visible.map((c) => (
                  <ReleaseRow
                    key={`${c.indexer}-${c.title}`}
                    c={c}
                    busy={grab.isPending}
                    onGrab={() => grab.mutate(c)}
                  />
                ))}
              </tbody>
            </table>
          )}

          <div className="section-toolbar release-toolbar">
            <Pager page={current} size={size} total={filtered.length} onPage={setPage} />
          </div>
        </>
      )}
    </section>
  )
}

/**
 * One release row.
 *
 * The rejections are the reason this is its own component. Printed in full
 * they run three lines per row and turn the table into a wall; hidden entirely
 * they take away the one thing that explains why a search "found nothing". So
 * the first reason shows and the rest expand on click — the answer stays one
 * glance away without ever being in the way.
 */
function ReleaseRow({ c, busy, onGrab }: { c: ReleaseCandidate; busy: boolean; onGrab: () => void }) {
  const [expanded, setExpanded] = useState(false)
  const [first, ...rest] = c.rejections

  return (
    <tr className={c.accepted ? '' : 'row-rejected'}>
      <td>
        <div className="release-title mono">
          {c.infoUrl ? (
            <a href={c.infoUrl} target="_blank" rel="noreferrer" title="Open on the indexer (new tab)">
              {c.title}
            </a>
          ) : (
            c.title
          )}
        </div>
        {c.isUpgrade && <span className="ok-text">upgrade</span>}
        {c.match.matched && (
          <div className="ok-text">✓ {c.match.reason}</div>
        )}
        {/* A size that cannot hold the claim is worth saying even on a release
            the profile would take — but it is a caution, not a refusal, and it
            has to read as one. */}
        {c.warning && <div className="warn-text">⚠ {c.warning}</div>}
        {first && (
          <div className="error-text">
            ✕ {first.reason}
            {rest.length > 0 && (
              <button className="link-button" onClick={() => setExpanded((v) => !v)}>
                {expanded ? 'less' : `+${rest.length} more`}
              </button>
            )}
          </div>
        )}
        {expanded &&
          rest.map((r) => (
            <div key={r.code} className="error-text">
              ✕ {r.reason}
            </div>
          ))}
      </td>
      <td>
        <span className="pill pill-neutral">{c.quality}</span>
        {c.score !== 0 && (
          <div className="muted" title={(c.formats ?? []).join(', ')}>
            score {c.score > 0 ? '+' : ''}
            {c.score}
          </div>
        )}
      </td>
      <td className="muted num">{fmtBytes(c.size)}</td>
      <td className="muted num">{c.age || '—'}</td>
      <td className="muted num">{c.protocol === 'torrent' ? c.seeders : '—'}</td>
      <td className="muted">{c.indexer}</td>
      <td>
        <button
          className={c.accepted ? 'btn-accent' : ''}
          disabled={busy}
          onClick={onGrab}
          title={c.accepted ? 'Send to download client' : 'Grab anyway (override rejections)'}
        >
          Grab
        </button>
      </td>
    </tr>
  )
}
