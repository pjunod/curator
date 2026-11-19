import { mockActivity } from '../activity-fixture'
import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })
for (const width of [390, 1280]) {
 test(`per-indexer request limit stays explicit and in viewport at ${width}px`, async ({page})=>{
  await page.setViewportSize({width,height:844})
  const indexers=Array.from({length:30},(_,i)=>({id:i+1,name:`Provider ${i}`,url:'http://example.invalid',protocol:'torrent',dailyRequestCap:50,enabled:true}))
  await page.route('**/api/v1/indexers',r=>r.fulfill({json:indexers}))
  let saved: Record<string, unknown> | undefined
  await page.route('**/api/v1/indexers/30',r=>{saved=r.request().postDataJSON();return r.fulfill({json:{...indexers[29],dailyRequestCap:0}})})
  await page.goto('/settings#indexers')
  const panel = page.locator('#indexers')
  const trigger=panel.getByRole('button',{name:'Request limit',exact:true}).last()
  await trigger.scrollIntoViewIfNeeded();await trigger.click()
  const dialog=page.getByRole('dialog').last();await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('heading')).toBeInViewport({ratio:1});await expect(dialog.getByRole('heading')).toBeFocused()
  await expect(dialog.getByText(/applies to RSS, automatic search and manual search for this indexer/)).toBeVisible()
  const limit = dialog.getByLabel('Limit API requests')
  await expect(limit).toBeChecked()
  const input=dialog.getByRole('spinbutton');await expect(input).toHaveValue('50');await expect(input).toBeInViewport({ratio:1})
  await page.screenshot({ path: test.info().outputPath(`indexer-limit-${width}.png`) })
  await limit.uncheck()
  const save=dialog.getByRole('button',{name:'Save request limit'});await expect(save).toBeInViewport({ratio:1});await save.click()
  await expect.poll(() => saved).toEqual({dailyRequestCap:0})
  await expect(dialog).toHaveCount(0);await expect(trigger).toBeFocused()
 })
}

for (const width of [390, 1280]) {
 test(`reservation release explains duplicate risk and stays visible at ${width}px`, async ({page})=>{
  await page.setViewportSize({width,height:844})
  const rows=Array.from({length:40},(_,i)=>({id:i+1,mediaItemId:9876,title:`Show.S01.Pending${i}`,state:'grabbed',progress:0,protocol:'torrent',quality:'WEB-DL 1080p',addedAt:'2026-10-01T00:00:00Z',submissionPhase:'uncertain',reservationActionable:true,parkedReason:'Submission uncertain; custody retained'}))
  await mockActivity(page,()=>rows)
  await page.route('**/api/v1/queue/summary',r=>r.fulfill({json:{counts:{grabbed:40},active:40,total:40,retentionDays:30}}))
  await page.goto('/activity')
  await page.getByRole('button',{name:/^Needs attention /}).click()
  const trigger=page.getByRole('button',{name:'Review reservation',exact:true}).last()
  await trigger.scrollIntoViewIfNeeded();await trigger.click()
  const dialog=page.getByRole('dialog').last();await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('heading')).toBeFocused();await expect(dialog.getByRole('heading')).toBeInViewport({ratio:1})
  const release=dialog.getByRole('button',{name:'Release reservation',exact:true})
  await expect(release).toBeEnabled();await expect(release).toBeInViewport({ratio:1})
  await expect(dialog.getByText(/may result in a duplicate/)).toBeInViewport({ratio:1})
  await page.keyboard.press('Escape');await expect(page.getByRole('dialog',{name:'Review episode reservation',exact:true})).toHaveCount(0);await expect(trigger).toBeFocused()
 })
}
