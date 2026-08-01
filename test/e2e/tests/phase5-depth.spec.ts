import { expect, test } from '@playwright/test'

// Phase 5 depth features on the real binary: custom-format scoring visible
// in release search, import list CRUD, the mass editor, and the security
// surface. (Auth enablement itself is covered by Go tests — flipping it
// here would lock out the later specs.)
test.describe.configure({ mode: 'serial' })

test('custom format scores releases in interactive search', async ({ request }) => {
  const res = await request.post('/api/v1/customformats', {
    data: { name: 'E2E group', pattern: '-E2E$', score: 25 },
  })
  expect(res.status()).toBe(201)

  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const cands = await (
    await request.get(`/api/v1/library/${movies[0].id}/releases`)
  ).json()
  const scored = cands.find((c: any) => c.title.endsWith('-E2E'))
  expect(scored.score).toBe(25)
  expect(scored.formats).toContain('E2E group')
  const unscored = cands.find((c: any) => !c.title.endsWith('-E2E'))
  expect(unscored.score).toBe(0)

  // Bad regex is rejected up front.
  const bad = await request.post('/api/v1/customformats', {
    data: { name: 'broken', pattern: '([', score: 1 },
  })
  expect(bad.status()).toBe(400)
})

test('import list sources: add, list, remove', async ({ request }) => {
  const res = await request.post('/api/v1/importlists', {
    data: { name: 'Popular movies', type: 'tmdb-popular', qualityProfileId: 2 },
  })
  expect(res.status()).toBe(201)
  const created = await res.json()

  const lists = await (await request.get('/api/v1/importlists')).json()
  expect(lists.length).toBe(1)
  expect(lists[0].type).toBe('tmdb-popular')

  const del = await request.delete(`/api/v1/importlists/${created.id}`)
  expect(del.status()).toBe(204)
})

test('mass editor: bulk unmonitor and re-monitor', async ({ request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const books = await (await request.get('/api/v1/library?kind=book')).json()
  const ids = [movies[0].id, books[0].id]

  let res = await request.post('/api/v1/library/bulk', {
    data: { ids, monitored: false },
  })
  expect((await res.json()).updated).toBe(2)

  for (const id of ids) {
    const detail = await (await request.get(`/api/v1/library/${id}`)).json()
    expect(detail.monitored).toBe(false)
  }

  res = await request.post('/api/v1/library/bulk', { data: { ids, monitored: true } })
  expect((await res.json()).updated).toBe(2)
  const back = await (await request.get(`/api/v1/library/${ids[0]}`)).json()
  expect(back.monitored).toBe(true)
})

test('security surface: api key exposed, auth off by default, metrics opt-in', async ({ request }) => {
  const settings = await (await request.get('/api/v1/settings')).json()
  expect(settings.apiKey).toBeTruthy()
  expect(settings.authRequired).toBe(false)

  // /metrics is opt-in (MONARR_METRICS) and stays dark by default.
  const metrics = await request.get('/metrics')
  expect(metrics.status()).toBe(404)
})

test('security surface creates a mobile pairing QR only after reveal', async ({ page }) => {
  await page.goto('/settings')
  const security = page.locator('#security')
  await security.getByRole('button', { name: 'Reveal' }).click()
  await expect(security.getByRole('heading', { name: 'Link a mobile app' })).toBeVisible()

  await security.getByLabel('Mobile pairing server address').fill('http://[fd12:3456::20]:7676')
  await security.getByRole('button', { name: 'Show pairing QR' }).click()
  await expect(security.locator('.pairing-code svg')).toBeVisible()
  await expect(security).toContainText('Monarr app → Scan pairing QR')
})
