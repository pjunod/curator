import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

// A disc the owner ripped: a title the library does not hold, a file name
// that says nothing. Library → Import files… finds the title, adds it
// unmonitored with no search, then hands over to the item's Import file…
// with the source stated. Backing out removes the title it added.
const rip = '/rips/title_t00.mkv'

async function fixture(page: Page) {
  const calls: { add: Record<string, unknown>[]; plan: Record<string, unknown>[]; manual: Record<string, unknown>[]; deleted: number } = { add: [], plan: [], manual: [], deleted: 0 }
  await page.route('**/api/v1/library?*', route => route.fulfill({ json: [] }))
  await page.route('**/api/v1/metadata/search?*', route => route.fulfill({ json: [
    { kind: 'movie', tmdbId: 4242, title: 'Ripped Film', year: 2005, overview: '', posterPath: '', inLibrary: false },
  ] }))
  await page.route('**/api/v1/rootfolders', route => route.fulfill({ json: [
    { id: 3, path: '/media/movies', kind: 'movie', freeBytes: 1e12, accessible: true },
  ] }))
  await page.route('**/api/v1/library', async route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (route.request().method() !== 'POST') return route.fallback()
    calls.add.push(route.request().postDataJSON())
    await route.fulfill({ status: 201, json: { id: 4300, kind: 'movie', title: 'Ripped Film', year: 2005 } })
  })
  await page.route('**/api/v1/library/4300', async route => {
    if (route.request().method() === 'DELETE') {
      calls.deleted++
      return route.fulfill({ status: 204 })
    }
    await route.fulfill({ json: {
      id: 4300, kind: 'movie', title: 'Ripped Film', year: 2005, monitored: false, path: '/media/movies/Ripped Film (2005)',
      genres: [], ratings: [], ids: { tmdb: 4242 }, files: [], copies: [], aliases: [], countries: [], identitySources: [],
      downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 3, seasons: [],
    } })
  })
  await page.route('**/api/v1/import/default-path', route => route.fulfill({ json: { path: '/rips' } }))
  await page.route('**/api/v1/filesystem?*', route => route.fulfill({ json: { path: '/rips/', parent: '/', dirs: [] } }))
  await page.route('**/api/v1/import/scan?*', route => route.fulfill({ json: [
    { path: rip, name: 'title_t00.mkv', size: 31_000_000_000, kind: 'video', quality: '', season: -1, episodes: [] },
  ] }))
  await page.route('**/api/v1/import/plan', async route => {
    const body = route.request().postDataJSON()
    calls.plan.push(body)
    const q = body.qualitySource === 'remux' ? 'Remux 1080p' : 'Unknown 1080p'
    await route.fulfill({ json: [{ path: rip, quality: q, season: -1, episodes: [], subtitles: 0,
      destination: `/media/movies/Ripped Film (2005)/Ripped Film (2005) [${q}].mkv`, action: 'new', reason: 'nothing is there yet', mode: 'copy' }] })
  })
  await page.route('**/api/v1/import/manual', async route => {
    calls.manual.push(route.request().postDataJSON())
    await route.fulfill({ status: 202, json: { downloadId: 92 } })
  })
  await page.route('**/api/v1/queue/92', route => route.fulfill({ json: { id: 92, state: 'imported', progress: 1, handoff: [] } }))
  return calls
}

async function chooseNewTitle(page: Page) {
  await page.goto('/')
  const trigger = page.getByRole('button', { name: 'Import files…' })
  await trigger.click()
  const picker = page.getByRole('dialog', { name: 'Import files' })
  await expect(picker.locator('.action-dialog-head h2')).toBeFocused()
  await picker.getByLabel('Title to search for').fill('Ripped Film')
  await picker.getByRole('button', { name: 'Search', exact: true }).click()
  await picker.getByRole('radio', { name: /Ripped Film/ }).check()
  await expect(picker.getByLabel(/Root folder/)).toHaveValue('3')
  await picker.getByRole('button', { name: 'Add and choose files' }).click()
  return trigger
}

for (const width of [390, 1280]) {
  test(`a ripped file goes into a title not yet in the library at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    const trigger = await chooseNewTitle(page)
    await expect.poll(() => calls.add).toEqual([expect.objectContaining({ kind: 'movie', tmdbId: 4242, rootFolderId: 3, monitored: false, searchNow: false })])

    const dialog = page.getByRole('dialog', { name: 'Import into Ripped Film' })
    await dialog.getByRole('checkbox', { name: /title_t00\.mkv/ }).check()
    await dialog.getByRole('button', { name: 'Next' }).click()
    await dialog.getByLabel(/Source/).selectOption('remux')
    await expect(dialog.getByText(/\[Remux 1080p\]\.mkv/)).toBeVisible()
    const go = dialog.getByRole('button', { name: /^Import \d* ?file/ })
    await go.scrollIntoViewIfNeeded()
    await expect(go).toBeInViewport({ ratio: 1 })
    await go.click()
    await expect.poll(() => calls.manual).toEqual([expect.objectContaining({ path: '/rips', paths: [rip], mediaItemId: 4300, qualitySource: 'remux' })])
    await dialog.getByRole('button', { name: 'Done' }).click()
    await expect(dialog).toHaveCount(0)
    expect(calls.deleted).toBe(0)
    await expect(trigger).toBeFocused()
  })
}

test('backing out of the import removes the title it added', async ({ page }) => {
  const calls = await fixture(page)
  await chooseNewTitle(page)
  const dialog = page.getByRole('dialog', { name: 'Import into Ripped Film' })
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toHaveCount(0)
  await expect.poll(() => calls.deleted).toBe(1)
})
