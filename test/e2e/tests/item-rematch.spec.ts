import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

// Re-match… and Move to… on the item page (ADR 0027). The API is mocked at
// the network edge; what is checked is the dialog: the preview it shows,
// what it sends, progress and Stop, and that every result lands on screen.

const oldPath = '/20t/movies/Dune (1984)/Dune (1984) [Bluray-2160p].mkv'
const newPath = '/20t/movies/Dune (2021) {tmdb-438631}/Dune (2021) [Bluray-2160p].mkv'
const ep5 = '/20t/tv/Slow Horses/Season 04/Slow Horses - S04E05 - Five [WEBDL-1080p].mkv'
const ep6 = '/20t/tv/Slow Horses/Season 04/Slow Horses - S04E06 - Six [WEBDL-1080p].mkv'

async function fixture(page: Page) {
  const calls: { preview: unknown[]; start: unknown[]; cancel: number } = { preview: [], start: [], cancel: 0 }
  await page.route('**/api/v1/library/4243', route => route.fulfill({ json: {
    id: 4243, kind: 'movie', title: 'Dune', year: 1984, monitored: true, path: '/20t/movies/Dune (1984)',
    genres: [], ratings: [], ids: { tmdb: 841 }, seasons: [], copies: [], aliases: [], countries: [], identitySources: [],
    downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
    files: [{ id: 51, path: oldPath, size: 60_000_000_000, episodeIds: [], copyId: 0, quality: 'Bluray-2160p', fileGeneration: 'g51', sourceGeneration: '', sourceTransfer: '' }],
  } }))
  await page.route('**/api/v1/library/4244', route => route.fulfill({ json: {
    id: 4244, kind: 'series', title: 'Slow Horses', year: 2022, monitored: true, path: '/20t/tv/Slow Horses',
    genres: [], ratings: [], ids: {}, copies: [], aliases: [], countries: [], identitySources: [],
    downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
    seasons: [{ number: 4, monitored: true, episodes: [5, 6].map(n => ({ id: 900 + n, seasonNumber: 4, episodeNumber: n, title: n === 5 ? 'Five' : 'Six', monitored: true, hasFile: n === 5, airDate: '' })) }],
    files: [{ id: 61, path: ep5, size: 1_600_000_000, episodeIds: [905], copyId: 0, quality: 'WEBDL-1080p', fileGeneration: 'g61', sourceGeneration: 'sg', sourceTransfer: 't',
      sourceRelease: 'Slow.Horses.S04E05.1080p.WEB', sourceGuid: 'guid-61', sourceIndexerId: 3 }],
  } }))
  await page.route('**/api/v1/library?*', route => route.fulfill({ json: [
    { id: 4243, kind: 'movie', title: 'Dune', year: 1984, author: '', posterPath: '' },
  ] }))
  await page.route('**/api/v1/metadata/search?*', route => route.fulfill({ json: [
    { kind: 'movie', tmdbId: 841, title: 'Dune', year: 1984, overview: '', posterPath: '', inLibrary: true },
    { kind: 'movie', tmdbId: 438631, title: 'Dune', year: 2021, overview: '', posterPath: '', inLibrary: false },
    { kind: 'movie', tmdbId: 693134, title: 'Dune: Part Two', year: 2024, overview: '', posterPath: '', inLibrary: false },
  ] }))
  const base = { leftOut: [], blocklist: [], blocklistOn: true, sourceRevision: 7, targetRevision: 0, fingerprint: 'fp-1' }
  await page.route('**/api/v1/library/4243/rematch/preview', async route => {
    calls.preview.push(route.request().postDataJSON())
    await route.fulfill({ json: { ...base,
      source: { id: 4243, kind: 'movie', title: 'Dune', year: 1984, inLibrary: true, folder: '/20t/movies/Dune (1984)' },
      target: { id: 0, kind: 'movie', title: 'Dune', year: 2021, inLibrary: false, folder: '/20t/movies/Dune (2021) {tmdb-438631}', folderChosen: true },
      moves: [{ fileId: 51, from: oldPath, to: newPath, season: -1, episodes: [], mode: 'copy', bytes: 60_000_000_000, quality: 'Bluray-2160p' }],
      sourceOutcome: 'removed', sourceReason: 'removed: folder adoption created it and it is now empty',
      blocklistNote: 'These files were adopted from disk, not grabbed, so there is no release to blocklist',
      cinema: 'cinema scans the new folder and drops the old entry', summary: 'Move 1 file' } })
  })
  let stopped = false
  await page.route('**/api/v1/library/4243/rematch', async route => {
    calls.start.push(route.request().postDataJSON())
    await route.fulfill({ status: 202, json: { operationId: 'rm-1', requestId: 'r', state: 'moving', sourceItemId: 4243, targetItemId: 5000, bytesDone: 0, bytesTotal: 60_000_000_000 } })
  })
  await page.route('**/api/v1/rematch/rm-1', route => route.fulfill({ json: stopped
    ? { operationId: 'rm-1', requestId: 'r', state: 'cancelled', sourceItemId: 4243, targetItemId: 5000, bytesDone: 0, bytesTotal: 60_000_000_000, cinema: [] }
    : { operationId: 'rm-1', requestId: 'r', state: 'moving', sourceItemId: 4243, targetItemId: 5000, bytesDone: 12_000_000_000, bytesTotal: 60_000_000_000, cinema: [] } }))
  await page.route('**/api/v1/rematch/rm-1/cancel', async route => {
    calls.cancel++
    stopped = true
    await route.fulfill({ status: 202 })
  })

  await page.route('**/api/v1/library/4244/rematch/preview', async route => {
    const body = route.request().postDataJSON() as { choices?: { fileId: number; season?: number; episodes?: number[] }[] }
    calls.preview.push(body)
    const chosen = body.choices?.find(c => c.fileId === 61)?.episodes?.[0] === 6
    await route.fulfill({ json: { ...base, targetRevision: 9, fingerprint: chosen ? 'fp-6' : 'fp-5',
      source: { id: 4244, kind: 'series', title: 'Slow Horses', year: 2022, inLibrary: true },
      target: { id: 4244, kind: 'series', title: 'Slow Horses', year: 2022, inLibrary: true, folder: '/20t/tv/Slow Horses' },
      moves: [{ fileId: 61, from: ep5, to: chosen ? ep6 : ep5, season: 4, episodes: [chosen ? 6 : 5], label: chosen ? 'S04E06 · Six' : 'S04E05 · Five', mode: chosen ? 'rename' : 'relink', bytes: 1_600_000_000 }],
      blocklist: [{ fileId: 61, release: 'Slow.Horses.S04E05.1080p.WEB' }],
      blocklistNote: 'Blocklisted for Slow Horses, so automatic search does not grab the same release for the episode it left',
      episodes: [{ season: 4, episode: 5, title: 'Five', hasFile: true }, { season: 4, episode: 6, title: 'Six', hasFile: false }],
      sourceOutcome: 'stays', sourceReason: 'the files stay in Slow Horses', summary: 'Move 1 file' } })
  })
  await page.route('**/api/v1/library/4244/rematch', async route => {
    calls.start.push(route.request().postDataJSON())
    await route.fulfill({ status: 202, json: { operationId: 'rm-2', requestId: 'r2', state: 'complete', sourceItemId: 4244, targetItemId: 4244, bytesDone: 0, bytesTotal: 0,
      result: { moved: [{ from: ep5, to: ep6, episodes: [6], mode: 'rename' }], blocklisted: [{ release: 'Slow.Horses.S04E05.1080p.WEB' }] }, cinema: [] } })
  })
  await page.route('**/api/v1/rematch/rm-2', route => route.fulfill({ json: { operationId: 'rm-2', requestId: 'r2', state: 'complete', sourceItemId: 4244, targetItemId: 4244, bytesDone: 0, bytesTotal: 0,
    result: { moved: [{ from: ep5, to: ep6, episodes: [6], mode: 'rename' }], blocklisted: [{ release: 'Slow.Horses.S04E05.1080p.WEB' }] },
    cinema: [{ directory: '/20t/tv', status: 'ok', result: 'visible → plurx item 12', visibleAt: 1 }] } }))
  return calls
}

async function inViewport(page: Page, locator: ReturnType<Page['locator']>) {
  await expect(locator).toBeInViewport({ ratio: 1 })
  const box = await locator.boundingBox()
  const size = page.viewportSize()!
  expect(box && box.x >= 0 && box.x + box.width <= size.width + 1).toBe(true)
}

for (const width of [390, 1280]) {
  test(`Re-match… moves a wrongly adopted movie to the right title, with progress and Stop, at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4243')
    const trigger = page.getByRole('button', { name: 'Re-match…' })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Re-match Dune (1984)' })
    const heading = dialog.locator('.action-dialog-head h2')
    await inViewport(page, heading)
    await expect(heading).toBeFocused()

    await dialog.getByRole('button', { name: 'Search' }).click()
    // The current title is listed, by its library id, and cannot be chosen.
    await expect(dialog.getByRole('radio', { name: /Dune \(1984\).*in your library/ })).toBeDisabled()
    await dialog.getByRole('radio', { name: /Dune \(2021\)/ }).check()
    await expect.poll(() => calls.preview.length).toBeGreaterThan(0)
    expect(calls.preview.at(-1)).toMatchObject({ target: { kind: 'movie', tmdbId: 438631 } })
    await expect(dialog.getByText(/added to the library at/)).toBeVisible()
    await expect(dialog.getByText(newPath)).toBeVisible()
    await expect(dialog.getByText('removed: folder adoption created it and it is now empty')).toBeVisible()
    await expect(dialog.getByText(/copied across volumes, verified/)).toBeVisible()
    const go = dialog.getByRole('button', { name: 'Move 1 file' })
    await go.scrollIntoViewIfNeeded()
    await inViewport(page, go)
    await page.screenshot({ path: test.info().outputPath(`item-rematch-preview-${width}.png`) })
    await go.click()

    await expect.poll(() => calls.start.length).toBe(1)
    expect(calls.start[0]).toMatchObject({ target: { kind: 'movie', tmdbId: 438631 }, sourceRevision: 7, targetRevision: 0, fingerprint: 'fp-1' })
    // Progress is attributable and stoppable from here.
    await inViewport(page, dialog.getByRole('status').filter({ hasText: /Copying 11\.2 GB of 55\.9 GB/ }))
    const stop = dialog.getByRole('button', { name: 'Stop' })
    await inViewport(page, stop)
    await stop.click()
    await expect.poll(() => calls.cancel).toBe(1)
    await inViewport(page, dialog.getByRole('status').filter({ hasText: 'Stopped. Every file is back where it was.' }))
    await expect(stop).toHaveCount(0)
    await dialog.getByRole('button', { name: 'Done' }).click()
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })

  test(`Move to… moves one episode file to another episode of the same show at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4244')
    await page.getByRole('heading', { name: /^Files/ }).click() // the panel opens folded
    const trigger = page.getByRole('button', { name: 'Move to…' })
    await trigger.scrollIntoViewIfNeeded()
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: /^Move Slow Horses - S04E05/ })
    await inViewport(page, dialog.locator('.action-dialog-head h2'))
    await expect(dialog.getByRole('radio', { name: 'Same show, different episode' })).toBeChecked()
    const select = dialog.getByLabel(/Target episode for/)
    await expect(select).toBeVisible()
    await select.selectOption('4:6')
    await expect(dialog.getByText(ep6)).toBeVisible()
    await expect(dialog.getByRole('checkbox', { name: /Blocklist Slow\.Horses\.S04E05\.1080p\.WEB for Slow Horses/ })).toBeChecked()
    const go = dialog.getByRole('button', { name: 'Move', exact: true })
    await go.scrollIntoViewIfNeeded()
    await inViewport(page, go)
    await go.click()
    await expect.poll(() => calls.start.length).toBe(1)
    expect(calls.start[0]).toMatchObject({ target: { itemId: 4244 }, fileIds: [61], choices: [{ fileId: 61, season: 4, episodes: [6] }], fingerprint: 'fp-6', targetRevision: 9 })
    await inViewport(page, dialog.getByRole('status').filter({ hasText: 'Moved 1 file.' }))
    await expect(dialog.getByText(/In cinema\./)).toBeVisible()
    await dialog.getByRole('button', { name: 'Done' }).click()
    await expect(dialog).toHaveCount(0)
  })
}
