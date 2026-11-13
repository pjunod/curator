import {mkdtempSync,writeFileSync} from 'node:fs'
import {join} from 'node:path'
import {expect,test} from '@playwright/test'
import {mockActivity,openAttempt} from '../activity-fixture'
for(const width of [390,1280]){
 test(`consolidated attempt cards wrap and selected work stays in viewport at ${width}px`,async({page})=>{
  await page.setViewportSize({width,height:850});const rows=Array.from({length:20},(_,i)=>({id:i+1,mediaItemId:1,title:`A.Long.Release.S01E${i}.1080p.WEB-DL`,state:'downloading',progress:.4,protocol:'torrent',quality:'1080p',addedAt:new Date().toISOString()}));await mockActivity(page,()=>rows);await page.goto('/activity');const cards=page.locator('.activity-attempt');await expect(cards).toHaveCount(20);for(const card of await cards.all()){const box=await card.boundingBox();expect(box!.height).toBeLessThan(290)}const detail=await openAttempt(page,rows[19].title);await expect(detail.getByRole('heading')).toBeFocused();await expect(detail.getByRole('heading')).toBeInViewport({ratio:1});expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);await page.keyboard.press('Escape');await expect(cards.last().getByRole('button',{name:'Details',exact:true})).toBeFocused()
 })
}
test('a long manual file list remains bounded in System Storage',async({page})=>{
 const dir=mkdtempSync(join(process.env.E2E_MEDIA_ROOT!,'long-import-'));for(let i=1;i<=30;i++)writeFileSync(join(dir,`Test.Show.S01E${String(i).padStart(2,'0')}.1080p.WEB-DL.mkv`),'x');await page.goto('/system#storage');await page.getByRole('button',{name:'Import files…',exact:true}).click();const panel=page.locator('.manual-import');await panel.getByRole('textbox').first().fill(dir);await panel.getByRole('button',{name:'Scan',exact:true}).click();const scroller=panel.locator('.log-scroll').first();await expect(scroller).toBeVisible();expect(await scroller.evaluate(el=>getComputedStyle(el).maxHeight)).not.toBe('none');expect((await scroller.boundingBox())!.height).toBeLessThanOrEqual(360)
})
