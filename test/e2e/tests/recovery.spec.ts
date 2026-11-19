import { expect, test } from '@playwright/test'

test('Storage offers its configuration and Activity instead of a historical receipt console', async ({ page }) => {
  const receipts: string[] = []
  await page.route('**/api/v1/downloads/deliveries?*', route => {
    receipts.push(route.request().url())
    return route.fulfill({ json: { items: [] } })
  })
  await page.goto('/settings')
  const sections = page.getByRole('navigation', { name: 'Settings sections' })
  await expect(sections.getByRole('button', { name: 'Dev', exact: true })).toHaveCount(0)
  await expect(sections.getByRole('button', { name: 'Resolve', exact: true })).toHaveCount(0)
  await page.goto('/system#storage')
  const storage = page.locator('#storage')
  await expect(storage.getByRole('heading', { name: 'Automatic cleanup' })).toBeVisible()
  await expect(storage.getByRole('link', { name: 'Manage storage folders' })).toHaveAttribute('href', '/settings#library-folders')
  await expect(storage.getByRole('button', { name: 'Import files…' })).toHaveCount(0)
  await expect(storage.locator('.delivery-lifecycle-archive')).toHaveCount(0)
  await storage.getByRole('link', { name: 'Job history' }).click()
  await expect(page.getByRole('button', { name: /^History / })).toHaveAttribute('aria-pressed', 'true')
  expect(receipts).toEqual([])
})
