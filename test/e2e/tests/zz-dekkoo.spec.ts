import { expect, test, type Locator } from '@playwright/test'

async function inViewport(element: Locator, viewport: { width: number; height: number }) {
  const box = await element.boundingBox()
  expect(box).not.toBeNull()
  expect(box!.x).toBeGreaterThanOrEqual(0)
  expect(box!.y).toBeGreaterThanOrEqual(0)
  expect(box!.x + box!.width).toBeLessThanOrEqual(viewport.width)
  expect(box!.y + box!.height).toBeLessThanOrEqual(viewport.height)
}

for (const viewport of [{ width: 1280, height: 900 }, { width: 390, height: 844 }]) {
  test(`Dekkoo uses Discover posters and Add controls at ${viewport.width}px`, async ({ page, request }) => {
    await page.setViewportSize(viewport)
    await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
    let roots = await (await request.get('/api/v1/rootfolders')).json()
    if (!roots.some((r: {kind: string}) => r.kind === 'mixed' || r.kind === 'movie')) {
      const root = await request.post('/api/v1/rootfolders', { data: { path: process.env.E2E_MEDIA_ROOT!, kind: 'mixed' } })
      expect(root.ok()).toBeTruthy()
      roots = await (await request.get('/api/v1/rootfolders')).json()
    }
    // Exercise cleanup even in a focused retry, with an already-added movie.
    const seed = await request.post('/api/v1/library', { data: {
      kind: 'movie', tmdbId: 860,
      rootFolderId: roots.find((r: { kind: string }) => r.kind === 'mixed' || r.kind === 'movie').id,
      monitored: false, searchNow: false,
    } })
    expect([201, 409]).toContain(seed.status())
    // Library summaries expose title/kind, not TMDB IDs. Each viewport must
    // remove the fixture before testing the actual UI add.

    const library = await (await request.get('/api/v1/library')).json()
    for (const item of library) {
      if (item.title === 'Dekkoo Fixture Film' && item.kind === 'movie') expect((await request.delete(`/api/v1/library/${item.id}`)).ok()).toBeTruthy()
    }
    await page.goto('/discover')
    await expect(page.getByRole('tab', { name: 'Dekkoo', exact: true })).toHaveCount(0)
    await expect(page.getByLabel('Title to find')).toHaveCount(0)
    await page.getByRole('tab', { name: /Movies/ }).click()
    await expect(page.locator('[data-list="dekkoo-series"]')).toHaveCount(0)
    const row = page.locator('[data-list="dekkoo-movies"]')
    await row.scrollIntoViewIfNeeded()
    const card = row.getByRole('button', { name: /Dekkoo Fixture Film/ })
    await expect(card).toHaveCount(1)
    await expect(card.locator('img')).toHaveAttribute('src', /dekkoo.jpg/)
    await expect(row).not.toContainText('The Test Movie')
    await card.click()
    const dialog = page.getByRole('dialog', { name: 'Add Dekkoo Fixture Film' })
    await expect(dialog).toBeVisible()
    await inViewport(dialog, viewport)
    await expect(dialog.getByRole('heading')).toBeFocused()
    await expect(dialog.getByText('A movie that exists only inside the e2e suite.')).toBeVisible()
    await expect(dialog.getByLabel('Quality profile')).toBeVisible()
    await dialog.getByLabel('Search on add').uncheck()
    const add = dialog.getByRole('button', { name: 'Add to library' })
    await add.scrollIntoViewIfNeeded()
    await inViewport(add, viewport)
    await add.click()
    const receipt = dialog.getByRole('status')
    await expect(receipt).toContainText('added')
    await inViewport(receipt, viewport)
    await dialog.getByRole('button', { name: 'Close', exact: true }).click()
    await expect(card).toBeFocused()
    await expect(card).toContainText('in library')
    const movies = await (await request.get('/api/v1/discover/items?list=dekkoo-movies')).json()
    expect(movies).toHaveLength(1)
    expect(movies[0]).toMatchObject({ tmdbId: 860, kind: 'movie', source: 'dekkoo', inLibrary: true })

    await page.getByRole('tab', { name: /Shows/ }).click()
    await expect(page.locator('[data-list="dekkoo-movies"]')).toHaveCount(0)
    const shows = page.locator('[data-list="dekkoo-series"]')
    await shows.scrollIntoViewIfNeeded()
    const show = shows.getByRole('button', { name: /Dekkoo Fixture Show/ })
    await expect(show).toHaveCount(1)
    await show.click()
    const showDialog = page.getByRole('dialog', { name: 'Add Dekkoo Fixture Show' })
    await inViewport(showDialog, viewport)
    await expect(showDialog.getByLabel('Seasons:')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(show).toBeFocused()
  })
}
