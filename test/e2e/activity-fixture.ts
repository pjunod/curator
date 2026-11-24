import type { Locator, Page } from '@playwright/test'

export function activityIdentity(id: number, generationKey = `generation-${id}`, epoch = '0') {
  return { id, generationKey, epoch }
}

export function activityPage(rows: any[], url: string) {
  const params = new URL(url).searchParams
  const view = params.get('view') ?? 'progress'
  const search = (params.get('q') ?? '').toLowerCase()
  const offset = Number(params.get('offset') ?? 0)
  const limit = Number(params.get('limit') ?? 50)
  const items = rows.map(row => {
    const download = row.download ?? row
    const category = row.category ?? (
      download.importState ? 'progress'
        : download.importControl?.heldStages || download.state === 'awaiting_import' || download.submissionPhase === 'uncertain' ? 'attention'
        : ['imported', 'failed', 'notified'].includes(download.state) ? 'history'
        : 'progress'
    )
    const identity = download.id > 0 ? activityIdentity(download.id, download.importControl?.generationKey, download.importControl?.epoch) : undefined
    return {
      download, title: download.libraryTitle ?? download.title, kind: 'movie', destination: '', updatedAt: download.addedAt ?? new Date().toISOString(),
      ...(row.download ? row : {}),
      category,
      identity,
      prerequisite: row.prerequisite, operation: row.operation,
      reason: row.reason ?? download.control?.message ?? download.heldReason ?? download.error,
      actions: row.actions ?? (identity && category !== 'history' ? [{ kind: 'delete', label: 'Delete', enabled: true }] : []),
    }
  }).filter(item => `${item.title} ${item.download.title}`.toLowerCase().includes(search))
  const counts: Record<string, number> = { all: items.length, progress: 0, attention: 0, history: 0 }
  for (const item of items) counts[item.category]++
  const selected = items.filter(item => item.category === view)
  return { items: selected.slice(offset, offset + limit), counts, total: selected.length, hasMore: offset + limit < selected.length, retentionDays: 30 }
}

export async function mockActivity(page: Page, rows: () => any[]) {
  await page.route('**/api/v1/activity?*', route => route.fulfill({ json: activityPage(rows(), route.request().url()) }))
}

export const attempt = (page: Page, id: number): Locator => page.locator(`[data-activity-id="${id}"]`)
