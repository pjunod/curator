import type { Page } from '@playwright/test'

// The browser fixtures model the public unified projection, not two independent
// pages merged in the browser. API tests cover real SQL identities and counts.
export function activityPage(rows: any[], url: string) {
 const p=new URL(url).searchParams, view=p.get('view')??'progress', search=(p.get('q')??'').toLowerCase(), offset=Number(p.get('offset')??0),limit=Number(p.get('limit')??50)
 const items=rows.map(row=>row.download?{...row,category:row.category==='active'||row.category==='queued'?'progress':row.category==='imported'?'history':row.category}:{download:row,title:row.libraryTitle??row.title,kind:'movie',destination:'',updatedAt:row.addedAt,category:row.category??(row.importState?'progress':row.importControl?.pendingPublications||row.importControl?.retainedPublications||row.importControl?.heldStages||row.state==='awaiting_import'||row.submissionPhase==='uncertain'?'attention':['imported','failed','notified'].includes(row.state)?'history':'progress')})
 .filter(item=>`${item.title} ${item.download.title}`.toLowerCase().includes(search))
 const counts:Record<string,number>={all:items.length,progress:0,attention:0,history:0};items.forEach(item=>counts[item.category]++)
 const selected=items.filter(item=>item.category===view)
 return {items:selected.slice(offset,offset+limit),counts,total:selected.length,hasMore:offset+limit<selected.length,retentionDays:30}
}
export async function mockActivity(page:Page, rows: ()=>any[]) { await page.route('**/api/v1/activity?*', route=>route.fulfill({json:activityPage(rows(),route.request().url())})) }
export async function openAttempt(page:Page, title: string) { const row=page.locator('.activity-attempt').filter({hasText:title}).first();await row.getByRole('button',{name:'Details',exact:true}).click();return page.getByRole('dialog').last() }
