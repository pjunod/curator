import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { getAvailability, getLibraryDiscoveryItems, getLibraryDiscoverySeeds, type SearchResult } from './api'
import { AvailabilityBadge } from './AvailabilityBadge'
import './LibraryDiscovery.css'

export function LibraryDiscovery({renderResults}:{renderResults:(items:SearchResult[])=>ReactNode}){
 const [search,setSearch]=useState(''),[offset,setOffset]=useState(0),[seed,setSeed]=useState<SearchResult|null>(null)
 const seeds=useQuery({queryKey:['library-discovery-seeds',search,offset],queryFn:()=>getLibraryDiscoverySeeds(search,offset),retry:false})
 const current=useQuery({queryKey:['library-seed-availability',seed?.kind,seed?.tmdbId,seed?.availability?.libraryItemId],queryFn:()=>getAvailability([seed!]),enabled:!!seed,refetchInterval:10000,retry:false})
 const value=current.isError?undefined:current.data?.[0]
 const changed=!!seed&&!!current.data&&(value?.catalog!=='present'||value.libraryItemId!==seed.availability?.libraryItemId)
 const ready=!!seed&&value?.catalog==='present'&&!changed&&!current.isError
 return <div className="library-discovery">
  <p className="muted">Choose a movie or show in your library. Suggestions use catalogue metadata, not watch history.</p>
  <section className="library-seed-picker" aria-label="Choose a library title">
   <label>Find a library title<input type="search" maxLength={200} value={search} onChange={e=>{setSearch(e.target.value);setOffset(0)}} /></label>
   {seeds.isPending&&<p role="status">Loading library titles…</p>}
   {seeds.error&&<p role="alert">Library titles are unavailable. <button onClick={()=>void seeds.refetch()}>Try again</button></p>}
   {seeds.data&&<><label>Base suggestions on<select aria-label="Base suggestions on" value={seed?`${seed.kind}:${seed.tmdbId}`:''} onChange={e=>setSeed(seeds.data!.items.find(item=>`${item.kind}:${item.tmdbId}`===e.target.value)??null)}><option value="">Choose a movie or show…</option>{seed&&!seeds.data.items.some(item=>item.kind===seed.kind&&item.tmdbId===seed.tmdbId)&&<option value={`${seed.kind}:${seed.tmdbId}`}>{seed.title}</option>}{seeds.data.items.map(item=><option key={`${item.kind}:${item.tmdbId}`} value={`${item.kind}:${item.tmdbId}`} disabled={item.availability?.catalog!=='present'}>{item.title} ({item.year||'year unknown'}) · {item.kind==='series'?'Show':'Movie'}{item.availability?.catalog!=='present'?' · identity needs review':''}</option>)}</select></label><div className="row-actions"><button disabled={offset===0} onClick={()=>setOffset(Math.max(0,offset-50))}>Previous titles</button><span className="muted small">Page {Math.floor(offset/50)+1}</span><button disabled={!seeds.data.hasMore} onClick={()=>setOffset(offset+50)}>More titles</button></div>{seeds.data.items.length===0&&<p>No matching movie or show with a TMDB identity. Choose another search or use Browse all.</p>}</>}
  </section>
  {!seed&&<div className="empty-state"><h2>Start with a title you already have</h2><p>Related titles and collection entries appear here. For shows, review the seasons still to get.</p></div>}
  {seed&&<section className="selected-library-seed"><div className="lib-section-bar"><div><h2>Based on {seed.title}</h2><p className="muted">Primary copy · known aired regular episodes</p></div><button onClick={()=>setSeed(null)}>Change title</button></div>{current.isPending?<p role="status">Checking library identity…</p>:<AvailabilityBadge value={value} checked />}{(current.isError||changed)&&<p role="alert">The library identity could not be verified or changed. Select it again before loading suggestions. <button onClick={()=>void current.refetch()}>Refresh library check</button></p>}{value?.catalog==='present'&&value.presence==='unknown'&&<p className="muted">Presence needs a successful library scan. Library checks expire after 13 hours; new or changed files stay unknown until checked. <Link to="/settings" hash="scan">Scan disk</Link>.</p>}</section>}
  {ready&&seed&&<>
   <LibraryShelf seed={seed} shelf="related" renderResults={renderResults} />
   {seed.kind==='movie'&&<LibraryShelf seed={seed} shelf="collection" renderResults={renderResults} />}
   {seed.kind==='series'&&<section className="lib-section"><h2>More seasons to review</h2><p className="muted">Available episodes and offload intent stay separate. Review a season before changing monitoring or searching.</p>{!value?.seasons?.length?<p>No regular season episode inventory is available yet.</p>:<div className="library-season-grid">{value.seasons.filter(sn=>(sn.expected>0||sn.unknownDates>0)&&(value.presence==='unknown'||sn.present<sn.expected||sn.unknownDates>0)).map(sn=><article key={sn.number}><h3>Season {sn.number}</h3><p>{value.presence==='unknown'?`${sn.present} confirmed episodes · remaining availability unknown`:`${sn.present} of ${sn.expected} aired episodes available`}</p>{sn.offloaded>0&&<p className="muted">{sn.offloaded} offloaded episodes</p>}{sn.unknownDates>0&&<p className="muted">{sn.unknownDates} episodes have unknown air dates</p>}<Link to="/library/$id" params={{id:String(value.libraryItemId)}} hash={`season-${sn.number}`}>Review Season {sn.number}</Link></article>)}</div>}{value?.presence==='present'&&value.seasons?.every(sn=>sn.present===sn.expected&&sn.unknownDates===0)&&<p>All known aired regular episodes are available in the primary copy.</p>}</section>}
  </>}
 </div>
}
function LibraryShelf({seed,shelf,renderResults}:{seed:SearchResult;shelf:'related'|'collection';renderResults:(items:SearchResult[])=>ReactNode}){
 const query=useQuery({queryKey:['library-discovery-items',seed.kind,seed.tmdbId,seed.availability?.libraryItemId,shelf],queryFn:()=>getLibraryDiscoveryItems(seed,shelf),retry:false,refetchInterval:30000,refetchOnWindowFocus:true})
 const items=query.data?.items??[]
 const visible=shelf==='collection'?items.filter(item=>!(item.availability?.catalog==='present'&&item.availability.presence==='present')):items
 return <section className="lib-section discover-section" aria-label={shelf==='related'?'Related library titles':'Collection entries'}><div className="lib-section-bar"><div><h2>{shelf==='related'?`More like ${seed.title}`:query.data?.title?`Complete ${query.data.title}`:'Complete the collection'}</h2><p className="muted">{shelf==='related'?'TMDB recommendations · first page, up to 30 titles':'Verified TMDB collection membership · available primary copies are omitted'}</p></div></div>{query.isPending&&<p role="status">Loading this shelf…</p>}{query.error?<p role="alert">This shelf could not be refreshed. <button onClick={()=>void query.refetch()}>Try again</button></p>:query.data&&<>{visible.length?renderResults(visible):<p className="muted">{shelf==='collection'?items.length?'Every entry in this collection is available at the last library check.':'TMDB does not identify a collection for this title.':'No recommendations were returned for this title.'}</p>}</>}</section>
}
