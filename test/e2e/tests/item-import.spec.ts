import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

const folder = '/processing/complete/Slow.Horses.S04.job-8812.runner-1'
const named = `${folder}/Slow.Horses.S04E03.1080p.WEB.h264.mkv`
const obfuscated = `${folder}/a7f3c91e.mkv`
const loose = '/downloads/Slow.Horses.S04E05.mkv'

async function fixture(page: Page) {
  const calls: { plan: unknown[]; manual: unknown[]; place: unknown[]; cancel: unknown[] } = { plan: [], manual: [], place: [], cancel: [] }
  await page.route('**/api/v1/library/4242', route => route.fulfill({ json: {
    id: 4242, kind: 'series', title: 'Slow Horses', monitored: true, path: '/20t/tv/Slow Horses',
    genres: [], ratings: [], ids: {}, files: [], copies: [], aliases: [], countries: [], identitySources: [],
    downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, rootFolderId: 1,
    seasons: [{ number: 4, monitored: true, episodes: [3, 4, 5].map(n => ({
      id: n, seasonNumber: 4, episodeNumber: n, title: `Episode ${n}`, monitored: true, hasFile: false, airDate: '',
    })) }],
  } }))
  await page.route('**/api/v1/import/default-path', route => route.fulfill({ json: { path: folder } }))
  await page.route('**/api/v1/rootfolders', route => route.fulfill({ json: [{ id: 1, path: '/20t', kind: 'any', freeBytes: 1, accessible: true }] }))
  await page.route('**/api/v1/filesystem?*', route => {
    const path = new URL(route.request().url()).searchParams.get('path') ?? '/'
    const dirs = path === '/downloads/' ? [] : [{ name: 'Subs', path: `${folder}/Subs`, registered: false }]
    return route.fulfill({ json: { path, parent: '/processing/complete', dirs } })
  })
  await page.route('**/api/v1/import/scan?*', route => {
    const path = new URL(route.request().url()).searchParams.get('path')
    if (path === '/nope') return route.fulfill({ status: 400, json: { message: 'path not accessible: Curator cannot read /nope (no such file). If the download client sees this folder under a different path, add a remote path mapping to that client in Settings → Acquisition so Curator reads it where it is mounted here' } })
    if (path === '/downloads') return route.fulfill({ json: [
      { path: loose, name: 'Slow.Horses.S04E05.mkv', size: 1_600_000_000, kind: 'video', quality: 'WEBDL-1080p', season: 4, episodes: [5] },
    ] })
    const publication = (p: string) => ({ downloadId: 77, publicationId: 5, path: p, outcome: 'unresolved' })
    return route.fulfill({ json: [
      { path: named, name: 'Slow.Horses.S04E03.1080p.WEB.h264.mkv', size: 1_700_000_000, kind: 'video', quality: 'WEBDL-1080p', season: 4, episodes: [3], subtitles: 2, publication: publication('Slow.Horses.S04E03.1080p.WEB.h264.mkv') },
      { path: obfuscated, name: 'a7f3c91e.mkv', size: 1_600_000_000, kind: 'video', quality: 'WEBDL-1080p', season: -1, episodes: [], publication: publication('a7f3c91e.mkv') },
    ] })
  })
  await page.route('**/api/v1/import/plan', async route => {
    const body = route.request().postDataJSON() as { files: { path: string; season: number; episodes: number[] }[]; removeOriginal?: boolean }
    calls.plan.push(body)
    await route.fulfill({ json: body.files.map(f => f.episodes.length === 0
      ? { path: f.path, quality: 'WEBDL-1080p', season: -1, episodes: [], subtitles: 0, error: 'choose the episode this file is; its name does not say' }
      : { path: f.path, quality: 'WEBDL-1080p', season: f.season, episodes: f.episodes, subtitles: f.path === named ? 2 : 0,
          destination: `/20t/tv/Slow Horses/Season 04/Slow Horses - S04E0${f.episodes[0]} - Episode ${f.episodes[0]} [WEBDL-1080p].mkv`,
          action: f.episodes[0] === 4 ? 'replace' : 'new', existing: f.episodes[0] === 4 ? 'HDTV-720p' : undefined,
          reason: f.episodes[0] === 4 ? 'WEBDL-1080p ranks above the HDTV-720p here' : 'nothing is there yet',
          mode: f.path === loose ? (body.removeOriginal ? 'rename' : 'hardlink') : 'runner',
          publication: f.path === loose ? undefined : { downloadId: 77, publicationId: 5, path: f.path.split('/').pop(), outcome: 'unresolved' } }) })
  })
  await page.route('**/api/v1/queue/77/publications/5/files/place', async route => {
    calls.place.push(route.request().postDataJSON())
    await route.fulfill({ status: 204 })
  })
  await page.route('**/api/v1/import/manual', async route => {
    calls.manual.push(route.request().postDataJSON())
    await route.fulfill({ status: 202, json: { downloadId: 91 } })
  })
  let stopped = false
  await page.route('**/api/v1/queue/77/cancel-import', async route => {
    calls.cancel.push(route.request().postDataJSON())
    stopped = true
    await route.fulfill({ status: 204 })
  })
  await page.route('**/api/v1/queue/77', route => route.fulfill({ json: stopped
    ? { id: 77, state: 'downloaded', progress: 1, handoff: [] }
    : { id: 77, state: 'importing', importState: 'running', progress: 1, bytes: 900_000_000, handoff: [],
        importControl: { generationKey: 'g77', epoch: '1', outcome: '', pendingPublications: 0, retainedPublications: 0, heldStages: 0 } } }))
  await page.route('**/api/v1/queue/91', route => route.fulfill({ json: { id: 91, state: 'imported', progress: 1, handoff: [
    { step: 'originals_removal_done', at: 1, detail: 'Remove original: removed 1 original(s)' }] } }))
  return calls
}

async function inViewport(page: Page, locator: ReturnType<Page['locator']>) {
  await expect(locator).toBeInViewport({ ratio: 1 })
  const box = await locator.boundingBox()
  const size = page.viewportSize()!
  expect(box && box.x >= 0 && box.x + box.width <= size.width + 1).toBe(true)
}

for (const width of [390, 1280]) {
  test(`Import file… places a runner delivery's files as chosen episodes at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4242')
    const trigger = page.getByRole('button', { name: 'Import file…' })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Import into Slow Horses' })
    const heading = dialog.locator('.action-dialog-head h2')
    await inViewport(page, heading)
    await expect(heading).toBeFocused()

    // Choose: the completed folder opens, with its files, subtitles and delivery.
    const first = dialog.getByRole('checkbox', { name: /Slow\.Horses\.S04E03/ })
    await expect(first).toBeVisible()
    await expect(dialog.getByText('2 subs')).toBeVisible()
    await expect(dialog.getByText('no episode in name')).toBeVisible()
    await first.check()
    await dialog.getByRole('checkbox', { name: /a7f3c91e/ }).check()
    const next = dialog.getByRole('button', { name: 'Next' })
    await next.scrollIntoViewIfNeeded()
    await inViewport(page, next)
    await next.click()

    // Confirm: the obfuscated file needs an episode before anything can start.
    await expect(dialog.getByText('choose the episode this file is; its name does not say')).toBeVisible()
    const go = dialog.getByRole('button', { name: 'Import 2 files' })
    await expect(go).toBeDisabled()
    await dialog.getByLabel('Episode for a7f3c91e.mkv').selectOption('4:4')
    await expect(dialog.getByText('replaces HDTV-720p')).toBeVisible()
    await expect(dialog.getByText(/Season 04\/Slow Horses - S04E04/)).toBeVisible()
    await expect(dialog.getByText(/cleans up the delivery folder/)).toBeVisible()
    await expect(dialog.getByRole('checkbox', { name: /Remove original/ })).toHaveCount(0)
    await go.scrollIntoViewIfNeeded()
    await inViewport(page, go)
    await page.screenshot({ path: test.info().outputPath(`item-import-confirm-${width}.png`) })
    await go.click()

    // One placement for both files of the delivery: one admission, one import.
    await expect.poll(() => calls.place.length).toBe(1)
    expect(calls.place[0]).toEqual({ mediaItemId: 4242, files: [
      { path: 'Slow.Horses.S04E03.1080p.WEB.h264.mkv', season: 4, episodes: [3] },
      { path: 'a7f3c91e.mkv', season: 4, episodes: [4] },
    ] })
    // Started: the job is attributable and stoppable from here.
    const stop = dialog.getByRole('button', { name: 'Stop' })
    await inViewport(page, stop)
    await stop.click()
    await expect.poll(() => calls.cancel).toEqual([{ generationKey: 'g77', epoch: '1' }])
    await inViewport(page, dialog.getByText(/Stopped\. The files stay where they were/))
    await expect(dialog.getByText(/Activity #77: downloaded/)).toBeVisible()
    await expect(stop).toHaveCount(0)
    const done = dialog.getByRole('button', { name: 'Done' })
    await done.click()
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })

  test(`Import file… imports a loose file with Remove original, and names an unreadable path at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    const calls = await fixture(page)
    await page.goto('/library/4242')
    await page.getByRole('button', { name: 'Import file…' }).click()
    const dialog = page.getByRole('dialog', { name: 'Import into Slow Horses' })
    const pathBox = dialog.getByLabel('Folder path')
    await pathBox.fill('/nope')
    await dialog.getByRole('button', { name: 'Open' }).click()
    const err = dialog.getByRole('alert').filter({ hasText: 'remote path mapping' })
    await inViewport(page, err)
    await pathBox.fill('/downloads')
    await dialog.getByRole('button', { name: 'Open' }).click()
    await dialog.getByRole('checkbox', { name: /S04E05/ }).check()
    await dialog.getByRole('button', { name: 'Next' }).click()
    await expect(dialog.getByText('hardlinked — your copy stays')).toBeVisible()
    const remove = dialog.getByRole('checkbox', { name: /Remove original/ })
    await expect(remove).not.toBeChecked()
    await remove.check()
    await expect(dialog.getByText('renamed into the library')).toBeVisible()
    const go = dialog.getByRole('button', { name: 'Import 1 file' })
    await go.scrollIntoViewIfNeeded()
    await inViewport(page, go)
    await go.click()
    await expect.poll(() => calls.manual.length).toBe(1)
    expect(calls.manual[0]).toMatchObject({ path: '/downloads', paths: [loose], mediaItemId: 4242, removeOriginal: true,
      episodeTargets: [{ path: loose, season: 4, episodes: [5] }] })
    await inViewport(page, dialog.getByText(/Activity #91: imported/))
    await inViewport(page, dialog.getByText('Remove original: removed 1 original(s)'))
  })
}
