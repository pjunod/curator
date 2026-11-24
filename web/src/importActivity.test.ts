import { describe, expect, it } from 'vitest'
import { canRetryImport, importActivityLabel, importActivityReason, importProgress } from './importActivity'
import type { ImportActivityItem } from './api'
const fixture = (): ImportActivityItem => ({category:'attention',title:'Film',kind:'movie',destination:'/library/Film',updatedAt:'2026-11-09T12:00:00Z',download:{id:1,mediaItemId:2,title:'Film.release',state:'failed',progress:1,protocol:'manual',quality:'1080p',addedAt:'2026-11-08T12:00:00Z',importPath:'/completed/film',importControl:{generationKey:'g',epoch:'1',outcome:'',heldStages:0,retainedPublications:0,pendingPublications:0}}})
describe('import recovery eligibility',()=>{
 it('retries retained sources but blocks custody, workers and unknown controls',()=>{
  expect(canRetryImport(fixture())).toBe(true)
  for(const patch of [{importState:'queued' as const},{submissionPhase:'uncertain'},{importControl:{...fixture().download.importControl!,outcome:'pending' as const}},{importControl:{...fixture().download.importControl!,heldStages:1}},{importPath:'',savePath:''}]){
   const item=fixture();Object.assign(item.download,patch);expect(canRetryImport(item)).toBe(false)
  }
 })
 it('never treats completed download progress as measured copy bytes',()=>{
  const item=fixture();item.category='active';item.download.stage='importing';item.download.total=100
  expect(importProgress(item)).toBeNull()
  item.download.bytes=25;item.download.bytesPerSecond=5
  expect(importProgress(item)).toMatchObject({percent:25,secondsLeft:15})
  item.download.stage='downloading';expect(importProgress(item)).toBeNull()
 })
 it('keeps healthy active stage ownership ahead of normal staged custody',()=>{
 const item=fixture();item.category='active';item.download.importState='running';item.download.stageDetail='Copying movie';item.download.importControl!.heldStages=1
 expect(importActivityLabel(item)).toBe('Copying movie')
 })
 it('separates admission, worker queue and interrupted state',()=>{
  const item=fixture();item.category='queued';expect(importActivityLabel(item)).toContain('admission')
  item.download.importState='queued';expect(importActivityLabel(item)).toContain('worker')
  item.category='attention';item.download.importState=undefined;item.download.state='importing'
  expect(importActivityLabel(item)).toContain('no importer')
 })
})

it('keeps the failure pill short and exposes the original cause separately',()=>{
 const item=fixture();item.download.error='publication import_failed: nothing importable: episode 8 outside planned import allowlist'
 expect(importActivityLabel(item)).toBe('Import failed')
 expect(importActivityReason(item)).toBe('nothing importable: episode 8 outside planned import allowlist')
 item.download.error=undefined;item.download.importControl!.heldStages=1
 expect(importActivityReason(item)).toContain('scratch files')
})

it('shows resolved cancellation without claiming retained files and preserves its failure reason',()=>{
 const item=fixture();item.category='history';item.download.importControl!.outcome='cancelled'
 item.download.error='publication import_failed: existing stage retained; explicit retry or custody review required'
 expect(importActivityLabel(item)).toBe('Cancelled')
 expect(importActivityReason(item)).toBe('existing stage retained; explicit retry or custody review required')
 for (const field of ['heldStages','pendingPublications','retainedPublications'] as const) {
  item.download.importControl![field]=1
  expect(importActivityLabel(item)).toBe('Review required')
  item.download.importControl![field]=0
 }
 item.category='attention'
 expect(importActivityLabel(item)).toBe('Cancelled · review required')
})
