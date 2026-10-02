import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })
for (const width of [390, 1280]) {
 test(`season selection explanation stays in viewport at ${width}px`, async ({page})=>{
  await page.setViewportSize({width,height:844})
  const selection={version:1,trigger:'backlog',target:'WEB-DL 1080p',partial:false,reason:'Compared attainable episode outcomes up to your target; protected existing files are kept.',titles:{pack:'Show.S01.1080p.WEB-DL'},importAllowlists:{pack:[1,2]},plan:{Releases:['pack'],Unserved:[],Cost:{ZeroSeedTorrents:0,UnknownSizes:0,KnownBytes:15*1024**3,UnknownSeedTorrents:1,RedundantEpisodes:0,Transfers:1}},scopes:[]}
  const rows=Array.from({length:40},(_,i)=>({id:i+1,mediaItemId:9876,title:`Show.S01.Pack${i}`,state:'planned',progress:0,protocol:'torrent',quality:'WEB-DL 1080p',addedAt:'2026-10-01T00:00:00Z',planId:1,planState:'admitted',submissionPhase:'pending',selection}))
  await page.route('**/api/v1/queue?**',r=>r.fulfill({json:rows}))
  await page.route('**/api/v1/queue/summary',r=>r.fulfill({json:{counts:{planned:40},active:40,total:40,retentionDays:30}}))
  await page.goto('/activity')
  const trigger=page.getByRole('button',{name:'Why chosen',exact:true}).last()
  await trigger.scrollIntoViewIfNeeded();await trigger.click()
  const dialog=page.getByRole('dialog');await expect(dialog).toBeVisible()
  const heading=dialog.getByRole('heading',{name:'Why chosen'})
  await expect(heading).toBeInViewport({ratio:1});await expect(heading).toBeFocused()
  await expect(dialog.getByText('Completed bounded discovery.',{exact:false})).toBeInViewport({ratio:1})
  await expect(dialog.getByRole('button',{name:'Close',exact:true})).toBeInViewport({ratio:1})
  const box=await dialog.boundingBox();expect(box).not.toBeNull();expect(box!.x).toBeGreaterThanOrEqual(0);expect(box!.x+box!.width).toBeLessThanOrEqual(width)
  await page.keyboard.press('Escape');await expect(dialog).toHaveCount(0);await expect(trigger).toBeFocused()
 })
}

for (const width of [390, 1280]) {
 test(`saved request cap action stays in viewport at ${width}px`, async ({page})=>{
  await page.setViewportSize({width,height:844})
  const indexers=Array.from({length:30},(_,i)=>({id:i+1,name:`Provider ${i}`,url:'http://example.invalid',protocol:'torrent',dailyRequestCap:50,enabled:true}))
  await page.route('**/api/v1/indexers',r=>r.fulfill({json:indexers}))
  await page.route('**/api/v1/indexers/30',r=>r.fulfill({json:{...indexers[29],dailyRequestCap:100}}))
  await page.goto('/settings?tab=acquisition')
  const trigger=page.getByRole('button',{name:'Request cap',exact:true}).last()
  await trigger.scrollIntoViewIfNeeded();await trigger.click()
  const dialog=page.getByRole('dialog');await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('heading')).toBeInViewport({ratio:1});await expect(dialog.getByRole('heading')).toBeFocused()
  const input=dialog.getByRole('spinbutton');await expect(input).toBeInViewport({ratio:1});await input.fill('100')
  const save=dialog.getByRole('button',{name:'Save request cap'});await expect(save).toBeInViewport({ratio:1});await save.click()
  await expect(dialog).toHaveCount(0);await expect(trigger).toBeFocused()
 })
}
