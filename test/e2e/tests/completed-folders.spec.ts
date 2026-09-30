import { expect, test } from '@playwright/test'
import { mkdtempSync, writeFileSync, rmSync, realpathSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

test('an empty queue cannot hide unknown completed files', async ({ page, request }) => {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'curator-completed-')))
  const previous = await (await request.get('/api/v1/settings')).json()
  try {
    writeFileSync(join(root, 'forgotten.rar'), Buffer.alloc(1024))
    expect((await request.put('/api/v1/settings', { data: { completedRoots: root } })).ok()).toBeTruthy()
    expect((await request.post('/api/v1/system/tasks/downloads.inventory/run')).ok()).toBeTruthy()
    await expect.poll(async () => (await (await request.get('/api/v1/downloads/inventory')).json()).bytes).toBe(1024)
    await page.goto('/activity')
    const panel = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Download storage', exact: true }) })
    await expect(panel).toContainText('Need attention')
    await expect(panel).toContainText('forgotten.rar')
    await expect(panel).toContainText('No download record')
    await panel.getByRole('button', { name: 'Review import' }).click()
    await expect(page.getByLabel('Path', { exact: true })).toHaveValue(join(root, 'forgotten.rar'))
    await panel.getByRole('button', { name: 'Delete files…' }).click()
    await expect(panel.getByText('Delete 1 entry permanently?')).toBeVisible()
    await panel.getByRole('button', { name: 'Confirm delete' }).click()
    await expect(panel.getByRole('listitem')).toHaveCount(0)
    await expect(panel).toContainText('1 entry deleted.')
    await page.goto('/settings')
    await expect(page.getByLabel('Download storage folders', { exact: true })).toHaveValue(root)
  } finally {
    await request.put('/api/v1/settings', { data: { completedRoots: previous.completedRoots ?? '' } })
    await request.post('/api/v1/system/tasks/downloads.inventory/run')
    rmSync(root, { recursive: true, force: true })
  }
})

// Deterministic large inventory: exercise layout and selection without touching
// real storage. The first test above covers the actual server deletion path.
test('storage pagination, responsive rows and bulk outcomes', async ({ page }) => {
  const root = '/working/monarr'
  const longName = 'A.Very.Long.Release.Name.2160p.WEB-DL.DDP5.1.H.265-Group'.repeat(3)
  let entries = Array.from({ length: 78 }, (_, index) => ({
    path: `${root}/completed/${String(index).padStart(3, '0')}.${longName}`,
    fingerprint: `fingerprint-${index}`, bytes: 5_100_000_000, files: 2,
    status: index === 0 ? 'active' : 'untracked', reason: index === 0 ? 'Active download' : 'No download record; review or manually import these files', receipts: [],
  }))
  const deleted: string[] = []
  await page.route('**/api/v1/downloads/inventory', (route) => route.fulfill({ json: {
    bytes: entries.length * 5_100_000_000, attention: entries.length - 1,
    roots: [{ path: root, bytes: entries.length * 5_100_000_000, checkedAt: new Date().toISOString(), lastCompleteAt: new Date().toISOString(), error: '', entries }],
  } }))
  await page.route('**/api/v1/downloads/inventory/entry', async (route) => {
    const { path, fingerprint } = route.request().postDataJSON()
    expect(entries.find((entry) => entry.path === path)?.fingerprint).toBe(fingerprint)
    if (path.includes('/001.')) {
      await route.fulfill({ status: 409, json: { message: 'Files changed since the scan' } })
      return
    }
    deleted.push(path)
    entries = entries.filter((entry) => entry.path !== path)
    await route.fulfill({ status: 204 })
  })
  await page.goto('/activity')
  const panel = page.getByRole('region', { name: 'Download storage', exact: true })
  await expect(panel.getByRole('listitem')).toHaveCount(25)
  await expect(panel).toContainText('1–25 of 78')
  await expect(panel.getByRole('checkbox', { name: `Select 000.${longName}`, exact: true })).toBeDisabled()
  await panel.getByRole('checkbox', { name: 'Select page', exact: true }).check()
  await expect(panel).toContainText('24 selected')
  await panel.getByRole('button', { name: 'Next ›', exact: true }).first().click()
  await expect(panel).toContainText('26–50 of 78')
  await expect(panel).toContainText('24 selected')
  await panel.getByRole('button', { name: 'Select all 77 eligible', exact: true }).click()
  await expect(panel).toContainText('77 selected')
  await panel.getByRole('button', { name: 'Review import', exact: true }).first().click()
  const firstPath = await page.getByLabel('Path', { exact: true }).inputValue()
  await panel.getByRole('button', { name: 'Review import', exact: true }).nth(1).click()
  await expect(page.getByLabel('Path', { exact: true })).not.toHaveValue(firstPath)
  await page.getByRole('button', { name: 'Close manual import', exact: true }).click()

  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await expect(panel.getByRole('listitem').first().getByRole('button', { name: 'Delete files…' })).toBeVisible()
    expect(await panel.evaluate((node) => node.scrollWidth <= node.clientWidth + 1)).toBeTruthy()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy()
    await page.screenshot({ path: `/tmp/monarr-storage-${width}.png`, fullPage: false })
  }
  // Switching the app's desktop/mobile shell remounts the page. Select again
  // in the mobile shell before verifying the destructive action.
  await panel.getByRole('button', { name: 'Select all 77 eligible', exact: true }).click()
  await panel.getByRole('button', { name: 'Delete selected…', exact: true }).click()
  const confirmation = panel.getByRole('region', { name: 'Confirm storage deletion' })
  await expect(confirmation).toContainText('Delete 77 entries permanently?')
  await expect(confirmation.getByRole('listitem')).toHaveCount(77)
  expect(deleted).toHaveLength(0)
  await confirmation.getByRole('button', { name: 'Confirm delete', exact: true }).click()
  await expect(panel).toContainText('76 entries deleted.', { timeout: 20000 })
  await expect(panel).toContainText('1 failed; those files were not confirmed deleted.')
  expect(deleted).toHaveLength(76)
  expect(deleted.some((path) => path.includes('/000.'))).toBeFalsy()
  await expect(panel.getByRole('list', { name: 'Storage entries' }).getByRole('listitem')).toHaveCount(2)
  await expect(panel).toContainText('1 selected')
  await panel.getByRole('searchbox', { name: 'Search storage' }).fill('no-such-release')
  await expect(panel).toContainText('No entries match')
  await expect(panel).toContainText('0 selected')
})

test('incomplete scans preserve results and disable file actions', async ({ page }) => {
  let running = false
  let lastRunAt = '2026-09-29T12:00:00Z'
  await page.route('**/api/v1/system/tasks', (route) => route.fulfill({ json: [{ name: 'downloads.inventory', running, lastRunAt, lastError: 'context deadline exceeded', intervalSeconds: 300 }] }))
  await page.route('**/api/v1/system/tasks/downloads.inventory/run', async (route) => {
    running = true
    await route.fulfill({ status: 204 })
  })
  await page.route('**/api/v1/downloads/inventory', (route) => route.fulfill({ json: {
    bytes: 5000, attention: 1, roots: [{ path: '/working/monarr', bytes: 5000, checkedAt: lastRunAt, lastCompleteAt: '2026-09-29T11:00:00Z', error: 'context deadline exceeded', entries: [{ path: '/working/monarr/retained.mkv', fingerprint: 'old', bytes: 5000, files: 1, status: 'untracked', reason: 'No download record', receipts: [] }] }],
  } }))
  await page.goto('/activity')
  const panel = page.getByRole('region', { name: 'Download storage', exact: true })
  await expect(panel).toContainText('Showing stale or partial results')
  await expect(panel.getByRole('button', { name: 'Review import' })).toBeDisabled()
  await expect(panel.getByRole('button', { name: 'Delete files…' })).toBeDisabled()
  await expect(panel.getByRole('checkbox', { name: 'Select retained.mkv' })).toBeDisabled()
  await panel.getByRole('button', { name: 'Scan download storage', exact: true }).click()
  await expect(panel.getByRole('button', { name: 'Scanning storage…', exact: true })).toBeDisabled()
  await expect(panel).toContainText('Previous results remain visible')
  running = false
  lastRunAt = '2026-09-29T12:05:00Z'
  await expect(panel).toContainText('Scan failed: context deadline exceeded')
  await expect(panel.getByRole('button', { name: 'Scan download storage', exact: true })).toBeEnabled()
})
