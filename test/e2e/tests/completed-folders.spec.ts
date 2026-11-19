import { expect, test } from '@playwright/test'
import { activityIdentity, attempt, mockActivity } from '../activity-fixture'

test.use({ serviceWorkers: 'block' })

test('Storage shows measured capacity, last cleanup run and download root configuration', async ({ page }) => {
  const at = '2026-11-18T16:05:00Z'
  await page.route('**/api/v1/rootfolders', route => route.fulfill({ json: [
    { id: 1, path: '/media/library', kind: 'movie', accessible: true, freeBytes: 24 * 1024 ** 3, totalBytes: 80 * 1024 ** 3 },
    { id: 2, path: '/media/books', kind: 'book', accessible: false, freeBytes: 0 },
  ] }))
  let fullInventoryReads = 0
  await page.route('**/api/v1/downloads/inventory', route => {
    fullInventoryReads++
    return route.fulfill({ json: { roots: [] } })
  })
  await page.route('**/api/v1/system/storage-observations', route => route.fulfill({ json: {
    cleanup: { logicalBytesRemoved: 2 * 1024 ** 3, filesRemoved: 3, lastRemovalAt: at },
    roots: [
      { owner: 'Curator', role: 'downloads', path: '/media/completed', observedAt: at,
        status: 'unavailable', error: 'Could not reach the download drive.' },
      { owner: 'Runner', name: 'Living room', role: 'processing', path: '/runner/processing', observedAt: at,
        status: 'available', freeBytes: 0, totalBytes: 10 * 1024 ** 3 },
    ],
  } }))
  await page.route('**/api/v1/system/tasks', route => route.fulfill({ json: [
    { name: 'downloads.cleanup', intervalSeconds: 300, lastRunAt: at, lastError: 'Could not reach the download drive.', running: false },
  ] }))
  await page.goto('/system#storage')
  const library = page.getByRole('region', { name: 'Library folders' })
  await expect(library).toContainText('24.0 GB free of 80.0 GB')
  await expect(library).toContainText('Available space unknown')
  await expect(library).toContainText('Unavailable')
  const downloads = page.getByRole('region', { name: 'Download folders' })
  await expect(downloads).toContainText('/media/completed')
  await expect(downloads).toContainText('Could not reach the download drive.')
  await expect(downloads).toContainText('Available space unknown')
  await expect(downloads.getByText('Runner · Living room · processing', { exact: false })).toBeVisible()
  await expect(downloads).toContainText('0 B free of 10.0 GB')
  expect(fullInventoryReads).toBe(0)
  await expect(page.getByRole('region', { name: 'Automatic cleanup' })).toContainText('Cleanup reported an error')
  await expect(page.locator('#storage')).toContainText('3 files · 2.0 GB in logical file sizes')
  await expect(page.locator('#storage')).not.toContainText('reclaimed')
  await page.getByRole('link', { name: 'Manage storage folders' }).click()
  await expect(page.getByLabel('Download storage folders', { exact: true })).toBeVisible()
})

test('held transfers survive reload and use the same generation and retry action', async ({ page }) => {
  const identity = activityIdentity(991)
  const control = {
    version: 1, revision: '9007199254740993', lifecycle: 'held', cause: 'capacity', stage: 'download_write',
    retry_policy: 'resume_same_job', message: 'Waiting for storage capacity', instance: 'fixture',
  }
  await mockActivity(page, () => [{
    id: 991, mediaItemId: 1, title: 'Held.Show.S01E01', state: 'held', progress: 1,
    protocol: 'usenet', category: 'attention', control,
    actions: [{ kind: 'retry', label: 'Retry', enabled: true }],
  }])
  let submitted: Record<string, unknown> | undefined
  const outcome = () => ({
    id: submitted?.requestId, action: 'retry', state: 'completed', total: 1, completed: 1, succeeded: 1, blocked: 0,
    targets: [{ ...identity, state: 'succeeded', title: 'Held.Show.S01E01' }],
  })
  await page.route('**/api/v1/activity/actions', async route => {
    submitted = route.request().postDataJSON()
    await route.fulfill({ status: 202, json: outcome() })
  })
  await page.route('**/api/v1/activity/operations/*', route => submitted ? route.fulfill({ json: outcome() }) : route.fulfill({ status: 404 }))
  await page.goto('/activity')
  await page.getByRole('button', { name: /^Needs attention / }).click()
  await expect(attempt(page, 991)).toContainText('Waiting for storage capacity')
  await page.reload()
  await page.getByRole('button', { name: /^Needs attention / }).click()
  await attempt(page, 991).getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Retry jobs' })).toContainText('1 succeeded')
  expect(submitted).toMatchObject({ action: 'retry', targets: [identity] })
})

test('publication verification names the measured Activity stage', async ({ page }) => {
  await mockActivity(page, () => [{
    id: 992, mediaItemId: 1, title: 'Verified.Movie', state: 'downloading',
    progress: 1, total: 9000, protocol: 'usenet', stage: 'publication_verify',
    stagePeer: 'Runner', stageSince: new Date().toISOString(),
  }])
  await page.goto('/activity')
  await expect(attempt(page, 992).locator('.pill')).toHaveText('Verifying publication')
})
