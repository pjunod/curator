import { expect, test } from '@playwright/test'

test('recovery readiness stays advisory and Settings Resolve imports an exact receipt file', async ({ page, request }) => {
  const previous = await (await request.get('/api/v1/import/recovery/settings')).json()
  const selectedPath = 'Season 01/Episode 01.mkv'
  let submitted: { paths?: string[] } | undefined
  await page.route('**/api/v1/downloads/deliveries?*', route => route.fulfill({ json: { items: [{
    receiptId: 733, clientId: 2, downloadId: 73, generationKey: 'generation-73', mediaItemId: 8, copyId: 0,
    deliveryId: 'delivery-733', publicationGeneration: 'publication-733', manifestDigest: 'a'.repeat(64),
    runnerInstanceId: 'runner-2', jobId: '73', requestToken: 'transfer-73', publicationPath: '/processing/73',
    mappedPath: '/completed/73', readyAt: new Date().toISOString(), terminalOutcome: 'SUCCESS',
    keepSourceOnFailure: false, state: 'imported', acceptance: 'accepted', consumerReceiptId: 'consumer-733',
    attempts: 1, attemptHistory: [], manifestFiles: [{ path: selectedPath, sizeBytes: 1024, sha256: 'b'.repeat(64) }],
    sourceFileOutcomes: [], sourceCleanup: { status: 'succeeded', attempts: 1 },
    sourceCleanupAttempts: 0, directoryCleanupState: 'pending', directoryCleanupAttempts: 0, curatorFiles: [],
  }], nextBeforeId: 0 } }))
  await page.route('**/api/v1/downloads/deliveries/733/import', async route => {
    submitted = route.request().postDataJSON()
    await route.fulfill({ status: 202, body: '' })
  })

  try {
    await page.goto('/settings')
    const sections = page.getByRole('navigation', { name: 'Settings sections' })
    await sections.getByRole('button', { name: 'Dev', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Enable Runner recovery' })).toBeVisible()
    await page.getByLabel('Curator recovery mount').fill('/missing/e2e-recovery')
    await page.getByLabel('Enable automatic recovery status refresh').check()
    await page.getByRole('button', { name: 'Save recovery settings' }).click()
    await expect.poll(async () => (await (await request.get('/api/v1/import/recovery/settings')).json()).settings.enabled).toBe(true)
    const advisory = page.locator('.recovery-advisory')
    await expect(advisory).toContainText('local mount available')
    await expect(advisory).toContainText('unmet')

    // Readiness remains advisory. The settings save does not block resolving a
    // durable, accepted Runner receipt through the shared Resolve surface.
    await page.getByLabel('Enable automatic recovery status refresh').uncheck()
    await page.getByRole('button', { name: 'Save recovery settings' }).click()
    await sections.getByRole('button', { name: 'Resolve', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Resolve', exact: true })).toBeVisible()
    await expect(page.getByText(/No accepted Runner delivery receipt/)).toHaveCount(0)
    // Resolve opens the receipt archive by default; don't toggle it closed.
    const trace = page.locator('.delivery-lifecycle').filter({
      has: page.locator('summary', { hasText: 'Runner handoff · 73 · imported' }),
    })
    await expect(trace).toBeVisible()
    await trace.locator(':scope > summary').click()
    await trace.getByText('Per-file outcomes (0 publication files)', { exact: true }).click()
    await trace.getByRole('checkbox', { name: `Select ${selectedPath} for import` }).check()
    await trace.getByRole('button', { name: 'Queue import selected (1)' }).click()
    await expect.poll(() => submitted).toEqual({ paths: [selectedPath] })
    await expect(trace.getByRole('status')).toContainText('Selected manifest files queued')
  } finally {
    await request.put('/api/v1/import/recovery/settings', { data: previous.settings })
  }
})
