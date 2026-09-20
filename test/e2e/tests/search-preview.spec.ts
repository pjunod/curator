import { expect, test } from '@playwright/test'

const original = { kind: 'series', tmdbId: 0, tvdbId: 414217, imdbId: 'tt16867040', hydrationSource: 'tvmaze', title: 'Cunk on Earth', year: 2022, overview: 'Original full synopsis.\n\nThe final paragraph is readable.', posterPath: '', inLibrary: false }
const details = { kind: 'series', title: original.title, year: 2022, overview: 'Enriched synopsis.\n\nA much longer final paragraph.', ids: { tvdb: 414217, tmdb: 999, imdb: original.imdbId }, genres: ['Comedy'], runtimeMinutes: 30, status: 'Ended', previewSource: 'tvmaze', ownership: 'absent', addability: 'supported' }

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/metadata/search?*', (route) => route.fulfill({ json: [original] }))
  await page.route('**/api/v1/rootfolders', (route) => route.fulfill({ json: [{ id: 8, kind: 'series', path: '/media/tv' }] }))
  await page.route('**/api/v1/profiles', (route) => route.fulfill({ json: [] }))
})

for (const width of [1280, 320]) {
  test(`preview navigation, accessibility and immutable Add at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 850 })
    await page.route('**/api/v1/metadata/preview?*', (route) => route.fulfill({ json: details }))
    let payload: Record<string, unknown> | undefined
    await page.route('**/api/v1/library', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback()
      payload = route.request().postDataJSON()
      await route.fulfill({ json: { id: 77, ...original }, status: 201 })
    })
    await page.goto('/add?kind=series&q=cunk')
    const trigger = page.getByRole('button', { name: 'View details for Cunk on Earth' })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: original.title })
    await expect(dialog).toBeVisible()
    await expect(dialog).toContainText('A much longer final paragraph.')
    await expect(dialog).toContainText('30 min per episode')
    await expect(dialog.getByRole('link', { name: /IMDb/ })).toHaveAttribute('href', 'https://www.imdb.com/title/tt16867040/')
    await expect(dialog.getByRole('link', { name: /IMDb/ })).toHaveAttribute('rel', 'noopener noreferrer')
    expect(await page.locator('#root').evaluate((node) => (node as HTMLElement).inert)).toBe(true)
    expect(await dialog.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true)
    await page.keyboard.press('Tab')
    expect(await dialog.evaluate((node) => node.contains(document.activeElement))).toBe(true)
    await page.keyboard.press('Escape')
    await expect(dialog).not.toBeVisible()
    await expect(trigger).toBeFocused()
    await trigger.click()
    await page.goBack()
    await expect(dialog).not.toBeVisible()
    await page.goForward()
    await expect(dialog).toBeVisible()
    await dialog.getByText('Add options', { exact: true }).click()
    await dialog.getByLabel('Monitored', { exact: true }).uncheck()
    await dialog.getByRole('button', { name: 'Add to library', exact: true }).click()
    await expect(dialog.getByRole('link', { name: 'Open in library' })).toHaveAttribute('href', '/library/77')
    expect(payload).toMatchObject({ kind: 'series', tvdbId: 414217, imdbId: original.imdbId, hydrationSource: 'tvmaze', rootFolderId: 8, monitored: false, searchNow: false })
    expect(payload).not.toHaveProperty('tmdbId')
    await dialog.getByRole('button', { name: 'Close details' }).click()
    await expect(page.locator('.result')).toContainText('added')
    await expect(page.getByRole('searchbox')).toHaveValue('cunk')
  })
}

test('provider failure keeps original facts; retry conflict discards enrichment', async ({ page }) => {
  let response = 503
  await page.route('**/api/v1/metadata/preview?*', (route) => route.fulfill({ status: response, json: response === 200 ? details : { code: response === 409 ? 'identity_conflict' : 'provider_unavailable', message: 'Unavailable' } }))
  await page.goto('/add?kind=series&q=cunk')
  await page.getByRole('button', { name: /View details for/ }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('More details are unavailable')
  await expect(dialog).toContainText('The final paragraph is readable.')
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeEnabled()
  response = 200
  await dialog.getByRole('button', { name: 'Retry' }).click()
  await expect(dialog).toContainText('Enriched synopsis.')
  await dialog.getByRole('button', { name: 'Close details' }).click()
  response = 409
  await page.getByRole('button', { name: /View details for/ }).click()
  await expect(dialog).toContainText('conflicting identities')
  await expect(dialog).not.toContainText('Enriched synopsis.')
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeDisabled()
  await expect(dialog.getByRole('link', { name: /IMDb/ })).toHaveCount(0)
})

test('direct preview URL closes without leaving Add and ambiguous ownership preserves facts', async ({ page }) => {
  await page.route('**/api/v1/metadata/preview?*', (route) => route.fulfill({ json: { ...details, ownership: 'ambiguous', addability: 'conflict', addBlockReason: 'Resolve matching library identities.' } }))
  await page.goto('/add?kind=series&q=cunk&preview=series%3Atvdb%3A414217')
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('Enriched synopsis.')
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeDisabled()
  await expect(dialog.getByRole('link', { name: /IMDb/ })).toBeVisible()
  await expect(dialog.getByRole('link', { name: 'Open in library' })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'Close details' }).click()
  await expect(page).toHaveURL(/\/add\?.*q=cunk/)
  await expect(dialog).not.toBeVisible()
})

test('quick Add stays independent of preview', async ({ page }) => {
  let previews = 0
  await page.route('**/api/v1/metadata/preview?*', async (route) => { previews++; await route.fulfill({ json: details }) })
  await page.route('**/api/v1/library', (route) => route.request().method() === 'POST' ? route.fulfill({ json: { ...original, id: 78 }, status: 201 }) : route.fallback())
  await page.goto('/add?kind=series&q=cunk')
  await page.locator('.result').getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.locator('.result')).toContainText('added')
  expect(previews).toBe(0)
  await expect(page.getByRole('dialog')).toHaveCount(0)
})

test('book and TVDB preview URLs restore directly without a text-search resolver', async ({ page }) => {
  // Exact search cannot provide these selections. Preview still can.
  await page.route('**/api/v1/metadata/search?*', (route) => route.fulfill({ status: 409, json: { code: 'identity_conflict', message: 'Local ambiguity' } }))
  await page.route('**/api/v1/metadata/preview?*', (route) => route.fulfill({ json: route.request().url().includes('olid=')
    ? { kind: 'book', title: 'A Book', overview: 'The whole work synopsis.', ids: { olid: 'OL12W' }, ownership: 'absent', addability: 'supported' }
    : { ...details, ownership: 'ambiguous', addability: 'conflict' } }))
  await page.goto('/add?kind=book&preview=book%3Aolid%3AOL12W')
  await expect(page.getByRole('dialog')).toContainText('The whole work synopsis.')
  await page.reload()
  await expect(page.getByRole('dialog')).toContainText('The whole work synopsis.')
  await expect(page.getByRole('dialog').getByRole('link', { name: /Open Library/ })).toHaveAttribute('href', 'https://openlibrary.org/works/OL12W')
  await page.goto('/add?kind=series&q=cunk&preview=series%3Atvdb%3A414217')
  await expect(page.getByRole('dialog')).toContainText('Enriched synopsis.')
  await expect(page.getByRole('dialog').getByRole('button', { name: 'Add to library' })).toBeDisabled()
})

test('unsupported hydration remains blocked through failed retries', async ({ page }) => {
  let status = 422
  await page.route('**/api/v1/metadata/preview?*', (route) => route.fulfill({ status, json: status === 200 ? details : { code: status === 422 ? 'unsupported_hydration' : 'provider_unavailable', message: 'No supported provider can add this title.' } }))
  await page.goto('/add?kind=series&q=cunk')
  await page.getByRole('button', { name: /View details for/ }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('No supported provider can add this title.')
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeDisabled()
  await expect(dialog.getByRole('link', { name: /IMDb/ })).toBeVisible()
  status = 503
  await dialog.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeDisabled()
  status = 200
  await dialog.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(dialog.getByRole('button', { name: 'Add to library' })).toBeEnabled()
})
