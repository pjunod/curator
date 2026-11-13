import {expect,test} from '@playwright/test'
import {mockActivity,openAttempt} from '../activity-fixture'
for(const width of [1280,390]) {
 test(`durable cancellation preserves exact epoch and requires explicit retained-file disposition at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850});const identity={generationKey:'acquisition-1',epoch:'9007199254740993'}
  let pending=false,retained=false,cancellation:unknown,reviewRequest:unknown
  const row=()=>({id:871,mediaItemId:88,title:'The retained NFS import',state:'importing',progress:.8,protocol:'manual',quality:'1080p',addedAt:new Date().toISOString(),...(!pending&&!retained?{importState:'running'}:{}),importControl:{...identity,outcome:retained?'cancelled':pending?'pending':'',pendingPublications:pending?1:0,retainedPublications:retained?1:0}})
  await mockActivity(page,()=>[row()])
  await page.route('**/api/v1/queue/871/cancel-import',async route=>{cancellation=route.request().postDataJSON();pending=true;await route.fulfill({status:202,json:{status:'cancel_requested'}})})
  await page.route('**/api/v1/queue/871/import-review',async route=>{
   if(route.request().method()==='POST'){reviewRequest=route.request().postDataJSON();pending=false;retained=true;await route.fulfill({json:{...identity,outcome:'cancelled',pendingPublications:0,retainedPublications:1}});return}
   await route.fulfill({json:{attempt:row().importControl,writerActive:false,publications:[{id:'placement:one',epoch:identity.epoch,source:'/qnap/source/movie.mkv',target:'/qnap/library/movie.mkv',outcome:retained?'reconciled':'pending',resolution:retained?'retained_for_review':''}],stages:[]}})
  })
  await page.goto('/activity');const details=await openAttempt(page,'The retained NFS import');await details.getByRole('button',{name:'Cancel import',exact:true}).click();const result=page.getByRole('dialog',{name:'Activity action results'});await expect(result).toContainText('Cancellation requested');expect(cancellation).toEqual(identity);await page.keyboard.press('Escape')
  await page.getByRole('button',{name:/^Needs attention /}).click();const trigger=page.getByRole('button',{name:'Review retained files',exact:true});await trigger.click();const dialog=page.getByRole('dialog',{name:'Review retained files',exact:true});await expect(dialog.getByRole('heading')).toBeInViewport({ratio:1});const finalize=dialog.getByRole('button',{name:'Finalize cancellation and retain uncertain bytes'});await expect(finalize).toBeDisabled();await dialog.getByRole('checkbox').check();await finalize.click();expect(reviewRequest).toEqual({...identity,action:'retain_uncertain',ids:['placement:one'],acknowledged:true});await expect(dialog.getByRole('status')).toContainText('Review recorded');await page.keyboard.press('Escape');await expect(trigger).toBeFocused()
 })
}

test('retained review acknowledgement is bound to the polled file identities',async({page})=>{
 let epoch='1',polls=0,posts=0
 const row=()=>({id:872,mediaItemId:88,title:'Changing retained records',state:'downloaded',progress:1,protocol:'manual',quality:'1080p',addedAt:new Date().toISOString(),importControl:{generationKey:'exact',epoch,outcome:'pending',pendingPublications:1,retainedPublications:0}})
 await mockActivity(page,()=>[row()])
 await page.route('**/api/v1/queue/872/import-review',async route=>{
  if(route.request().method()==='POST'){posts++;await route.fulfill({status:409,json:{message:'Unexpected submit'}});return}
  polls++;await route.fulfill({json:{attempt:row().importControl,writerActive:false,publications:[{id:`placement:${epoch}`,epoch,source:`/source/${epoch}`,target:'/library/movie',outcome:'pending',resolution:''}],stages:[]}})
 })
 await page.goto('/activity');await page.getByRole('button',{name:/^Needs attention /}).click();await page.getByRole('button',{name:'Review retained files',exact:true}).click()
 const dialog=page.getByRole('dialog',{name:'Review retained files',exact:true}),submit=dialog.getByRole('button',{name:'Finalize cancellation and retain uncertain bytes'})
 await dialog.getByRole('checkbox').check();await expect(submit).toBeEnabled();epoch='2'
 await expect.poll(()=>polls,{timeout:10000}).toBeGreaterThan(1);await expect(dialog).toContainText('/source/2');await expect(dialog.getByRole('checkbox')).not.toBeChecked();await expect(submit).toBeDisabled();expect(posts).toBe(0)
})

test('an open detail never cancels a replacement generation with the same id',async({page})=>{
 let generation='original',requests=0,reads=0
 const row=()=>({id:873,mediaItemId:88,title:'Reused identity',state:'importing',importState:'running',progress:.8,protocol:'manual',quality:'1080p',addedAt:new Date().toISOString(),importControl:{generationKey:generation,epoch:'1',outcome:'',pendingPublications:0,retainedPublications:0}})
 await page.route('**/api/v1/activity?*',async route=>{reads++;const {activityPage}=await import('../activity-fixture');await route.fulfill({json:activityPage([row()],route.request().url())})})
 await page.route('**/api/v1/queue/873/cancel-import',async route=>{requests++;await route.fulfill({status:204})})
 await page.goto('/activity');const dialog=await openAttempt(page,'Reused identity');await expect(dialog.getByRole('button',{name:'Cancel import',exact:true})).toBeEnabled();generation='replacement'
 await expect.poll(()=>reads,{timeout:10000}).toBeGreaterThan(1);await expect(dialog).toContainText('This attempt moved out');await expect(dialog.getByRole('button',{name:'Cancel import',exact:true})).toBeDisabled();expect(requests).toBe(0)
})
