import { mockActivity } from '../activity-fixture'
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
      directoryCleanupState: sourceCleanupStatus, directoryCleanupAttempts: 0, curatorFiles: [],
    }], nextBeforeId: 0 } })
  })
  await page.route('**/api/v1/downloads/deliveries/811/retry-curator-cleanup', async route => {
    await route.fulfill({ status: 202, body: '' })
    // The accepted retry is asynchronous. Its first invalidation can still
    // observe the old state; the open archive must pick up the later result.
    setTimeout(() => { sourceCleanupStatus = 'succeeded' }, 2_000)
  })

  await page.goto('/system#storage')
  const archive = page.locator('.delivery-lifecycle-archive')
  await archive.locator(':scope > summary').click()
  const trace = archive.locator('.delivery-lifecycle')
  await expect(trace).toBeVisible()
  await trace.locator(':scope > summary').click()
  await expect(trace.getByText(/Curator publication cleanup: Retry pending/)).toBeVisible()
  const retryButton = trace.getByRole('button', { name: 'Retry Curator cleanup' })
  await retryButton.click()
  await expect(trace.getByRole('status')).toContainText('Retry queued')
  // The actual retry trigger keeps focus through pending and settled state.
  // No test-side refocus may conceal focus theft by a background refresh.
  await expect(retryButton).toBeFocused()
  const beforeRefresh = await page.evaluate(() => ({ scrollY: window.scrollY, activeText: document.activeElement?.textContent?.trim() }))
  await expect(trace.getByText(/Curator publication cleanup: Complete/)).toBeVisible({ timeout: 9_000 })
  await expect(retryButton).toBeFocused()
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

test('daily Activity never starts the historical receipt console or polling', async ({page})=>{
 let requests=0
 await mockActivity(page,()=>[{id:812,mediaItemId:8,title:'Nested.Polling',state:'downloading',progress:.5,protocol:'torrent',quality:'1080p',addedAt:new Date().toISOString()}])
 await page.route('**/api/v1/downloads/deliveries?*',async route=>{requests++;await route.fulfill({json:{items:[],nextBeforeId:0}})})
 await page.goto('/activity');await page.locator('[data-activity-id="812"]').getByRole('button',{name:'Details',exact:true}).click();await expect(page.locator('.delivery-lifecycle-archive')).toHaveCount(0);await expect(page.locator('.delivery-lifecycle-lookup')).toHaveCount(0);await page.waitForTimeout(5500);expect(requests).toBe(0)
 await page.keyboard.press('Escape');await page.goto('/system#storage');await expect.poll(()=>requests).toBe(1);await expect(page.locator('.delivery-lifecycle-archive')).toHaveCount(0);await page.waitForTimeout(5500);expect(requests).toBe(1)
})
