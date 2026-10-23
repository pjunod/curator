import { expect, test } from '@playwright/test'

test('open lifecycle archive refreshes asynchronous retry results and stops polling when closed', async ({ page }) => {
  let listRequests = 0
  let sourceCleanupStatus = 'retry'
  const observedStatuses: string[] = []

  await page.route('**/api/v1/downloads/deliveries?*', async route => {
    listRequests++
    observedStatuses.push(sourceCleanupStatus)
    await route.fulfill({ json: { items: [{
      receiptId: 811, clientId: 2, downloadId: 81, generationKey: 'generation-81', mediaItemId: 8, copyId: 0,
      deliveryId: 'delivery-811', publicationGeneration: 'publication-811', manifestDigest: 'a'.repeat(64),
      runnerInstanceId: 'runner-2', jobId: '81', requestToken: 'transfer-81', publicationPath: '/processing/81',
      mappedPath: '/completed/Lifecycle.Polling', readyAt: new Date().toISOString(), terminalOutcome: 'SUCCESS',
      keepSourceOnFailure: false, state: 'imported', acceptance: 'accepted', consumerReceiptId: 'consumer-811',
      attempts: 1, attemptHistory: [], manifestFiles: [], sourceFileOutcomes: [],
      sourceCleanup: { status: sourceCleanupStatus, attempts: 2 }, sourceCleanupAttempts: 0,
      directoryCleanupState: 'pending', directoryCleanupAttempts: 0, curatorFiles: [],
    }], nextBeforeId: 0 } })
  })
  await page.route('**/api/v1/downloads/deliveries/811/retry-runner-cleanup', async route => {
    await route.fulfill({ status: 202, body: '' })
    // The accepted retry is asynchronous. Its first invalidation can still
    // observe the old state; the open archive must pick up the later result.
    setTimeout(() => { sourceCleanupStatus = 'succeeded' }, 2_000)
  })

  await page.goto('/activity')
  const archive = page.locator('.delivery-lifecycle-archive')
  await archive.locator(':scope > summary').click()
  const trace = archive.locator('.delivery-lifecycle')
  await expect(trace).toBeVisible()
  await trace.locator(':scope > summary').click()
  await expect(trace.getByText(/Runner source cleanup: Retry pending/)).toBeVisible()
  const retryButton = trace.getByRole('button', { name: 'Retry Runner source cleanup' })
  await retryButton.click()
  await expect(trace.getByRole('status')).toContainText('Retry queued')
  const traceSummary = trace.locator(':scope > summary')
  // The mutation disables its own trigger while pending. Move focus to the
  // stable trace disclosure after that user action, then prove background
  // polling leaves both focus and viewport position alone.
  await traceSummary.focus()
  const beforeRefresh = await page.evaluate(() => ({ scrollY: window.scrollY, activeText: document.activeElement?.textContent?.trim() }))
  await expect(trace.getByText(/Runner source cleanup: Complete/)).toBeVisible({ timeout: 9_000 })
  await expect(traceSummary).toBeFocused()
  expect(await page.evaluate(() => window.scrollY)).toBe(beforeRefresh.scrollY)
  expect(await page.evaluate(() => document.activeElement?.textContent?.trim())).toBe(beforeRefresh.activeText)
  expect(listRequests).toBeGreaterThanOrEqual(3)
  expect(observedStatuses).toContain('retry')
  expect(observedStatuses).toContain('succeeded')

  await archive.locator(':scope > summary').click()
  const requestsAtClose = listRequests
  await page.waitForTimeout(5_500)
  expect(listRequests).toBe(requestsAtClose)
})

test('a closed download trace stops both its receipt query and initially-open nested archive', async ({ page }) => {
  let receiptRequests = 0
  let archiveRequests = 0
  await page.route('**/api/v1/queue/summary', route => route.fulfill({ json: {
    total: 1, active: 1, counts: { grabbed: 0, downloading: 1, downloaded: 0, awaiting_import: 0, importing: 0, imported: 0, failed: 0 },
  } }))
  await page.route('**/api/v1/queue?*', route => route.fulfill({ json: [{
    id: 812, mediaItemId: 8, clientId: 2, title: 'Nested.Polling', state: 'downloading', progress: 0.5,
    protocol: 'torrent', quality: 'WEBDL-1080p', addedAt: new Date().toISOString(),
  }] }))
  await page.route('**/api/v1/downloads/deliveries?*', async route => {
    const url = new URL(route.request().url())
    if (url.searchParams.has('downloadId')) {
      receiptRequests++
      await route.fulfill({ json: { items: [], nextBeforeId: 0 } })
    } else {
      archiveRequests++
      await route.fulfill({ json: { items: [], nextBeforeId: 0 } })
    }
  })

  await page.goto('/activity')
  const row = page.getByRole('row').filter({ hasText: 'Nested.Polling' }).first()
  await row.getByRole('button', { name: 'Show handoff' }).click()
  const lookup = page.locator('.delivery-lifecycle-lookup')
  await lookup.locator(':scope > summary').click()
  const archive = lookup.locator('.delivery-lifecycle-archive')
  await expect(archive).toBeVisible()
  await expect.poll(() => archiveRequests).toBeGreaterThan(0)
  await page.waitForTimeout(5_500)
  expect(receiptRequests).toBeGreaterThanOrEqual(2)
  expect(archiveRequests).toBeGreaterThanOrEqual(2)

  await lookup.locator(':scope > summary').click()
  const closedReceiptRequests = receiptRequests
  const closedArchiveRequests = archiveRequests
  await page.waitForTimeout(5_500)
  expect(receiptRequests).toBe(closedReceiptRequests)
  expect(archiveRequests).toBe(closedArchiveRequests)
})
