import { expect,test } from '@playwright/test'
import { mockActivity,openAttempt } from '../activity-fixture'
const item=(id:number,category='attention')=>({category,title:`Import title ${id}`,kind:'movie',destination:'/library/title',updatedAt:'2026-11-09T12:00:00Z',download:{id,mediaItemId:88,title:`release.${id}.1080p`,state:category==='history'?'imported':category==='progress'?'importing':'awaiting_import',progress:1,quality:'1080p',protocol:'manual',addedAt:'2026-11-09T12:00:00Z',importPath:`/completed/${id}`,importControl:{generationKey:`g-${id}`,epoch:'1',outcome:'',pendingPublications:0,retainedPublications:0,heldStages:0},...(category==='progress'?{importState:'running',stage:'importing',stageDetail:'Verifying source identity'}:{})}})
for(const width of [1280,390]) {
 test(`one Activity view retains history, serial retry results and old import links at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850})
  const rows=[item(1,'progress'),item(3),item(4),...Array.from({length:53},(_,i)=>item(i+10,'history'))];await mockActivity(page,()=>rows)
  const retried:number[]=[];await page.route('**/api/v1/queue/*/import',async route=>{const id=Number(route.request().url().split('/').at(-2));retried.push(id);await route.fulfill(id===4?{status:409,json:{message:'Publication requires custody review'}}:{status:204})})
  await page.goto('/imports');await expect(page).toHaveURL(/activity\?scope=imports/);await expect(page.getByRole('heading',{name:'Activity',exact:true})).toBeVisible();await expect(page.getByRole('combobox',{name:'Activity scope'})).toHaveValue('imports')
  await expect(page.locator('[data-activity-id="1"] .progress-track')).toHaveCount(0)
  await page.getByRole('button',{name:/^History 53/}).click();await expect(page.getByText('1–50 of 53')).toBeVisible();await page.getByRole('button',{name:'Next',exact:true}).click();await expect(page.getByText('51–53 of 53')).toBeVisible()
  await page.getByRole('button',{name:/^Needs attention 2/}).click();await page.getByRole('checkbox',{name:'Select retryable imports on this page'}).check()
  const trigger=page.getByRole('button',{name:/^Retry selected/});await trigger.click();const result=page.getByRole('dialog',{name:'Activity action results'});await expect(result).toContainText('Import accepted');await expect(result).toContainText('Publication requires custody review');expect(retried).toEqual([3,4]);await expect(result.getByRole('heading')).toBeInViewport({ratio:1});await page.keyboard.press('Escape');await expect(page.getByRole('heading',{name:'Activity',exact:true})).toBeFocused()
  await page.getByRole('searchbox',{name:'Find a title or release'}).fill('no-such-title');await expect(page.getByRole('heading',{name:'No activity matches this search'})).toBeVisible()
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
 })
 test(`selected-attempt cancellation and retained-file protection remain visible at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850});const rows=Array.from({length:18},(_,i)=>item(100+i,'progress'));rows[17].download.importControl.heldStages=1;await mockActivity(page,()=>rows)
  let identity:unknown;await page.route('**/api/v1/queue/117/cancel-import',async route=>{identity=route.request().postDataJSON();await route.fulfill({status:409,json:{message:'Import attempt changed; refresh before cancelling.'}})})
  await page.route('**/api/v1/queue/117/import-review',route=>route.fulfill({json:{attempt:rows[17].download.importControl,writerActive:true,publications:[],stages:[{id:'one',source:'/source',stage:'/stage',target:'/library/file',state:'held'}]}}))
  await page.goto('/activity');const details=await openAttempt(page,'Import title 117');await details.getByRole('button',{name:'Cancel import',exact:true}).click();const result=page.getByRole('dialog',{name:'Activity action results'});await expect(result).toContainText('Import attempt changed');await expect(result.getByRole('heading')).toBeInViewport({ratio:1});expect(identity).toEqual({generationKey:'g-117',epoch:'1'});await page.keyboard.press('Escape');await expect(page.locator('[data-activity-id="117"]').getByRole('button',{name:'Details',exact:true})).toBeFocused()
  await page.locator('[data-activity-id="117"]').getByRole('button',{name:'Review retained files'}).click();const review=page.getByRole('dialog',{name:'Review retained files',exact:true});await expect(review).toContainText('An importer still owns this work');await expect(review.getByRole('button',{name:'Attest all listed bytes independently handled'})).toBeDisabled();await expect(review.getByRole('heading')).toBeInViewport({ratio:1})
 })
 test(`unavailable activity is not an empty queue and storage is absent from the daily view at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850});await page.route('**/api/v1/activity?*',route=>route.fulfill({status:503,json:{message:'Local status unavailable'}}));await page.goto('/activity');await expect(page.getByRole('alert')).toContainText('Activity and counts are unavailable');await expect(page.getByText('Nothing in progress',{exact:true})).toHaveCount(0);await expect(page.getByRole('heading',{name:'Download storage',exact:true})).toHaveCount(0);await expect(page.getByRole('button',{name:'Resolve',exact:true})).toHaveCount(0)
 })
}

for (const width of [1280, 390]) {
 test(`cleaned import failures show their cause as history at ${width}px`, async ({page}) => {
  await page.setViewportSize({width,height:850})
  const row = item(501,'history')
  Object.assign(row.download,{state:'failed',error:'publication import_failed: nothing importable: runner-swc-s02e08.avi: episode 8 outside planned import allowlist',publications:[{id:882,kind:'completed',state:'removed',path:'/completed/rejected',files:[]}]})
  await mockActivity(page,()=>[row])
  await page.goto('/activity')
  await page.getByRole('button',{name:/^History 1/}).click()
  const card=page.locator('[data-activity-id="501"]')
  await expect(card.locator('.activity-attempt-heading .pill')).toHaveText('failed')
  await expect(card.getByText('nothing importable: runner-swc-s02e08.avi: episode 8 outside planned import allowlist',{exact:true})).toBeInViewport({ratio:1})
  await expect(card.getByText('Publication files have been removed. This record is retained history.',{exact:true})).toBeInViewport({ratio:1})
  await expect(card.getByRole('checkbox')).toHaveCount(0)
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
 })
}
