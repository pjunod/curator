import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  cancelWantedSearch,
  createWantedSearch,
  fmtRelative,
  getTasks,
  getWanted,
  getWantedSearches,
  getWantedSearchResults,
  type WantedSearchRun,
  type WantedSearchScope,
  type WantedSearchRequest,
} from '../api'
import { PAGE_SIZES, PageSizePicker, Pager, clampPage, sliceForPage } from '../Pager'
import { selectWantedGroups, type WantedFilter, type WantedGroup, type WantedSort } from '../wanted'

const RUNS_QUERY_KEY = ['wanted-searches'] as const
const RESULTS_PAGE_SIZE = 100
const SIZE_KEY = 'monarr-wanted-page-size'

function savedPageSize(): number {
  try {
    const stored = localStorage.getItem(SIZE_KEY)
    if (stored === null) return 50
    const size = Number(stored)
    return PAGE_SIZES.includes(size as (typeof PAGE_SIZES)[number]) ? size : 50
  } catch { return 50 }
}

function titleCase(value: string): string {
  return value ? value[0].toUpperCase() + value.slice(1) : value
}

function GroupActionLabel({ group, filter }: { group: WantedGroup; filter: WantedFilter }) {
  const noun = group.kind === 'series' ? 'show' : 'group'
  return <>{filter === 'all' ? `Search ${noun}` : `Search ${titleCase(filter)} in ${noun}`} ({group.children.length})</>
}

export function WantedPage() {
  const qc = useQueryClient()
  const wanted = useQuery({ queryKey: ['wanted'], queryFn: getWanted, refetchInterval: 30_000 })
  const tasks = useQuery({ queryKey: ['tasks'], queryFn: getTasks, refetchInterval: 15_000 })
  const [filter, setFilter] = useState<WantedFilter>('all')
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<WantedSort>('title')
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('asc')
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(savedPageSize)
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  const [targetDelayMs, setTargetDelayMs] = useState(1000)
  const [runNotice, setRunNotice] = useState('')

  const runs = useQuery({
    queryKey: RUNS_QUERY_KEY,
    queryFn: getWantedSearches,
    refetchInterval: (query) => query.state.data?.items.some(isActiveRun) ? 1000 : 5000,
  })

  const previousRuns = useRef<WantedSearchRun[] | undefined>(undefined)
  useEffect(() => {
    const currentRuns = runs.data?.items
    if (!currentRuns) return
    const previous = previousRuns.current
    previousRuns.current = currentRuns
    if (!previous) return
    const previousTerminal = new Set(previous.filter((run) => !isActiveRun(run)).map((run) => run.runId))
    if (currentRuns.some((run) => !isActiveRun(run) && !previousTerminal.has(run.runId))) {
      void qc.invalidateQueries({ queryKey: ['wanted'] })
      void qc.invalidateQueries({ queryKey: ['queue'] })
      void qc.invalidateQueries({ queryKey: ['library'] })
    }
  }, [qc, runs.data])

  const submit = useMutation({
    mutationFn: (request: WantedSearchRequest) => createWantedSearch(request),
    onSuccess: (run) => updateRun(qc, run),
    onError: (error) => setRunNotice(`Could not start search: ${error.message}`),
  })

  const loop = (name: string) => tasks.data?.find((task) => task.name === name)
  const rss = loop('rss.sync')
  const backlog = loop('backlog.search')
  const now = new Date()
  const all = useMemo(() => wanted.data ?? [], [wanted.data])
  const missingCount = useMemo(() => all.filter((item) => item.reason === 'missing').length, [all])
  const upgradeCount = all.length - missingCount
  const selected = useMemo(
    () => selectWantedGroups(all, filter, query, sort, sortDirection),
    [all, filter, query, sort, sortDirection],
  )
  const currentPage = clampPage(page, selected.length, pageSize)
  const visible = sliceForPage(selected, currentPage, pageSize)

  useEffect(() => {
    if (page !== currentPage) setPage(currentPage)
  }, [currentPage, page])
  useEffect(() => {
    const surviving = new Set(all.map((item) => item.mediaItemId))
    setExpanded((old) => new Set([...old].filter((id) => surviving.has(id))))
  }, [all])

  const changePageSize = (size: number) => {
    setPageSize(size)
    setPage(0)
    try { localStorage.setItem(SIZE_KEY, String(size)) } catch { /* private mode */ }
  }
  const clearFilters = () => {
    setFilter('all')
    setQuery('')
    setPage(0)
  }
  const start = (scope: WantedSearchScope) => {
    setRunNotice('')
    const requestId = globalThis.crypto?.randomUUID?.() ?? `wanted-${Date.now()}-${Math.random().toString(36).slice(2)}`
    submit.mutate({ ...scope, targetDelayMs, requestId })
  }
  const groupScope = (group: WantedGroup): WantedSearchScope => ({
    scope: 'group',
    mediaItemId: group.mediaItemId,
    ...(filter === 'all' ? {} : { reason: filter }),
  })

  return (
    <>
      <header className="page-head">
        <h1>Wanted</h1>
        <div className="head-actions">
          <button onClick={() => start({ scope: 'all' })} disabled={all.length === 0}>
            Search all now ({all.length})
          </button>
        </div>
      </header>

      <p className="muted">
        Everything monitored that's missing or below its quality cutoff. RSS sync
        {rss && <> (last {fmtRelative(rss.lastRunAt, now)}, next {fmtRelative(rss.nextRunAt, now)})</>}
        {' '}grabs new releases as indexers publish them; backlog search
        {backlog && <> (last {fmtRelative(backlog.lastRunAt, now)})</>} hunts the rest.
        Group searches examine individual episodes; use an item's Auto search for season packs.
      </p>

      {wanted.isLoading && <div className="banner">Loading the wanted list…</div>}
      {wanted.isError && (
        <div className="banner banner-error">
          Could not load Wanted: {(wanted.error as Error).message}{' '}
          <button onClick={() => void wanted.refetch()}>Retry</button>
        </div>
      )}
      {all.length === 0 && wanted.isSuccess && (
        <div className="banner">Nothing wanted — everything monitored is on disk at or above its cutoff.</div>
      )}
      {runNotice && <div className="banner banner-error" role="alert">{runNotice}</div>}
      {runs.isError && (
        <div className="banner banner-error">
          Could not load Wanted searches: {runs.error.message}{' '}
          <button onClick={() => void runs.refetch()}>Retry</button>
        </div>
      )}
      {runs.data && runs.data.items.length > 0 && (
        <section aria-label="Wanted searches">
          {runs.data.items.map((run) => <WantedRun key={run.runId} run={run} now={now} />)}
        </section>
      )}

      {all.length > 0 && (
        <section className="panel">
          <div className="wanted-reasons" aria-label="Search every title by reason">
            <span className="muted">Reason actions span all titles and pages:</span>
            <button disabled={missingCount === 0} onClick={() => start({ scope: 'reason', reason: 'missing' })}>
              Search all Missing ({missingCount})
            </button>
            <button disabled={upgradeCount === 0} onClick={() => start({ scope: 'reason', reason: 'upgrade' })}>
              Search all Upgrade ({upgradeCount})
            </button>
            <details className="wanted-advanced">
              <summary>Advanced pacing</summary>
              <label>Gap between targets (ms){' '}
                <input type="number" min={0} max={60000} step={100} value={targetDelayMs}
                  onChange={(event) => setTargetDelayMs(Math.round(Math.max(0, Math.min(60000, Number(event.target.value)))))} />
              </label>
              <span className="muted">Minimum gap; indexer timeouts and rate limits still apply.</span>
            </details>
          </div>
          <div className="section-toolbar wanted-toolbar">
            <input type="search" aria-label="Search wanted items" placeholder="Search title, reason, quality…"
              value={query} onChange={(event) => { setQuery(event.target.value); setPage(0) }} />
            <select aria-label="Filter wanted reason" title="Filter by why the item is wanted" value={filter}
              onChange={(event) => { setFilter(event.target.value as WantedFilter); setPage(0) }}>
              <option value="all">All ({all.length})</option>
              <option value="missing">Missing ({missingCount})</option>
              <option value="upgrade">Upgrade ({upgradeCount})</option>
            </select>
            <select aria-label="Sort wanted titles" title="Sort wanted titles" value={sort}
              onChange={(event) => { setSort(event.target.value as WantedSort); setPage(0) }}>
              <option value="title">Sort: title</option><option value="reason">Sort: reason</option>
              <option value="kind">Sort: media type</option>
            </select>
            <button aria-label={sortDirection === 'asc' ? 'Sort direction: ascending. Change to descending' : 'Sort direction: descending. Change to ascending'}
              title={sortDirection === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending'}
              onClick={() => { setSortDirection((direction) => direction === 'asc' ? 'desc' : 'asc'); setPage(0) }}>
              {sortDirection === 'asc' ? '↑' : '↓'}
            </button>
            <PageSizePicker size={pageSize} label="Titles per page" onChange={changePageSize} />
            <div className="toolbar-spacer" />
            <Pager page={currentPage} size={pageSize} total={selected.length} onPage={setPage} />
          </div>

          {selected.length === 0 ? (
            <p className="muted wanted-empty">Nothing matches the current search and reason filter.{' '}
              <button className="link-button" onClick={clearFilters}>Show all {all.length}</button>
            </p>
          ) : (
            <table className="wanted-groups-table">
              <thead><tr><th>Title</th><th>Type</th><th>Wanted</th><th>Action</th></tr></thead>
              <tbody>
                {visible.map((group) => {
                  const direct = group.children.length === 1 && group.kind !== 'series'
                  const open = expanded.has(group.mediaItemId)
                  const region = `wanted-group-${group.mediaItemId}`
                  return (
                    <Fragment key={group.mediaItemId}>
                      <tr className="wanted-group-row">
                        <td>
                          {!direct && <button className="wanted-expander" aria-expanded={open} aria-controls={region}
                            aria-label={`${open ? 'Collapse' : 'Expand'} ${group.title}`}
                            onClick={() => setExpanded((old) => {
                              const next = new Set(old); if (next.has(group.mediaItemId)) next.delete(group.mediaItemId); else next.add(group.mediaItemId); return next
                            })}>{open ? '▾' : '▸'}</button>}
                          <Link to="/library/$id" params={{ id: String(group.mediaItemId) }}>{group.title}</Link>
                          {direct && <span className="muted"> {group.children[0].detail}</span>}
                          {/* A single wanted target can be an additional copy or a
                              book edition while the primary is satisfied — the row
                              has to say which, or a grab lands on the wrong one. */}
                          {direct && (group.children[0].copy || group.kind === 'book') && (
                            <>{' '}<span className="pill pill-info">
                              {group.kind === 'book' ? `${group.children[0].copy || 'Primary'} edition` : group.children[0].copy}
                            </span></>
                          )}
                        </td>
                        <td className="muted mono">{group.kind}</td>
                        <td>{group.children.length} wanted · {group.missing} missing · {group.upgrade} upgrade</td>
                        <td><button title={group.kind === 'series' ? "Searches individual episodes; use the item's Auto search for season packs." : undefined}
                          onClick={() => start(direct ? { scope: 'target', wantableId: group.children[0].wantableId } : groupScope(group))}>
                          {direct ? <>Search item</> : <GroupActionLabel group={group} filter={filter} />}
                        </button></td>
                      </tr>
                      {!direct && open && (
                        <tr className="wanted-children-row">
                          <td colSpan={4}>
                            <div id={region} role="region" aria-label={`${group.title} wanted targets`}>
                              <table className="wanted-child-table"><tbody>
                                {group.children.map((child) => (
                                  <tr key={child.wantableId} className="wanted-child-row">
                                    <td>
                                      <span className="wanted-child-detail">{child.detail}</span>{' '}
                                      <span className="pill pill-info">
                                        {group.kind === 'book' ? `${child.copy || 'Primary'} edition` : child.copy || 'Primary'}
                                      </span>
                                    </td>
                                    <td className="muted">{child.reason === 'missing' ? 'Missing' : `Upgrade from ${child.current}`}</td>
                                    <td className="muted mono">{child.wantableId}</td>
                                    <td><button onClick={() => start({ scope: 'target', wantableId: child.wantableId })}>
                                      {group.kind === 'series' ? 'Search episode' : 'Search item'}
                                    </button></td>
                                  </tr>
                                ))}
                              </tbody></table>
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          )}
          {selected.length > 0 && (
            <div className="wanted-pager-bar">
              <span className="muted">{selected.reduce((sum, group) => sum + group.children.length, 0)} wanted items</span>
              <Pager page={currentPage} size={pageSize} total={selected.length} onPage={setPage} />
            </div>
          )}
        </section>
      )}
    </>
  )
}

function isActiveRun(run: WantedSearchRun): boolean {
  return run.status === 'queued' || run.status === 'running'
}

function updateRun(qc: ReturnType<typeof useQueryClient>, run: WantedSearchRun) {
  qc.setQueryData<Awaited<ReturnType<typeof getWantedSearches>>>(RUNS_QUERY_KEY, (previous) => {
    const items = previous?.items ?? []
    return {
      items: items.some((item) => item.runId === run.runId)
        ? items.map((item) => item.runId === run.runId ? run : item)
        : [run, ...items],
    }
  })
  void qc.invalidateQueries({ queryKey: RUNS_QUERY_KEY })
}

function WantedRun({ run, now }: { run: WantedSearchRun; now: Date }) {
  const qc = useQueryClient()
  const [resultPage, setResultPage] = useState(0)
  const [showResults, setShowResults] = useState(false)
  const active = isActiveRun(run)
  const results = useQuery({
    queryKey: ['wanted-search-results', run.runId, resultPage],
    queryFn: () => getWantedSearchResults(run.runId, RESULTS_PAGE_SIZE, resultPage * RESULTS_PAGE_SIZE),
    enabled: !active && showResults,
  })
  const cancel = useMutation({
    mutationFn: () => cancelWantedSearch(run.runId),
    onSuccess: (next) => updateRun(qc, next),
  })

  return (
    <article className="panel wanted-run">
      <div className="wanted-run-head">
        <div aria-live="polite">
          <strong>{titleCase(run.status)}</strong> · {run.scopeLabel}
          <div className="muted">
            {run.status === 'queued' ? `${run.selected} selected items` : active
              ? `Searching ${run.processed} of ${run.selected} selected items`
              : `${run.processed} of ${run.selected} selected items processed`}
            {' '}· created {fmtRelative(run.createdAt, now)}
          </div>
        </div>
        {active && (
          <button aria-label={`Cancel ${run.scopeLabel}`}
            disabled={cancel.isPending || Boolean(run.cancelRequestedAt)} onClick={() => cancel.mutate()}>
            {run.cancelRequestedAt ? 'Cancelling…' : 'Cancel'}
          </button>
        )}
      </div>
      <div className="wanted-run-counts">
        <span>Searched {run.searched}</span><span>Skipped {run.skipped}</span>
        <span>Failed {run.failed}</span><span>Grabbed {run.grabbed}</span>
      </div>
      {run.error && <p className="error">{run.error}</p>}
      {cancel.isError && <p className="error">Could not cancel search: {cancel.error.message}</p>}
      {!active && run.processed > 0 && (
        <details onToggle={(event) => setShowResults(event.currentTarget.open)}>
          <summary>Target results ({run.processed})</summary>
          {results.isPending && <p className="muted">Loading target results…</p>}
          {results.isError && (
            <p className="error">Could not load target results: {results.error.message}{' '}
              <button onClick={() => void results.refetch()}>Retry</button>
            </p>
          )}
          {results.data && (
            <>
              <ul className="wanted-results">
                {results.data.items.map((item) => (
                  <li key={item.ordinal}>
                    <strong>{item.label}</strong> — {item.state}
                    {item.skipped && ` (${item.skipped.replaceAll('_', ' ')})`}
                    {item.grabbed && ` · grabbed ${item.grabbed}`}
                    {!item.grabbed && item.state === 'searched' && ` · ${item.seen} seen, ${item.matched} matched, ${item.accepted} accepted`}
                    {item.error && <span className="error"> · {item.error}</span>}
                  </li>
                ))}
              </ul>
              <Pager page={resultPage} size={RESULTS_PAGE_SIZE} total={run.processed} onPage={setResultPage} />
            </>
          )}
        </details>
      )}
    </article>
  )
}
