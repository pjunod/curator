import { expect, test } from '@playwright/test'

test.beforeAll(async ({ request }) => {
  await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key', semanticRankingEnabled: false } })
})

test('integrated recommendations enforce evidence, teen and language filters', async ({ request }) => {
  const res = await request.post('/api/v1/metadata/recommendations', { data: { kind: 'series', query: 'gay-themed series, no teen dramas', filters: { originalLanguage: 'en' } } })
  expect(res.status()).toBe(200)
  expect(res.headers()['cache-control']).toBe('no-store')
  const body = await res.json()
  expect(body.ranking).toBe('metadata_only')
  expect(body.results.map((r: { item: { title: string } }) => r.item.title)).toEqual(['Adult Gay Story'])
  expect(body.results[0].reasons.length).toBeGreaterThan(0)
  expect(body.applied.filters.theme).toBe('gay_male')
  const invalid = await request.post('/api/v1/metadata/recommendations', { data: { kind: 'series', filters: { hideInLibrary: null } } })
  expect(invalid.status()).toBe(400)
})

for (const width of [1280, 390]) {
  test(`description inference, details and fresh title seed at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 850 })
    await page.goto('/add?kind=series')
    await page.getByRole('button', { name: 'Describe what you want', exact: true }).click()
    const section = page.getByRole('region', { name: 'Describe series' })
    await section.getByLabel('Describe what you want', { exact: true }).fill('gay-themed series, no teen dramas')
    await section.getByRole('button', { name: 'Find series' }).click()
    await expect(section.getByLabel('Theme', { exact: true })).toHaveValue('gay_male')
    await expect(section.locator('.result-title').filter({ hasText: 'Adult Gay Story' })).toBeVisible()
    await expect(section.locator('.result-title').filter({ hasText: 'Teen Gay Story' })).toHaveCount(0)
    await section.getByLabel('Describe what you want', { exact: true }).fill('lesbian romance')
    await section.getByRole('button', { name: 'Find series' }).click()
    await expect(section.getByLabel('Theme', { exact: true })).toHaveValue('lesbian')
    await expect(section.locator('.result-title').filter({ hasText: 'Lesbian Love Story' })).toBeVisible()
    expect(page.url()).not.toContain('lesbian')
    await section.getByRole('button', { name: 'View details', exact: true }).click()
    await expect(page.getByRole('dialog', { name: 'Lesbian Love Story' })).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(section.locator('.result-title').filter({ hasText: 'Lesbian Love Story' })).toBeVisible()
    await page.getByRole('button', { name: 'Title search', exact: true }).click()
    await page.locator('.add-controls').getByRole('searchbox').fill('test show')
    const titleRow = page.locator('.result').filter({ hasText: 'The Test Show' })
    await expect(titleRow).toBeVisible()
    const next = page.waitForRequest((r) => r.url().endsWith('/metadata/recommendations') && r.method() === 'POST')
    await titleRow.getByRole('button', { name: 'More like this' }).click()
    const payload = (await next).postDataJSON()
    expect(payload.query).toBe('')
    expect(payload.filters).toEqual({ hideInLibrary: true })
    await expect(section.getByLabel('Theme', { exact: true })).toHaveValue('')
    await expect(section.getByText('Similar to:', { exact: false })).toBeVisible()
    expect(await page.locator('body').evaluate((node) => node.scrollWidth <= window.innerWidth)).toBe(true)
  })
}

test('leaving description mode cancels an in-flight request and ignores its response', async ({ page }) => {
  let finish: (() => void) | undefined
  let arrived: (() => void) | undefined
  const started = new Promise<void>((resolve) => { arrived = resolve })
  await page.route('**/api/v1/metadata/recommendations', async (route) => {
    arrived?.()
    await new Promise<void>((resolve) => { finish = resolve })
    await route.fulfill({ json: { state: 'ready', applied: { filters: { theme: 'gay_male' } }, results: [], warnings: [], checkedCount: 99, coverage: 'bounded' } }).catch(() => {})
  })
  await page.goto('/add?kind=series')
  await page.getByRole('button', { name: 'Describe what you want', exact: true }).click()
  await page.getByLabel('Describe what you want', { exact: true }).fill('gay series')
  await page.getByRole('button', { name: 'Find series' }).click()
  await started
  await page.getByRole('button', { name: 'Title search', exact: true }).click()
  finish?.()
  await page.getByRole('button', { name: 'Describe what you want', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Find series' })).toBeEnabled()
  await expect(page.getByText(/99 candidates checked/)).toHaveCount(0)
})

test('recommendation Add uses the normal library flow and cached ownership refresh', async ({ page, request }) => {
  const roots = await (await request.get('/api/v1/rootfolders')).json()
  if (!roots.some((r: { kind: string }) => r.kind === 'series' || r.kind === 'mixed')) {
    const created = await request.post('/api/v1/rootfolders', { data: { path: process.env.E2E_MEDIA_ROOT!, kind: 'series' } })
    expect(created.ok()).toBeTruthy()
  }
  await page.goto('/add?kind=series&mode=describe')
  const section = page.getByRole('region', { name: 'Describe series' })
  await section.getByLabel('Describe what you want', { exact: true }).fill('gay-themed series, no teen dramas')
  await section.getByLabel('Original language', { exact: true }).selectOption('en')
  await section.getByRole('button', { name: 'Find series' }).click()
  const row = section.locator('.result').filter({ hasText: 'Adult Gay Story' })
  await expect(row).toBeVisible()
  await page.getByLabel('Search on add', { exact: true }).uncheck()
  const added = page.waitForResponse((r) => r.url().endsWith('/api/v1/library') && r.request().method() === 'POST')
  await row.getByRole('button', { name: 'Add', exact: true }).click()
  const response = await added
  expect(response.status()).toBe(201)
  expect(response.request().postDataJSON()).toMatchObject({ kind: 'series', tmdbId: 7201, tvdbId: 720100, hydrationSource: 'tmdb', searchNow: false })
  await expect(row).toHaveCount(0)
  const cached = await request.post('/api/v1/metadata/recommendations', { data: { kind: 'series', query: 'gay-themed series, no teen dramas', filters: { originalLanguage: 'en' } } })
  const body = await cached.json()
  expect(body.results).toHaveLength(0)
  expect(body.hiddenOwnedCount).toBe(1)
})
