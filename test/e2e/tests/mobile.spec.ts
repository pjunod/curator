import { expect, test } from '@playwright/test'

// The phone shell (iPhone-sized viewport, run after the desktop suite so
// the library is populated): bottom tabs, the More sheet, the calendar
// agenda, scrollable tables, and the PWA install surface.
test.describe.configure({ mode: 'serial' })

test('bottom tabs navigate; sidebar is gone', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('.mobile-tabs')).toBeVisible()
  await expect(page.locator('.sidebar')).toHaveCount(0)

  await page.locator('.mobile-tab', { hasText: 'Wanted' }).click()
  await expect(page.getByRole('heading', { name: 'Wanted' })).toBeVisible()
  await page.locator('.mobile-tab', { hasText: 'Activity' }).click()
  await expect(page.getByRole('heading', { name: 'Activity' })).toBeVisible()
})

test('More sheet carries search, nav, and the theme picker', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: 'More' }).click()

  const sheet = page.getByRole('dialog', { name: 'More' })
  await expect(sheet).toBeVisible()
  await expect(sheet.getByRole('searchbox', { name: 'Search media and settings' })).toBeVisible()
  await expect(sheet.getByRole('group', { name: 'Theme' })).toBeVisible()

  // Navigating from the sheet closes it.
  await sheet.getByRole('link', { name: 'Settings' }).click()
  await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()
  await expect(page.getByRole('dialog', { name: 'More' })).toHaveCount(0)
})

test('calendar shows the agenda, not the month grid', async ({ page }) => {
  await page.goto('/calendar')
  await expect(page.getByRole('heading', { name: 'Calendar' })).toBeVisible()
  await expect(page.locator('.cal-agenda')).toBeVisible()
  await expect(page.locator('.cal-grid')).toHaveCount(0)
})

test('library keeps its grouped sections and cards render', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('.lib-section-head').first()).toBeVisible()
  await expect(page.locator('.poster-card').first()).toBeVisible()
})

test('PWA: manifest served with the right type, service worker registers', async ({ page, request }) => {
  const res = await request.get('/manifest.webmanifest')
  expect(res.status()).toBe(200)
  expect(res.headers()['content-type']).toContain('manifest')
  const manifest = await res.json()
  expect(manifest.name).toBe('Monarr')
  expect(manifest.display).toBe('standalone')
  expect(manifest.icons.length).toBeGreaterThanOrEqual(3)

  await page.goto('/')
  const registered = await page.evaluate(async () => {
    if (!('serviceWorker' in navigator)) return 'unsupported'
    const reg = await navigator.serviceWorker.ready
    return reg?.active ? 'active' : 'none'
  })
  expect(registered).toBe('active')
})
