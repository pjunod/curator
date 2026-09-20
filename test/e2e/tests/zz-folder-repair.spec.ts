import { expect, test } from '@playwright/test'

test('folder repairs span pages, retain failures, and recheck restored folders', async ({ page }) => {
  let missing = Array.from({ length: 26 }, (_, i) => ({
    id: 9000 + i, kind: 'movie', title: `Repair movie ${i + 1}`,
    path: `/media/old-${i + 1}`, reason: i === 1 ? 'inaccessible' : 'missing',
  }))
  const calls: { id: number; expectedPath: string; path: string }[] = []
  await page.route('**/api/v1/library/scan/report*', async (route) => {
    await route.fulfill({ json: {
      scannedAt: new Date().toISOString(), rootsScanned: 1, itemsScanned: 0,
      filesLinked: 0, filesRemoved: 0, unmatchedDirs: [], missingItems: missing,
      missingPaths: missing.map((item) => item.path),
    } })
  })
  await page.route('**/api/v1/library/*/repair-folder', async (route) => {
    const id = Number(route.request().url().split('/').at(-2))
    calls.push({ id, ...route.request().postDataJSON() })
    if (id === 9000) {
      await route.fulfill({ status: 400, json: { message: 'Selected folder contains no media files' } })
    } else {
      missing = missing.filter((item) => item.id !== id)
      await route.fulfill({ status: 204 })
    }
  })
  await page.goto('/settings')
  await page.getByText('Folders needing attention (26)', { exact: true }).click()
  const panel = page.locator('.missing-folders')
  await expect(panel).toContainText('Titles awaiting their first download are not listed.')
  await expect(panel).toContainText('Folder cannot be accessed')
  await panel.getByRole('button', { name: 'Repair folders…' }).click()
  await panel.getByLabel('Existing folder for Repair movie 1', { exact: true }).fill('/media/empty')
  await panel.getByRole('button', { name: 'Next ›' }).click()
  await panel.getByLabel('Existing folder for Repair movie 26', { exact: true }).fill('/media/found-26')
  await panel.getByRole('button', { name: 'Apply all repairs (2)' }).click()
  await expect(panel.getByRole('status')).toHaveText('1 folder repaired · 1 still need attention')
  expect(calls).toEqual([
    { id: 9000, expectedPath: '/media/old-1', path: '/media/empty' },
    { id: 9025, expectedPath: '/media/old-26', path: '/media/found-26' },
  ])
  await expect(panel.getByRole('alert')).toContainText('Selected folder contains no media files')
  await expect(panel.getByLabel('Existing folder for Repair movie 1', { exact: true })).toHaveValue('/media/empty')
  missing = [] // the drive is restored before the next check
  await panel.getByRole('button', { name: 'Check again' }).click()
  await expect(panel.getByRole('status')).toHaveText('0 folder issues remain.')
  await expect(panel.locator('details')).toHaveCount(0)
})
