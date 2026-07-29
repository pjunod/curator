import { expect, test } from '@playwright/test'

// Phase 0 walking-skeleton smoke: the compiled binary serves the embedded
// UI, the API answers, health is green, the scheduler is alive, and events
// flow bus → SSE → browser.

// Health includes the metadata-provider check; give it a key so the suite
// is order-independent with library.spec.ts.
test.beforeAll(async ({ request }) => {
  await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
})

test.describe('API', () => {
  test('system status reports the essentials', async ({ request }) => {
    const res = await request.get('/api/v1/system/status')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.appName).toBe('Monarr')
    expect(body.dbSchemaVersion).toBeGreaterThanOrEqual(1)
    expect(typeof body.version).toBe('string')
    expect(Date.parse(body.startedAt)).not.toBeNaN()
  })

  test('health runs all checks green', async ({ request }) => {
    const res = await request.get('/api/v1/health')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.overall).toBe('ok')
    // The static checks are a fixed set; download clients each register their
    // own `client:<name>` on top, so the list grows with the configuration.
    // Asserting an exact array here made this test fail every time somebody
    // added a check OR added a client, which is the wrong thing to be
    // sensitive to. Pin the ones that must always exist, and require that
    // anything extra is a client check rather than a surprise.
    //
    // The list is written once and used twice, because the two copies drifted:
    // `imports` was registered in cmd/monarr without being added here, and the
    // "anything extra is a client check" guard turned that into a red suite on
    // main. A new static check belongs in this array.
    const STATIC_CHECKS = [
      'data-directory',
      'database',
      'imports',
      'library-folders',
      'metadata-provider',
      'web-ui',
    ]
    const names: string[] = body.checks.map((c: { name: string }) => c.name)
    for (const required of STATIC_CHECKS) {
      expect(names).toContain(required)
    }
    for (const n of names.filter((n) => !STATIC_CHECKS.includes(n))) {
      expect(n).toMatch(/^client:|^mediaserver:/)
    }
  })

  test('unknown task returns 404 with an error body', async ({ request }) => {
    const res = await request.post('/api/v1/system/tasks/ghost/run')
    expect(res.status()).toBe(404)
    const body = await res.json()
    expect(body.message).toContain('ghost')
  })
})

test.describe('UI', () => {
  test('dashboard renders live status from the API', async ({ page, request }) => {
    const status = await (await request.get('/api/v1/system/status')).json()

    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
    // The version card shows the real version reported by the API.
    await expect(page.locator('.card-value').first()).toHaveText(status.version)
    // The health pill settles on healthy.
    await expect(page.locator('.pill')).toHaveText('healthy')
  })

  test('system page lists health checks and scheduled tasks', async ({ page }) => {
    await page.goto('/system')
    await expect(page.getByRole('heading', { name: 'System' })).toBeVisible()

    for (const check of [
      'database',
      'data-directory',
      'web-ui',
      'metadata-provider',
      'library-folders',
    ]) {
      await expect(page.getByRole('cell', { name: check, exact: true })).toBeVisible()
    }
    // Every check is green. A count would have to be revised each time a
    // check or a download client is added; "nothing is unhealthy" is the
    // thing actually being asserted and does not go stale.
    await expect(page.locator('.pill-warning')).toHaveCount(0)
    await expect(page.locator('.pill-error')).toHaveCount(0)
    expect(await page.locator('.pill-ok').count()).toBeGreaterThanOrEqual(5)

    await expect(page.getByRole('cell', { name: 'health.check' })).toBeVisible()
    await expect(page.getByRole('cell', { name: 'db.wal-checkpoint' })).toBeVisible()
  })

  test('running a task emits a live event over SSE', async ({ page }) => {
    await page.goto('/system')
    // Wait for the SSE stream to connect (green dot on the Live events panel).
    await expect(page.locator('h2 .dot-ok')).toBeVisible()

    await page
      .locator('tr', { has: page.getByRole('cell', { name: 'db.wal-checkpoint' }) })
      .getByRole('button', { name: 'Run now' })
      .click()

    const log = page.locator('.event-log')
    await expect(log.locator('li').first()).toContainText('task.completed', { timeout: 10_000 })
    await expect(log.locator('li').first()).toContainText('db.wal-checkpoint')
  })

  test('SPA deep links and unknown routes serve the shell', async ({ page }) => {
    const res = await page.goto('/definitely/not/a/route')
    expect(res?.status()).toBe(200)
    await expect(page.locator('.wordmark')).toContainText('monarr')
  })
})
