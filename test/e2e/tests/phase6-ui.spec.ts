import { expect, test } from '@playwright/test'

// Validation-era UI batch: theme picker, sidebar search, the labeled fact
// grid (location chip, profile, rating, external links), release info
// links, per-item edit, and metadata refresh.
test.describe.configure({ mode: 'serial' })

test('theme picker: manual light/dark override persists, auto clears it', async ({ page }) => {
  await page.goto('/')
  const html = page.locator('html')

  await page.getByRole('group', { name: 'Theme' }).getByRole('button', { name: 'light' }).click()
  await expect(html).toHaveAttribute('data-theme', 'light')

  await page.reload()
  await expect(html).toHaveAttribute('data-theme', 'light') // pre-paint script

  await page.getByRole('group', { name: 'Theme' }).getByRole('button', { name: 'dark' }).click()
  await expect(html).toHaveAttribute('data-theme', 'dark')

  await page.getByRole('group', { name: 'Theme' }).getByRole('button', { name: 'auto' }).click()
  await expect(html).not.toHaveAttribute('data-theme')
})

test('sidebar search finds media and settings sections', async ({ page }) => {
  await page.goto('/dashboard')
  const box = page.getByRole('searchbox', { name: 'Search media and settings' })

  await box.fill('test movie')
  await page.getByRole('button', { name: /The Test Movie/ }).first().click()
  await expect(page).toHaveURL(/\/library\/\d+/)
  await expect(page.getByRole('heading', { name: /The Test Movie/ })).toBeVisible()

  await box.fill('indexers')
  await page.getByRole('button', { name: /^Indexers/ }).click()
  await expect(page).toHaveURL(/\/settings/)
  await expect(page.locator('#indexers')).toBeVisible()
})

test('detail page shows labeled facts: location chip, profile, rating, links', async ({ page, request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const movie = movies[0]
  await page.goto(`/library/${movie.id}`)

  await expect(page.getByText('Location', { exact: true })).toBeVisible()
  await expect(page.locator('.path-chip')).toContainText('The Test Movie')
  await expect(page.getByText('Profile', { exact: true })).toBeVisible()

  // Rating came from the provider at add time (fake TMDB: 7.6, 4321 votes),
  // labeled with its source.
  await expect(page.locator('.rating-chip', { hasText: 'TMDB' })).toContainText('7.6')

  // External links open in a new tab.
  const imdb = page.getByRole('link', { name: /IMDb/ })
  await expect(imdb).toHaveAttribute('target', '_blank')
  await expect(imdb).toHaveAttribute('href', /imdb\.com\/title\/tt6010001/)
})

test('interactive search titles link to the indexer details page', async ({ page, request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  await page.goto(`/library/${movies[0].id}`)
  await page.getByRole('button', { name: 'Interactive search' }).click()

  const link = page.locator('td a[target="_blank"]').first()
  await expect(link).toBeVisible()
  await expect(link).toHaveAttribute('href', /\/details\//)
})

test('per-item edit updates monitoring and completeness fields exist', async ({ request }) => {
  const list = await (await request.get('/api/v1/library')).json()
  for (const item of list) {
    expect(item.rating).not.toBeUndefined()
    expect(item.episodeCount).not.toBeUndefined()
    expect(item.fileCount).not.toBeUndefined()
  }
  const series = list.find((m: any) => m.kind === 'series')
  expect(series.episodeCount).toBe(2) // both aired, monitored

  const movie = list.find((m: any) => m.kind === 'movie')
  let res = await request.patch(`/api/v1/library/${movie.id}`, { data: { monitored: false } })
  expect(res.status()).toBe(200)
  expect((await res.json()).monitored).toBe(false)
  // Put it back so later specs see the original state.
  res = await request.patch(`/api/v1/library/${movie.id}`, { data: { monitored: true } })
  expect((await res.json()).monitored).toBe(true)
})

test('saved indexers and clients: per-row Test with stored credentials', async ({ page, request }) => {
  const idx = await (await request.get('/api/v1/indexers')).json()
  expect(idx.length).toBeGreaterThan(0)
  expect((await request.post(`/api/v1/indexers/${idx[0].id}/test`)).status()).toBe(200)

  const clients = await (await request.get('/api/v1/downloadclients')).json()
  expect(clients.length).toBeGreaterThan(0)
  expect((await request.post(`/api/v1/downloadclients/${clients[0].id}/test`)).status()).toBe(200)

  expect((await request.post('/api/v1/indexers/99999/test')).status()).toBe(404)

  // And the buttons are on the saved rows in Settings.
  await page.goto('/settings')
  await page.locator('#indexers').getByRole('button', { name: 'Test' }).first().click()
  await expect(page.locator('#indexers .ok-text').first()).toBeVisible()
  await expect(
    page.locator('#downloadclients tbody').getByRole('button', { name: 'Test', exact: true }).first(),
  ).toBeVisible()
})

test('metadata refresh re-hydrates from the provider', async ({ request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const res = await request.post(`/api/v1/library/${movies[0].id}/refresh`)
  expect(res.status()).toBe(200)
  const detail = await res.json()
  expect(detail.rating).toBeCloseTo(7.6, 1)
  expect(detail.ratingVotes).toBe(4321)

  // The scheduled task exists too.
  const tasks = await (await request.get('/api/v1/system/tasks')).json()
  expect(tasks.some((t: any) => t.name === 'metadata.refresh')).toBe(true)
})

test('quality copies: wanted independently, managed from the item page', async ({ page, request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const movie = movies[0]

  // Add an HD-1080p copy sharing the item folder.
  let res = await request.post(`/api/v1/library/${movie.id}/copies`, {
    data: { qualityProfileId: 2, name: 'second copy' },
  })
  expect(res.status()).toBe(200)
  const detail = await res.json()
  expect(detail.copies.length).toBe(1)
  const copyId = detail.copies[0].id
  expect(detail.copies[0].monitored).toBe(true)

  // The copy is wanted under its own suffixed identity with its label,
  // even though the primary is satisfied.
  const wanted = await (await request.get('/api/v1/wanted')).json()
  const entry = wanted.find((w: any) => w.wantableId === `movie:${movie.id}:c${copyId}`)
  expect(entry).toBeTruthy()
  expect(entry.copy).toBe('second copy')
  expect(entry.missing).toBe(true)

  // The item page lists it, and the Wanted page labels it.
  await page.goto(`/library/${movie.id}`)
  await expect(page.getByRole('heading', { name: 'Quality copies' })).toBeVisible()
  await expect(page.getByText('second copy')).toBeVisible()
  await page.goto('/wanted')
  await expect(page.locator('.pill-info', { hasText: 'second copy' })).toBeVisible()

  // Books have no copies.
  const books = await (await request.get('/api/v1/library?kind=book')).json()
  if (books.length > 0) {
    const bad = await request.post(`/api/v1/library/${books[0].id}/copies`, {
      data: { qualityProfileId: 2 },
    })
    expect(bad.status()).toBe(400)
  }

  // Remove it (files kept) so later specs see the original state.
  res = await request.delete(`/api/v1/library/${movie.id}/copies/${copyId}`)
  expect(res.status()).toBe(200)
  expect((await res.json()).copies.length).toBe(0)
  const drained = await (await request.get('/api/v1/wanted')).json()
  expect(drained.find((w: any) => w.wantableId.endsWith(`:c${copyId}`))).toBeFalsy()
})
