import { expect, test } from '@playwright/test'

test('Wanted groups titles, preserves siblings, and sends every explicit scope', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('monarr-wanted-page-size', '25'))
  const wanted = [
    {
      wantableId: 'episode:101:1:10', mediaItemId: 101, title: 'Example Show',
      detail: 'S01E10', missing: true, current: '', copy: '', copyId: 0,
      reason: 'missing', kind: 'series', season: 1, episode: 10,
    },
    {
      wantableId: 'episode:101:1:2:c4', mediaItemId: 101, title: 'Example Show',
      detail: 'S01E02', missing: true, current: '', copy: '4K', copyId: 4,
      reason: 'missing', kind: 'series', season: 1, episode: 2,
    },
    {
      wantableId: 'episode:101:2:1', mediaItemId: 101, title: 'Example Show',
      detail: 'S02E01', missing: false, current: 'HDTV 720p', copy: '', copyId: 0,
      reason: 'upgrade', kind: 'series', season: 2, episode: 1,
    },
    ...Array.from({ length: 26 }, (_, index) => ({
      wantableId: `movie:${200 + index}`, mediaItemId: 200 + index,
      title: `Movie ${String(index + 1).padStart(2, '0')}`, detail: '(2024)',
      missing: true, current: '', copy: '', copyId: 0,
      reason: 'missing', kind: 'movie',
    })),
  ]
  const submitted: Record<string, unknown>[] = []
  const runs = new Map<string, Record<string, unknown>>()
  const cancelled: string[] = []

  await page.route('**/api/v1/wanted', (route) => route.fulfill({ json: wanted }))
  await page.route('**/api/v1/system/tasks', (route) => route.fulfill({ json: [] }))
  await page.route('**/api/v1/wanted/searches**', async (route) => {
    const url = new URL(route.request().url())
    if (route.request().method() === 'POST' && url.pathname.endsWith('/wanted/searches')) {
      const scope = route.request().postDataJSON() as Record<string, unknown>
      submitted.push(scope)
      const runId = `run-${submitted.length}`
      const scopeLabel = ['All wanted items', 'All missing items', 'Missing in Example Show', 'Example Show S01E02', 'All upgrade items'][submitted.length - 1]
      const run = {
        runId, scope, scopeLabel, status: 'queued', createdAt: new Date().toISOString(),
        targetDelayMs: scope.targetDelayMs, selected: 1, processed: 0,
        searched: 0, skipped: 0, failed: 0, grabbed: 0,
      }
      runs.set(runId, run)
      await route.fulfill({ status: 202, json: run })
      return
    }
    if (url.pathname.endsWith('/results')) {
      await route.fulfill({ json: { items: [], limit: 100, offset: 0 } })
      return
    }
    if (url.pathname.endsWith('/wanted/searches')) {
      await route.fulfill({ json: { items: [...runs.values()].reverse() } })
      return
    }
    const [, runId, action] = /\/wanted\/searches\/([^/]+)(?:\/([^/]+))?$/.exec(url.pathname) ?? []
    const run = runId ? runs.get(runId) : undefined
    if (!runId || !run) {
      await route.fulfill({ status: 404, json: { message: 'Unknown run' } })
      return
    }
    if (action === 'cancel' && route.request().method() === 'POST') {
      cancelled.push(runId)
      run.status = 'cancelled'
      run.finishedAt = new Date().toISOString()
    }
    await route.fulfill({ json: run })
  })

  await page.goto('/wanted')
  await expect(page.getByText('1–25 of 27').first()).toBeVisible()
  await expect(page.getByRole('button', { name: 'Search all Missing (28)' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Search all Upgrade (1)' })).toBeVisible()

  const expand = page.getByRole('button', { name: 'Expand Example Show' })
  await expand.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Collapse Example Show' })).toHaveAttribute('aria-expanded', 'true')
  const region = page.getByRole('region', { name: 'Example Show wanted targets' })
  await expect(region.locator('.wanted-child-detail')).toHaveText(['S01E02', 'S01E10', 'S02E01'])

  await page.getByLabel('Search wanted items').fill('4K')
  await expect(region.locator('.wanted-child-row')).toHaveCount(3)
  await page.getByLabel('Search wanted items').fill('')

  await page.getByRole('button', { name: 'Search all now (29)' }).click()
  await page.getByRole('button', { name: 'Search all Missing (28)' }).click()
  await page.getByLabel('Filter wanted reason').selectOption('missing')
  await page.getByRole('button', { name: 'Search Missing in show (2)' }).click()
  await page.getByRole('button', { name: 'Search episode' }).first().click()

  await page.getByRole('button', { name: 'Search all Upgrade (1)' }).click()

  const cards = page.getByRole('region', { name: 'Wanted searches' }).getByRole('article')
  await expect(cards).toHaveCount(5)
  await expect(cards.filter({ hasText: 'Queued' })).toHaveCount(5)
  expect(submitted.map(({ requestId, ...scope }) => scope)).toEqual([
    { scope: 'all', targetDelayMs: 1000 },
    { scope: 'reason', reason: 'missing', targetDelayMs: 1000 },
    { scope: 'group', mediaItemId: 101, reason: 'missing', targetDelayMs: 1000 },
    { scope: 'target', wantableId: 'episode:101:1:2:c4', targetDelayMs: 1000 },
    { scope: 'reason', reason: 'upgrade', targetDelayMs: 1000 },
  ])
  const requestIds = submitted.map(({ requestId }) => requestId)
  expect(requestIds).toEqual(Array(5).fill(expect.stringMatching(/.+/)))
  expect(new Set(requestIds).size).toBe(5)

  await page.getByRole('button', { name: 'Cancel Missing in Example Show', exact: true }).click()
  await expect(cards.filter({ hasText: 'Missing in Example Show' })).toContainText('Cancelled')
  await expect(cards.filter({ hasText: 'Queued' })).toHaveCount(4)
  expect(cancelled).toEqual(['run-3'])
})
