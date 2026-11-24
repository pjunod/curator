import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { confirmLibraryVolume, previewLibraryVolume } from './api'

export function LibraryVolumeReview({id}:{id:number}){
 const qc=useQueryClient(),[ack,setAck]=useState<string|null>(null)
 const review=useQuery({queryKey:['library-volume-review',id],queryFn:()=>previewLibraryVolume(id),retry:false,refetchInterval:10000})
 const current=review.isError?undefined:review.data
 const accepted=!!current&&ack===current.fingerprint
 const confirm=useMutation({mutationFn:()=>{if(!current||!accepted||current.blocker)throw new Error('Review and acknowledge the current volume before confirming.');return confirmLibraryVolume(id,current.fingerprint,true)},onSuccess:()=>{setAck(null);for(const key of ['library-volume-review','library','library-item','discover-items','library-discovery-items','library-seed-availability','library-discovery-seeds','discover-add-availability'])void qc.invalidateQueries({queryKey:[key]})}})
 return <section aria-label="Library volume review">
  {review.isPending&&<p role="status">Checking the current volume and library custody…</p>}
  {review.error&&<p role="alert">The volume review is unavailable: {review.error.message}</p>}
  {current&&<><p className="path-chip">{current.state.path}</p><dl className="volume-review-facts"><dt>Previously verified volume</dt><dd>{current.state.expectedIdentity||'Not yet verified'}</dd><dt>Current volume</dt><dd>{current.observedIdentity||'Unavailable'}{current.empty?' · empty':''}</dd><dt>Affected catalogue records</dt><dd>{current.state.fileRecords} file records</dd><dt>Active work</dt><dd>{current.state.activeWork}</dd></dl><p>This may be an unavailable mount or an intentional replacement. Returning the original volume needs no rebind.</p><p>Use the current volume only after checking it is the intended library. Future scans may remove catalogue records for files missing here. This confirmation does not delete filesystem files.</p>{current.blocker&&<p role="alert">{current.blocker}</p>}<label className="volume-review-ack"><input type="checkbox" checked={accepted} onChange={e=>setAck(e.target.checked?current.fingerprint:null)} />I have checked this library volume and want Curator to use it for future scans and availability checks.</label></>}
  {confirm.error&&<p role="alert">{confirm.error.message}</p>}{confirm.isSuccess&&<p role="status">Volume binding confirmed. Previous availability proofs were invalidated. Scan disk to verify current files.</p>}
  <div className="row-actions"><button disabled={review.isFetching||confirm.isPending} onClick={()=>{setAck(null);confirm.reset();void review.refetch()}}>Refresh review</button><button className="btn-accent" disabled={!accepted||!current||!!current.blocker||confirm.isPending||review.isError} onClick={()=>confirm.mutate()}>{confirm.isPending?'Confirming…':'Use current volume'}</button></div>
 </section>
}
