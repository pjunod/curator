import { expect, test } from '@playwright/test'

for(const width of [1280,375]){
 test(`durable cancellation and retained review stay visible at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850})
  const identity={generationKey:'acquisition-1',epoch:'9007199254740993'}
  let pending=false,retained=false
  let cancellation:unknown,reviewRequest:unknown
  const row=()=>({id:871,mediaItemId:88,title:'The retained NFS import',state:'importing',progress:0.8,protocol:'manual',quality:'WEB-DL 1080p',addedAt:'2026-11-01T00:00:00Z',...(!pending&&!retained?{importState:'running'}:{}),importControl:{...identity,outcome:retained?'cancelled':pending?'pending':'',pendingPublications:pending?1:0,retainedPublications:retained?1:0}})
  await page.route('**/api/v1/queue?*',route=>route.fulfill({json:[row()]}))
  await page.route('**/api/v1/queue/summary*',route=>route.fulfill({json:{total:1,active:1,counts:{importing:1},retentionDays:7}}))
  await page.route('**/api/v1/queue/871/cancel-import',async route=>{cancellation=route.request().postDataJSON();pending=true;await route.fulfill({status:202,json:{status:'cancel_requested'}})})
  await page.route('**/api/v1/queue/871/import-review',async route=>{
   if(route.request().method()==='POST'){reviewRequest=route.request().postDataJSON();pending=false;retained=true;await route.fulfill({json:{...identity,outcome:'cancelled',pendingPublications:0,retainedPublications:1}});return}
   await route.fulfill({json:{attempt:row().importControl,writerActive:false,publications:[{id:'placement:one',epoch:identity.epoch,source:'/qnap/source/movie.mkv',target:'/qnap/library/movie.mkv',outcome:retained?'reconciled':'pending',resolution:retained?'retained_for_review':''}],stages:[]}})
  })
  await page.goto('/activity')
  await page.getByRole('button',{name:'Cancel import',exact:true}).click()
  expect(cancellation).toEqual(identity)
  await expect(page.getByText('Cancellation pending · publication review required')).toBeVisible()
  await expect(page.getByRole('button',{name:'Restart import',exact:true})).toHaveCount(0)
  const trigger=page.getByRole('button',{name:'Review retained import',exact:true})
  await trigger.click()
  const dialog=page.getByRole('dialog',{name:'Review retained import',exact:true})
  await expect(dialog).toBeVisible()
  const box=await dialog.boundingBox();expect(box).not.toBeNull();expect(box!.x).toBeGreaterThanOrEqual(0);expect(box!.y).toBeGreaterThanOrEqual(0);expect(box!.x+box!.width).toBeLessThanOrEqual(width+1);expect(box!.y+box!.height).toBeLessThanOrEqual(851)
  const finalize=dialog.getByRole('button',{name:'Finalize cancellation and retain uncertain bytes'})
  await expect(finalize).toBeDisabled()
  await dialog.getByRole('checkbox').check()
  await finalize.click()
  expect(reviewRequest).toEqual({...identity,action:'retain_uncertain',ids:['placement:one'],acknowledged:true})
  await expect(dialog.getByRole('status')).toContainText('Review recorded')
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()
  await expect(page.getByText('Cancelled · retained publication requires Library review')).toBeVisible()
 })
}

for(const width of [1280,375]){
 test(`lower-row cancellation conflict and completed stage review at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850})
  const rows=Array.from({length:18},(_,i)=>({id:900+i,mediaItemId:88,title:`Import ${i}`,state:'importing',importState:'running',progress:0.5,protocol:'manual',quality:'1080p',addedAt:'2026-11-01T00:00:00Z',importControl:{generationKey:`generation-${i}`,epoch:'2',outcome:'',pendingPublications:0,retainedPublications:0,heldStages:0}}))
  rows[17].importControl.heldStages=1
  await page.route('**/api/v1/queue?*',route=>route.fulfill({json:rows}))
  await page.route('**/api/v1/queue/summary*',route=>route.fulfill({json:{total:18,active:18,counts:{importing:18}}}))
  await page.route('**/api/v1/queue/917/cancel-import',route=>route.fulfill({status:409,json:{message:'Import attempt changed; refresh before cancelling.'}}))
  await page.goto('/activity')
  const trigger=page.getByRole('button',{name:'Cancel import',exact:true}).last();await trigger.click()
  const dialog=page.getByRole('dialog',{name:'Activity action failed'});await expect(dialog.getByRole('alert')).toContainText('Import attempt changed')
  const box=await dialog.boundingBox();expect(box!.y).toBeGreaterThanOrEqual(0);expect(box!.y+box!.height).toBeLessThanOrEqual(851);expect(box!.x+box!.width).toBeLessThanOrEqual(width+1)
  await page.keyboard.press('Escape');await expect(trigger).toBeFocused()
  // A later successful epoch still exposes the old interrupted stage.
  rows[17].state='imported';rows[17].importState='';rows[17].importControl.outcome='completed'
  await page.reload()
  await expect(page.getByRole('button',{name:'Review retained import',exact:true})).toHaveCount(1)
 })
}
