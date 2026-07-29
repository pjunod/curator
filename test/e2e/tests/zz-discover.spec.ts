import { expect, test } from '@playwright/test'

// Discover (ADR 0015): browse rows of trending/popular/upcoming titles and
// add one without leaving the page.
//
// Named zz- because the last test adds a library item, and library.spec.ts
// counts the posters on the library page. It sorts before zz-umbrella.spec.ts,
// which adds items against a root folder of its own.
//
// beforeAll sets its own TMDB key rather than relying on library.spec.ts
// having run — same reason smoke.spec.ts does, and the reason this file can
// be run alone.
test.describe.configure({ mode: 'serial' })

const TMDB_ROWS = 9
const TRAKT_ROWS = 5

test.beforeAll(async ({ request }) => {
  await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
})

test.describe('the catalogue', () => {
  test('TMDB alone serves the nine rows, and Trakt is absent until it is keyed', async ({
    request,
  }) => {
    // Clear the Trakt id first: the "Trakt rows appear" test below sets it,
    // and a rerun of this file must not depend on which order it last left
    // the database in.
    await request.put('/api/v1/settings', { data: { traktClientId: '' } })

    const res = await request.get('/api/v1/discover/lists')
    expect(res.ok()).toBeTruthy()
    const lists: { id: string; title: string; blurb: string; kind: string; source: string }[] =
      await res.json()

    expect(lists).toHaveLength(TMDB_ROWS)
    expect(lists.every((l) => l.source === 'tmdb')).toBeTruthy()
    // Every row says what it measures — a heading alone is not readable,
    // because "trending" is a different number at each provider.
    expect(lists.every((l) => l.title.length > 0 && l.blurb.length > 0)).toBeTruthy()
    expect(lists.every((l) => l.kind === 'movie' || l.kind === 'series')).toBeTruthy()
    // Books have no row on purpose (ADR 0015, non-goals).
    expect(lists.some((l) => l.kind === 'book')).toBeFalsy()
  })

  test('an unknown list is a 400, not a 500', async ({ request }) => {
    const res = await request.get('/api/v1/discover/items?list=not-a-row')
    expect(res.status()).toBe(400)
  })

  test('a row comes back marked against the library', async ({ request }) => {
    const res = await request.get('/api/v1/discover/items?list=tmdb-trending-movies')
    expect(res.ok()).toBeTruthy()
    const items = await res.json()
    expect(items.length).toBeGreaterThan(0)
    expect(items[0].title).toBe('Trending Test Movie')
    expect(items[0].kind).toBe('movie')
    expect(items[0].source).toBe('tmdb')
    expect(items[0]).toHaveProperty('inLibrary')
  })
})

test.describe('the page', () => {
  test('the sidebar link opens Discover with its rows and blurbs', async ({ page }) => {
    await page.goto('/')
    await page.getByRole('link', { name: 'Discover', exact: true }).click()

    await expect(page.getByRole('heading', { name: 'Discover' })).toBeVisible()
    await expect(page.locator('.discover-section').first()).toBeVisible()
    await expect(page.getByRole('heading', { name: /Trending this week/ }).first()).toBeVisible()
    await expect(page.getByText('What people looked up on TMDB').first()).toBeVisible()

    // Rows below the fold load as they are scrolled to, so the count on
    // arrival is only what fits — assert the sections exist, not that every
    // one has already fetched.
    await expect(page.locator('.discover-section')).toHaveCount(TMDB_ROWS)
    await expect(page.locator('.discover-card').first()).toBeVisible()
    await expect(page.getByText('Trending Test Movie')).toBeVisible()
  })

  test('the kind tabs filter which rows are shown', async ({ page }) => {
    await page.goto('/discover')
    await expect(page.locator('.discover-section')).toHaveCount(TMDB_ROWS)

    await page.getByRole('tab', { name: /Movies/ }).click()
    await expect(page.locator('.discover-section')).toHaveCount(5)
    await expect(page.locator('[data-list="tmdb-trending-movies"]')).toBeVisible()
    await expect(page.locator('[data-list="tmdb-popular-series"]')).toHaveCount(0)

    await page.getByRole('tab', { name: /Shows/ }).click()
    await expect(page.locator('.discover-section')).toHaveCount(4)
    await expect(page.locator('[data-list="tmdb-trending-series"]')).toBeVisible()

    await page.getByRole('tab', { name: /All/ }).click()
    await expect(page.locator('.discover-section')).toHaveCount(TMDB_ROWS)
  })

  test('a poster opens the dialog and adds to the library', async ({ page }) => {
    await page.goto('/discover')

    await page.locator('[data-list="tmdb-trending-movies"] .discover-card').first().click()
    const dialog = page.getByRole('dialog', { name: /Add Trending Test Movie/ })
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText(/discover fixture/)).toBeVisible()

    await dialog.getByRole('button', { name: 'Add to library' }).click()

    // The dialog turns into a receipt rather than closing: the row behind it
    // is still where the reader was, and the next poster is one click away.
    await expect(dialog.locator('.pill-ok', { hasText: 'added' })).toBeVisible()
    await dialog.getByRole('link', { name: 'Open in library' }).click()
    await expect(page.getByRole('heading', { name: /Trending Test Movie/ })).toBeVisible()

    // ...and the card says so on the way back, without a reload.
    await page.goto('/discover')
    await expect(
      page.locator('[data-list="tmdb-trending-movies"] .discover-card .pill-ok'),
    ).toHaveText('in library')
  })
})

test.describe('Trakt', () => {
  test('a client id adds five rows, hydrated with TMDB artwork', async ({ page, request }) => {
    await page.goto('/settings')
    await page.getByPlaceholder(/Trakt client id/i).fill('e2e-trakt-client-id')
    await page.getByRole('button', { name: 'Save Trakt client id' }).click()
    await expect(page.getByText('Saved.').first()).toBeVisible()

    const lists: { id: string; source: string }[] = await (
      await request.get('/api/v1/discover/lists')
    ).json()
    expect(lists).toHaveLength(TMDB_ROWS + TRAKT_ROWS)
    expect(lists.filter((l) => l.source === 'trakt')).toHaveLength(TRAKT_ROWS)

    // Trakt hands out ids without pictures, so the service fills the artwork
    // in from TMDB by id. The title stays Trakt's.
    const trending = await (
      await request.get('/api/v1/discover/items?list=trakt-trending-movies')
    ).json()
    expect(trending[0].title).toBe('Trakt Trending Film')
    expect(trending[0].posterPath).toBe('/hydrated.jpg')

    // And when hydration fails — this fixture points at an id with no detail
    // endpoint — the card keeps its title and loses only the picture.
    const boxOffice = await (
      await request.get('/api/v1/discover/items?list=trakt-boxoffice')
    ).json()
    expect(boxOffice[0].title).toBe('Trakt Box Office Film')
    expect(boxOffice[0].posterPath).toBe('')

    await page.goto('/discover')
    await expect(page.locator('.discover-section')).toHaveCount(TMDB_ROWS + TRAKT_ROWS)
    await expect(page.locator('[data-list="trakt-boxoffice"]')).toHaveCount(1)
  })
})
