import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

// Offload… and Re-acquire on the item page, and the library's On disk /
// Offloaded / Everything filter (ADR 0028). The API is mocked at the network
// edge; what is checked is what a person sees: the preview before anything is
// deleted, progress and Stop, that every result lands in the viewport, and
// that an offloaded row offers Re-acquire instead of a monitored tick.

const moviePath = '/20t/movies/Dune (2021)/Dune (2021) [Bluray-2160p].mkv'
const subPath = '/20t/movies/Dune (2021)/Dune (2021).en.srt'
const ep1 = '/20t/tv/Slow Horses/Season 04/Slow Horses - S04E01-E02 [WEBDL-1080p].mkv'

async function fixture(page: Page) {
  const calls = { preview: [] as unknown[], start: [] as unknown[], cancel: 0, reacquire: [] as unknown[] }
  let stopped = false
  await page.route('**/api/v1/library/4301', route => route.fulfill({ json: {
    id: 4301, kind: 'movie', title: 'Dune', year: 2021, monitored: true, path: '/20t/movies/Dune (2021)',
    genres: [], ratings: [], ids: { tmdb: 438631 }, seasons: [], copies: [], aliases: [], countries: [], identitySources: [],
    downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
    files: [{ id: 51, path: moviePath, size: 60_000_000_000, episodeIds: [], copyId: 0, quality: 'Bluray-2160p', fileGeneration: 'g51', sourceGeneration: '', sourceTransfer: '' }],
  } }))
  await page.route('**/api/v1/library/4301/status', route => route.fulfill({ status: 404, json: { error: 'none' } }))
  await page.route('**/api/v1/library/4301/offload/preview', async route => {
    calls.preview.push(route.request().postDataJSON())
    await route.fulfill({ json: {
      item: { id: 4301, kind: 'movie', title: 'Dune', year: 2021 }, scope: 'item', label: 'Dune (2021)',
      files: [{ path: moviePath, kind: 'media', bytes: 60_000_000_000 }, { path: subPath, kind: 'sidecar', bytes: 90_000 }],
      bytes: 60_000_090_000, freed: 60_000_090_000, freeBefore: 120_000_000_000, freeAfter: 180_000_090_000, root: '/20t/movies',
      cinema: 'Cinema does not advertise offload yet: it will drop the title and its watch history. Upgrade it first to keep them', cinemaWarning: true,
      fingerprint: 'fp-1', summary: 'Delete 2 files · 55.9 GB freed' } })
  })
  await page.route('**/api/v1/library/4301/offload', async route => {
    if (route.request().method() === 'GET') return route.fulfill({ status: 204, body: '' })
    calls.start.push(route.request().postDataJSON())
    await route.fulfill({ status: 202, json: { operationId: 'of-1', requestId: 'r', state: 'deleting', itemId: 4301, scope: 'item', label: 'Dune (2021)', filesTotal: 2, filesDone: 0, bytesTotal: 60_000_090_000, bytesFreed: 0, stoppable: true } })
  })
  await page.route('**/api/v1/offload/of-1', route => route.fulfill({ json: stopped
    ? { operationId: 'of-1', requestId: 'r', state: 'cancelled', itemId: 4301, scope: 'item', filesTotal: 2, filesDone: 1, bytesTotal: 60_000_090_000, bytesFreed: 60_000_000_000, stoppable: false }
    : { operationId: 'of-1', requestId: 'r', state: 'deleting', itemId: 4301, scope: 'item', filesTotal: 2, filesDone: 1, bytesTotal: 60_000_090_000, bytesFreed: 0, stoppable: true } }))
  await page.route('**/api/v1/offload/of-1/cancel', async route => { calls.cancel++; stopped = true; await route.fulfill({ status: 202 }) })

  await page.route('**/api/v1/library/4302', route => route.fulfill({ json: {
    id: 4302, kind: 'series', title: 'Slow Horses', year: 2022, monitored: true, path: '/20t/tv/Slow Horses', offloaded: true,
    genres: [], ratings: [], ids: {}, copies: [], aliases: [], countries: [], identitySources: [],
    downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
    seasons: [
      { number: 4, monitored: true, episodes: [1, 2].map(n => ({ id: 900 + n, seasonNumber: 4, episodeNumber: n, title: `Ep ${n}`, monitored: true, hasFile: true, airDate: '' })) },
      { number: 5, monitored: true, episodes: [1, 2].map(n => ({ id: 950 + n, seasonNumber: 5, episodeNumber: n, title: `Ep ${n}`, monitored: false, offloaded: n === 1, hasFile: n === 2, airDate: '' })) },
    ],
    files: [{ id: 61, path: ep1, size: 3_200_000_000, episodeIds: [901, 902], copyId: 0, quality: 'WEBDL-1080p', fileGeneration: 'g61', sourceGeneration: '', sourceTransfer: '' }],
  } }))
  await page.route('**/api/v1/library/4302/status', route => route.fulfill({ status: 404, json: { error: 'none' } }))
  await page.route('**/api/v1/library/4302/offload', route => route.fulfill({ status: 204, body: '' }))
  await page.route('**/api/v1/library/4302/offload/preview', async route => {
    calls.preview.push(route.request().postDataJSON())
    await route.fulfill({ json: {
      item: { id: 4302, kind: 'series', title: 'Slow Horses', year: 2022 }, scope: 'episode', label: 'S04E01 · Ep 1',
      files: [{ path: ep1, kind: 'media', bytes: 3_200_000_000, episodes: ['S04E01', 'S04E02'] }],
      bytes: 3_200_000_000, freed: 3_200_000_000, freeBefore: -1, freeAfter: -1,
      episodes: ['S04E01', 'S04E02'], widened: ['S04E02'],
      cinema: 'Cinema keeps the title, its poster and its watch history, greyed out with a Get it back button',
      fingerprint: 'fp-2', summary: 'Delete 1 file · 3.0 GB freed' } })
  })
  await page.route('**/api/v1/library/4302/reacquire', async route => {
    calls.reacquire.push(route.request().postDataJSON())
    await route.fulfill({ json: { mediaItemId: 4302, state: 'searching', cleared: true } })
  })
  return calls
}

async function inViewport(page: Page, locator: ReturnType<Page['locator']>) {
  await expect(locator).toBeInViewport({ ratio: 1 })
  const box = await locator.boundingBox()
  const size = page.viewportSize()!
  expect(box && box.x >= 0 && box.x + box.width <= size.width + 1).toBe(true)
}

for (const width of [390, 1280]) {
  test(`Offload… previews what is deleted, runs with progress and Stop, at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4301')
    const trigger = page.getByRole('button', { name: 'Offload…' })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Offload Dune' })
    const heading = dialog.locator('.action-dialog-head h2')
    await inViewport(page, heading)
    await expect(heading).toBeFocused()
    expect(calls.preview[0]).toMatchObject({ scope: 'item' })

    // Everything that goes, the space that comes back, and what cinema does.
    await expect(dialog.getByText(moviePath)).toBeVisible()
    await expect(dialog.getByText(subPath)).toBeVisible()
    await expect(dialog.getByTestId('offload-space')).toContainText('free space 112 GB → 168 GB')
    await expect(dialog.getByTestId('offload-cinema')).toContainText('will drop the title and its watch history')
    const go = dialog.getByRole('button', { name: 'Offload', exact: true })
    await expect(go).toBeDisabled() // not until the person says they understand
    await dialog.getByRole('checkbox', { name: /files are deleted/ }).check()
    await go.scrollIntoViewIfNeeded()
    await inViewport(page, go)
    await page.screenshot({ path: test.info().outputPath(`item-offload-preview-${width}.png`) })
    await go.click()

    await expect.poll(() => calls.start.length).toBe(1)
    expect(calls.start[0]).toMatchObject({ scope: 'item', fingerprint: 'fp-1' })
    await inViewport(page, dialog.getByRole('status').filter({ hasText: 'Deleting… 1 of 2 files.' }))
    const stop = dialog.getByRole('button', { name: 'Stop' })
    await inViewport(page, stop)
    await stop.click()
    await expect.poll(() => calls.cancel).toBe(1)
    await inViewport(page, dialog.getByRole('status').filter({ hasText: /Stopped\. 1 of 2 files were deleted; the rest stay on disk/ }))
    await expect(stop).toHaveCount(0)
    await dialog.getByRole('button', { name: 'Done' }).click()
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })

  test(`an episode Offload says a multi-episode file widens it; an offloaded episode offers Re-acquire, at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4302')
    // The Seasons panel is closed until asked; season 1 would open by
    // itself, these are 4 and 5, so open each.
    await page.getByText('Seasons', { exact: false }).first().click()
    await page.getByRole('button', { name: /Season 4/ }).click()
    await page.getByRole('button', { name: 'Offload…' }).nth(2).click() // item, season, then the first episode row
    const dialog = page.getByRole('dialog', { name: /Offload Slow Horses/ })
    expect(calls.preview.at(-1)).toMatchObject({ scope: 'episode', episodeId: 901 })
    await inViewport(page, dialog.getByRole('status').filter({ hasText: /widens to S04E02/ }))
    await expect(dialog.getByTestId('offload-space')).toContainText('free space unknown → unknown')
    await dialog.getByRole('button', { name: 'Cancel' }).click()
    await expect(dialog).toHaveCount(0)

    // Season 5's first episode is offloaded: no monitored tick, Re-acquire instead.
    await page.getByRole('button', { name: /Season 5/ }).click()
    await expect(page.getByRole('checkbox', { name: 'Monitor episode 5x1' })).toBeDisabled()
    await page.getByRole('row', { name: /5x01/ }).getByRole('button', { name: 'Re-acquire' }).click()
    await expect.poll(() => calls.reacquire.length).toBe(1)
    expect(calls.reacquire[0]).toMatchObject({ scope: 'episode', episodeId: 951 })
    await inViewport(page, page.getByRole('status').filter({ hasText: 'Re-acquire of 5x01: searching at the current profile.' }))
  })

  test(`First season only: a later season offers Monitor and search, and the result is on screen, at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const detail = {
      id: 4303, kind: 'series', title: 'Andor', year: 2022, monitored: true, monitor: 'first_season', path: '/20t/tv/Andor',
      genres: [], ratings: [], ids: {}, copies: [], aliases: [], countries: [], identitySources: [], files: [],
      downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
      seasons: [1, 2].map(n => ({ number: n, monitored: n === 1, episodes: [{ id: 970 + n, seasonNumber: n, episodeNumber: 1, title: 'One', monitored: n === 1, hasFile: false, airDate: '' }] })),
    }
    const calls = { patch: 0, search: 0 }
    await page.route('**/api/v1/library/4303', route => route.fulfill({ json: detail }))
    await page.route('**/api/v1/library/4303/status', route => route.fulfill({ status: 404, json: { error: 'none' } }))
    await page.route('**/api/v1/library/4303/seasons/2', async route => { calls.patch++; await route.fulfill({ json: detail }) })
    await page.route('**/api/v1/library/4303/autosearch', async route => { calls.search++; await route.fulfill({ json: { grabbed: 0, targets: [{ season: 2, skipped: 'queued' }] } }) })
    await page.goto('/library/4303')
    await page.getByText('Seasons', { exact: false }).first().click()
    await expect(page.getByRole('button', { name: 'Monitor and search' })).toHaveCount(1) // season 2 only
    await page.getByRole('button', { name: 'Monitor and search' }).click()
    await expect.poll(() => calls.patch + calls.search).toBe(2)
    await inViewport(page, page.getByRole('status').filter({ hasText: /Season 2 monitored\./ }))
  })

  test(`the library filter shows On disk, Offloaded or Everything and remembers it, at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const item = (id: number, title: string, extra: object) => ({ id, kind: 'movie', title, year: 2021, author: '', posterPath: '', monitored: true, path: '', rating: 0, ratingVotes: 0, ratings: [], episodeCount: 0, episodeFileCount: 0, fileCount: 0, addedAt: '2024-01-01', ...extra })
    await page.route('**/api/v1/library?*', route => route.fulfill({ json: [item(1, 'Here', { fileCount: 1 }), item(2, 'Gone', { offloaded: true })] }))
    await page.route('**/api/v1/library', route => route.fulfill({ json: [item(1, 'Here', { fileCount: 1 }), item(2, 'Gone', { offloaded: true })] }))
    await page.goto('/')
    await page.getByRole('tab', { name: 'Movies' }).click()
    const select = page.getByLabel('Presence in Movies')
    await expect(select).toHaveValue('all') // default Everything
    await expect(page.getByText('Gone')).toBeVisible()
    const offloadedCard = page.locator('.library-card').filter({ hasText: 'Gone' })
    await expect(offloadedCard.getByText('Offloaded', { exact: true })).toBeVisible()
    await select.selectOption('offloaded')
    await expect(page.getByText('Here')).toHaveCount(0)
    await expect(page.getByText('Gone')).toBeVisible()
    await select.selectOption('on_disk')
    await expect(page.getByText('Gone')).toHaveCount(0)
    await expect(page.getByText('Here')).toBeVisible()
    await page.reload()
    await page.getByRole('tab', { name: 'Movies' }).click()
    await expect(page.getByLabel('Presence in Movies')).toHaveValue('on_disk')
  })
}
