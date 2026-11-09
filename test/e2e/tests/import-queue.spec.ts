import { expect, test, type Page } from '@playwright/test'

const makeItem = (id:number,category='attention') => ({ category,title:`Import title ${id}`,kind:'movie',destination:`/library/title-${id}`,updatedAt:'2026-11-09T12:00:00Z',download:{id,mediaItemId:88,title:`release.${id}.1080p`,state:category==='imported'?'imported':category==='active'?'importing':category==='queued'?'downloaded':'failed',progress:1,quality:'WEB 1080p',protocol:'manual',addedAt:'2026-11-08T00:00:00Z',importPath:`/completed/title-${id}`,importControl:{generationKey:`g-${id}`,epoch:'1',outcome:'',pendingPublications:0,retainedPublications:0,heldStages:0},...(category==='active'?{importState:'running',stage:'importing',stageDetail:'Verifying source identity'}:{}),...(category==='queued'?{importState:'queued'}:{})} })

async function visibleDialog(page:Page,name:string,width:number){
 const dialog=page.getByRole('dialog',{name,exact:true});await expect(dialog).toBeVisible()
 const box=await dialog.boundingBox();expect(box).not.toBeNull();expect(box!.x).toBeGreaterThanOrEqual(0);expect(box!.y).toBeGreaterThanOrEqual(0);expect(box!.x+box!.width).toBeLessThanOrEqual(width+1);expect(box!.y+box!.height).toBeLessThanOrEqual(851)
 return dialog
}

for(const width of [1280,375]){
 test(`import queue filters history, reports unknown progress, and retries at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850})
  const items=[makeItem(1,'active'),makeItem(2,'queued'),makeItem(3),makeItem(4),...Array.from({length:53},(_,i)=>makeItem(i+10,'imported'))]
  const retried:number[]=[]
  await page.route('**/api/v1/imports?*',async route=>{
   const p=new URL(route.request().url()).searchParams
   const filter=p.get('filter'),q=p.get('q')?.toLowerCase()??'',offset=Number(p.get('offset'))
   const searched=items.filter(i=>i.title.toLowerCase().includes(q))
   const counts:Record<string,number>={all:searched.length,active:0,queued:0,attention:0,imported:0}
   searched.forEach(i=>counts[i.category]++)
   const rows=searched.filter(i=>filter==='all'||i.category===filter)
   await route.fulfill({json:{items:rows.slice(offset,offset+50),counts,total:rows.length,hasMore:offset+50<rows.length,retentionDays:30}})
  })
  await page.route('**/api/v1/queue/*/import',async route=>{
   const id=Number(route.request().url().split('/').at(-2));retried.push(id)
   await route.fulfill(id===4?{status:409,json:{message:'Publication requires custody review'}}:{status:204})
  })
  await page.goto('/imports')
  await expect(page.getByRole('heading',{name:'Import queue',exact:true})).toBeVisible()
  const active=page.locator('[data-import-id="1"]')
  await expect(active).toContainText('Byte progress is not available')
  await expect(active.getByRole('progressbar')).toHaveCount(0)
  await expect(page.locator('[data-import-id="2"]')).toContainText('Waiting for an import worker')
  await page.getByRole('button',{name:/^Imported 53/}).click()
  await expect(page.getByText('1–50 of 53')).toBeVisible()
  await page.getByRole('button',{name:'Next',exact:true}).click()
  await expect(page.getByText('51–53 of 53')).toBeVisible()
  await expect(page.locator('[data-import-id="62"]')).toBeVisible()
  await page.getByRole('button',{name:/^Needs attention 2/}).click()
  await page.getByRole('checkbox',{name:'Select retryable imports on this page'}).check()
  const trigger=page.getByRole('button',{name:'Retry selected (2)'})
  await trigger.click()
  const result=await visibleDialog(page,'Import action results',width)
  await expect(result).toContainText('Import retry accepted')
  await expect(result).toContainText('Publication requires custody review')
  expect(retried).toEqual([3,4])
  await page.keyboard.press('Escape');await expect(trigger).toBeFocused()
  const size=await page.evaluate(()=>({scroll:document.documentElement.scrollWidth,client:document.documentElement.clientWidth}))
  expect(size.scroll).toBeLessThanOrEqual(size.client+1)
  await page.getByRole('searchbox',{name:'Filter imports'}).fill('does not exist')
  await expect(page.getByText('No imports match this search.')).toBeVisible()
 })

 test(`import custody review and lower-row cancellation stay in viewport at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850})
  const rows=Array.from({length:18},(_,i)=>makeItem(100+i,'active'))
  rows[17].download.importControl.heldStages=1
  let identity:unknown
  await page.route('**/api/v1/imports?*',route=>route.fulfill({json:{items:rows,counts:{all:18,active:18,queued:0,attention:0,imported:0},total:18,hasMore:false,retentionDays:0}}))
  await page.route('**/api/v1/queue/117/cancel-import',async route=>{identity=route.request().postDataJSON();await route.fulfill({status:409,json:{message:'Import attempt changed; refresh before cancelling.'}})})
  await page.route('**/api/v1/queue/117/import-review',route=>route.fulfill({json:{attempt:rows[17].download.importControl,writerActive:true,publications:[],stages:[{id:'stage-one',source:'/completed/file',stage:'/library/stage',target:'/library/file',state:'held'}]}}))
  await page.goto('/imports')
  const row=page.locator('[data-import-id="117"]')
  const trigger=row.getByRole('button',{name:'Cancel import',exact:true});await trigger.click()
  const dialog=await visibleDialog(page,'Import action results',width)
  await expect(dialog).toContainText('Import attempt changed')
  expect(identity).toEqual({generationKey:'g-117',epoch:'1'})
  await page.keyboard.press('Escape');await expect(trigger).toBeFocused()
  const review=row.getByRole('button',{name:'Review retained import'});await review.click()
  const custody=await visibleDialog(page,'Review retained import',width)
  await expect(custody).toContainText('An importer still owns this work')
  await expect(custody.getByRole('button',{name:'Attest all listed bytes independently handled'})).toBeDisabled()
  await page.keyboard.press('Escape');await expect(review).toBeFocused()
 })
}
