import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { autoSearchItem, fmtRelative, getTasks, getWanted, runTask } from '../api'
import { PageSizePicker, Pager, clampPage, sliceForPage } from '../Pager'
import { selectWanted, wantedKind, type WantedFilter, type WantedSort } from '../wanted'

// WantedPage makes the automation visible: everything missing or below
// cutoff, plus when the loops that hunt for it last ran / run next.
export function WantedPage() {
  const qc = useQueryClient()
  const wanted = useQuery({ queryKey: ['wanted'], queryFn: getWanted, refetchInterval: 30_000 })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 15_000 })
  const [filter, setFilter] = useState<WantedFilter>('all')
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<WantedSort>('title')
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('asc')
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(50)

  const backlog = useMutation({
    mutationFn: () => runTask('backlog.search'),
    onSuccess: () => {
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: ['wanted'] })
        void qc.invalidateQueries({ queryKey: ['tasks'] })
      }, 2000)
    },
  })

  // Per-item automatic search; remembers which items were kicked off.
  const [kicked, setKicked] = useState<Set<number>>(new Set())
  const one = useMutation({
    mutationFn: (itemId: number) => autoSearchItem(itemId),
    onSuccess: (_data, itemId) => {
      setKicked((prev) => new Set(prev).add(itemId))
      setTimeout(() => void qc.invalidateQueries({ queryKey: ['wanted'] }), 4000)
    },
  })

  const loop = (name: string) => tasks.data?.find((t) => t.name === name)
  const rss = loop('rss.sync')
  const bl = loop('backlog.search')
  const now = new Date()
  const all = useMemo(() => wanted.data ?? [], [wanted.data])
  const missingCount = useMemo(() => all.filter((item) => item.missing).length, [all])
  const upgradeCount = all.length - missingCount
  const selected = useMemo(
    () => selectWanted(all, filter, query, sort, sortDirection),
    [all, filter, query, sort, sortDirection],
  )
  const currentPage = clampPage(page, selected.length, pageSize)
  const visible = sliceForPage(selected, currentPage, pageSize)

  // A refresh can remove enough wanted targets to eliminate the current page.
  // Persist the clamped value so later additions do not jump back to a stale
  // page number that was only hidden by the render-time clamp.
  useEffect(() => {
    if (page !== currentPage) setPage(currentPage)
  }, [currentPage, page])

  const resetPage = () => setPage(0)
  const clearFilters = () => {
    setFilter('all')
    setQuery('')
    setPage(0)
  }

  return (
    <>
      <header className="page-head">
        <h1>Wanted</h1>
        <div className="head-actions">
          <button onClick={() => backlog.mutate()} disabled={backlog.isPending}>
            Search all now
          </button>
        </div>
      </header>

      <p className="muted">
        Everything monitored that's missing or below its quality cutoff. Two loops work this
        list automatically: RSS sync
        {rss && <> (last {fmtRelative(rss.lastRunAt, now)}, next {fmtRelative(rss.nextRunAt, now)})</>}
        {' '}grabs new releases as indexers publish them, and the backlog search
        {bl && <> (last {fmtRelative(bl.lastRunAt, now)})</>} actively hunts for the rest.
      </p>

      {all.length === 0 && wanted.isSuccess && (
        <div className="banner">
          Nothing wanted — everything monitored is on disk at or above its cutoff.
        </div>
      )}

      {all.length > 0 && (
        <section className="panel">
          <div className="section-toolbar wanted-toolbar">
            <input
              type="search"
              aria-label="Search wanted items"
              placeholder="Search title, reason, quality…"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value)
                resetPage()
              }}
            />
            <select
              aria-label="Filter wanted reason"
              title="Filter by why the item is wanted"
              value={filter}
              onChange={(event) => {
                setFilter(event.target.value as WantedFilter)
                resetPage()
              }}
            >
              <option value="all">All reasons ({all.length})</option>
              <option value="missing">Missing ({missingCount})</option>
              <option value="upgrade">Upgrades ({upgradeCount})</option>
            </select>
            <select
              aria-label="Sort wanted items"
              title="Sort wanted items"
              value={sort}
              onChange={(event) => {
                setSort(event.target.value as WantedSort)
                resetPage()
              }}
            >
              <option value="title">Sort: title</option>
              <option value="reason">Sort: reason</option>
              <option value="kind">Sort: media type</option>
            </select>
            <button
              aria-label={sortDirection === 'asc'
                ? 'Sort direction: ascending. Change to descending'
                : 'Sort direction: descending. Change to ascending'}
              title={sortDirection === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending'}
              onClick={() => {
                setSortDirection((direction) => direction === 'asc' ? 'desc' : 'asc')
                resetPage()
              }}
            >
              {sortDirection === 'asc' ? '↑' : '↓'}
            </button>
            <PageSizePicker
              size={pageSize}
              label="Show"
              onChange={(size) => {
                setPageSize(size)
                resetPage()
              }}
            />
            <div className="toolbar-spacer" />
            <Pager page={currentPage} size={pageSize} total={selected.length} onPage={setPage} />
          </div>

          {selected.length === 0 ? (
            <p className="muted wanted-empty">
              Nothing matches the current search and reason filter.{' '}
              <button className="link-button" onClick={clearFilters}>Show all {all.length}</button>
            </p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Title</th>
                  <th>Type</th>
                  <th>Status</th>
                  <th>Action</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((w) => (
                  <tr key={w.wantableId}>
                    <td>
                      <Link to="/library/$id" params={{ id: String(w.mediaItemId) }}>
                        {w.title}
                      </Link>{' '}
                      <span className="muted">{w.detail}</span>
                      {w.copy && (
                        <span className="pill pill-info" title="This entry is for an additional quality copy">
                          {w.copy}
                        </span>
                      )}
                    </td>
                    <td className="muted mono">{wantedKind(w)}</td>
                    <td>
                      {w.missing ? (
                        <span className="pill pill-warning">missing</span>
                      ) : (
                        <span className="pill pill-neutral">upgrade from {w.current}</span>
                      )}
                    </td>
                    <td>
                      {kicked.has(w.mediaItemId) ? (
                        <span className="muted">searching…</span>
                      ) : (
                        <button
                          title="Automatic search for this item — best accepted release is grabbed"
                          disabled={one.isPending}
                          onClick={() => one.mutate(w.mediaItemId)}
                        >
                          Search
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          {selected.length > 0 && (
            <div className="wanted-pager-bar">
              <Pager page={currentPage} size={pageSize} total={selected.length} onPage={setPage} />
            </div>
          )}
        </section>
      )}
    </>
  )
}
