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
    await expect(panel).toContainText('1 entries need attention')
    await panel.getByText('Account for 1 entries').click()
    await expect(panel).toContainText('forgotten.rar')
    await expect(panel).toContainText('No download record')
    await panel.getByRole('button', { name: 'Review import' }).click()
    await expect(page.getByLabel('Path', { exact: true })).toHaveValue(join(root, 'forgotten.rar'))
    await panel.getByRole('button', { name: 'Delete files…' }).click()
    await expect(panel.getByText('Delete these 1 files permanently?')).toBeVisible()
    await panel.getByRole('button', { name: 'Confirm delete' }).click()
    await expect(panel).toContainText('0 entries need attention')
    await page.goto('/settings')
    await expect(page.getByLabel('Download storage folders', { exact: true })).toHaveValue(root)
  } finally {
    await request.put('/api/v1/settings', { data: { completedRoots: previous.completedRoots ?? '' } })
    await request.post('/api/v1/system/tasks/downloads.inventory/run')
    rmSync(root, { recursive: true, force: true })
  }
})
