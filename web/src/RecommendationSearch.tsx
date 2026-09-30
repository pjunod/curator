import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { getRecommendationStatus, posterUrl, recommendSeries } from './api'
import type { RecommendationFilters, RecommendationResponse, RecommendationTheme, SearchResult } from './api'
import './RecommendationSearch.css'

export function RecommendationSearch({ visible, seed, resetContext, onSeed, onPreview, onAdd, busy, canAdd, addedId }: {
  visible: boolean; seed: SearchResult | null; onSeed: (item: SearchResult | null) => void
  onPreview: (item: SearchResult) => void; onAdd: (item: SearchResult) => void
  busy: boolean; canAdd: boolean; addedId: (item: SearchResult) => number | undefined
  resetContext: number
}) {
  const status = useQuery({ queryKey: ['recommendation-status'], queryFn: getRecommendationStatus })
  const [query, setQuery] = useState('')
  const [filters, setFilters] = useState<RecommendationFilters>({ hideInLibrary: true })
  const [overrides, setOverrides] = useState<RecommendationFilters>({ hideInLibrary: true })
  const [response, setResponse] = useState<RecommendationResponse | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const controller = useRef<AbortController | null>(null)
  const generation = useRef(0)
  const heading = useRef<HTMLHeadingElement>(null)
  const change = (patch: RecommendationFilters) => { controller.current?.abort(); generation.current++; setLoading(false); setResponse(null); setFilters({ ...filters, ...patch }); setOverrides({ ...overrides, ...patch }) }
  const run = async (explicit = overrides, text = query) => {
    controller.current?.abort(); const abort = new AbortController(); controller.current = abort
    const current = ++generation.current; setLoading(true); setError(''); setResponse(null)
    try {
      const data = await recommendSeries({ kind: 'series', query: text, filters: explicit, seed: seed ? { provider: seed.tmdbId ? 'tmdb' : 'tvdb', id: String(seed.tmdbId || seed.tvdbId) } : null }, abort.signal)
      if (current !== generation.current) return
      setResponse(data); setFilters(data.applied.filters)
    } catch (failure) { if (!abort.signal.aborted && current === generation.current) setError((failure as Error).message) }
    finally { if (current === generation.current) setLoading(false) }
  }
  const runRef = useRef(run); runRef.current = run
  const priorReset = useRef(resetContext)
  useEffect(() => {
    const reset = priorReset.current !== resetContext; priorReset.current = resetContext
    if (reset) { setQuery(''); setFilters({ hideInLibrary: true }); setOverrides({ hideInLibrary: true }) }
    if (seed) void runRef.current(reset ? { hideInLibrary: true } : undefined, reset ? '' : undefined)
    else { controller.current?.abort(); generation.current++; setLoading(false); setResponse(null) }
  }, [seed?.tmdbId, seed?.tvdbId, resetContext])
  useEffect(() => { if (!visible) { controller.current?.abort(); generation.current++; setLoading(false) } }, [visible])
  useEffect(() => () => controller.current?.abort(), [])
  const results = response?.results.filter((r) => !filters.hideInLibrary || !addedId(r.item)) ?? []
  return <section className="recommendation-search" hidden={!visible} aria-label="Describe series">
    <form onSubmit={(event) => { event.preventDefault(); void run() }}>
      <label htmlFor="series-description">Describe what you want</label>
      <div className="recommendation-query"><input id="series-description" value={query} maxLength={500} placeholder="Gay-themed TV series, no teen dramas…" onChange={(event) => { controller.current?.abort(); generation.current++; setLoading(false); setResponse(null); setFilters({ hideInLibrary: true, ...overrides }); setQuery(event.target.value) }} /><button className="btn-accent" type="submit" disabled={loading}>Find series</button></div>
      <p className="muted">Choose a theme or use More like this on a title. Catalog metadata comes from TMDB; optional similarity ranking runs locally.</p>
      {seed && <p className="recommendation-seed">Similar to: <strong>{seed.title} · {seed.year || 'year unknown'}</strong> <button type="button" onClick={() => onSeed(null)}>Remove seed</button> {filters.theme && <span>Keep this theme</span>}</p>}
      <div className="recommendation-filters">
        <label>Theme<select aria-label="Theme" value={filters.theme ?? ''} onChange={(event) => change({ theme: (event.target.value || null) as RecommendationTheme | null, centralThemeOnly: false })}><option value="">No required theme</option>{status.data?.themes.map((theme) => <option key={theme.key} value={theme.key}>{theme.label}</option>)}</select></label>
        <label>Genre<select aria-label="Genre" value={filters.genres?.[0] ?? ''} onChange={(event) => change({ genres: event.target.value ? [Number(event.target.value)] : [] })}><option value="">Any</option>{[[18, 'Drama'], [35, 'Comedy'], [10765, 'Sci-Fi & Fantasy'], [80, 'Crime'], [9648, 'Mystery'], [16, 'Animation'], [10759, 'Action & Adventure']].map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select></label>
        <label>Original language<select aria-label="Original language" value={filters.originalLanguage ?? ''} onChange={(event) => change({ originalLanguage: event.target.value || null })}><option value="">Any</option>{[['en', 'English'], ['ko', 'Korean'], ['ja', 'Japanese'], ['th', 'Thai'], ['zh', 'Chinese'], ['fr', 'French'], ['es', 'Spanish'], ['de', 'German']].map(([code, name]) => <option key={code} value={code}>{name}</option>)}</select></label>
        <label>From year<input type="number" min={1900} max={new Date().getFullYear() + 5} value={filters.yearFrom ?? ''} onChange={(event) => change({ yearFrom: event.target.value ? Number(event.target.value) : null })} /></label>
        <label>To year<input type="number" min={1900} max={new Date().getFullYear() + 5} value={filters.yearTo ?? ''} onChange={(event) => change({ yearTo: event.target.value ? Number(event.target.value) : null })} /></label>
        <label className="inline"><input type="checkbox" disabled={!filters.theme} checked={filters.centralThemeOnly ?? false} onChange={(event) => change({ centralThemeOnly: event.target.checked })} />Central theme only</label>
        <label className="inline"><input type="checkbox" checked={filters.excludeTeenFocus ?? false} onChange={(event) => change({ excludeTeenFocus: event.target.checked })} />Exclude known teen-focused stories</label>
        <label className="inline"><input type="checkbox" checked={filters.hideInLibrary ?? true} onChange={(event) => change({ hideInLibrary: event.target.checked })} />Hide in library</label>
      </div>
      <p className="muted">Unclassified shows may remain when excluding known teen-focused stories.</p>
      {filters.centralThemeOnly && <p className="muted">Only explicit main-story evidence qualifies. Sparse metadata can produce few or no results.</p>}
    </form>
    {error && <p className="banner warning" role="alert">{error} <button onClick={() => void run()}>Retry</button></p>}
    <div role="status" aria-live="polite">{loading && 'Finding series…'}{response && `${results.length} suggestions · ${response.checkedCount} candidates checked · ${response.coverage === 'partial' ? 'Some metadata unavailable' : 'Bounded catalog search'}`}</div>
    {response?.warnings.map((warning) => <p key={warning.code} className="muted">{warning.message}</p>)}
    {response?.state === 'ready' && results.length === 0 && <p>No eligible matches in the checked candidates. Try an explicit filter change or another seed.</p>}
    <h2 ref={heading} tabIndex={-1} className="sr-only">Series suggestions</h2>
    <ul className="result-list">{results.map((row) => <li key={row.key} className="result">
      {row.item.posterPath ? <img src={posterUrl(row.item.posterPath, 'w185')} alt="" loading="lazy" /> : <div className="poster-fallback small">{row.item.title.slice(0, 1)}</div>}
      <div className="result-body"><div className="result-title">{row.item.title} <span className="muted">({row.item.year || '—'})</span></div><p className="muted clamp">{row.item.overview}</p>
        <button onClick={() => onPreview(row.item)}>View details</button>
        <details><summary>Why this matches</summary>{row.reasons.length ? row.reasons.map((reason, index) => <p key={index}>{reason.value}<br /><small>{reason.source} · {reason.field} · {new Date(reason.fetchedAt).toLocaleDateString()}</small></p>) : <p>Suggested by the provider's related-series lists.</p>}</details>
        {row.ownership === 'unknown' && <p className="muted">Library ownership could not be checked. Add will verify it.</p>}{row.ownership === 'ambiguous' && <p className="banner warning">Conflicting library identities; resolve them before adding.</p>}
      </div>
      <div className="result-action"><button onClick={() => onSeed(row.item)}>More like this</button>{row.libraryItemId ? <Link to="/library/$id" params={{ id: String(row.libraryItemId) }}>Open in library</Link> : addedId(row.item) ? <span>Added</span> : <button className="btn-accent" disabled={busy || !canAdd || row.addability !== 'supported'} onClick={() => { onAdd(row.item); heading.current?.focus() }}>Add</button>}</div>
    </li>)}</ul>
  </section>
}
