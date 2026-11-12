import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

// The item page's status panel (0.44.0). The server projects every state; the
// spec drives the panel against routed responses: where it sits, the 2 s poll
// while a download moves (without moving scroll or focus), every action's
// result inside the viewport, the one-line collapse, and a season pack shown
// once on its season.

const ITEM = 9881
const SERIES = 9882
const stages = (states: string[], details: string[] = []) =>
  ['searched', 'downloading', 'processed', 'moved', 'cinema_notified', 'in_cinema'].map((key, i) => ({
    key, state: states[i], at: states[i] === 'done' ? Date.now() - 60_000 : undefined, detail: details[i],
    actions: key === 'downloading' && states[i] === 'active' ? ['open_activity', 'cancel_download']
      : key === 'in_cinema' && states[i] === 'todo' ? ['confirm_cinema'] : undefined,
  }))

const downloading = () => ({
  mediaItemId: ITEM, kind: 'movie', generatedAt: Date.now(), active: true, done: false,
  headline: { state: 'active', title: 'Downloading, 3 min 48 s left', detail: 'Past.Lives.2023.2160p.WEB-DL · NZBgeek' },
  stages: stages(['done', 'active', 'todo', 'todo', 'todo', 'todo'], ['automatic · grabbed from NZBgeek', '34% · 3 min 48 s left']),
  download: { id: 501, release: 'Past.Lives.2023.2160p.WEB-DL', indexer: 'NZBgeek', state: 'downloading', bytes: 6.3e9, total: 18.4e9, progress: 0.34, rateBps: 48.2e6, etaSeconds: 228 },
  attempts: [{ downloadId: 400, release: 'Past.Lives.2023.2160p.BAD', indexer: 'NZBgeek', at: Date.now() - 3600_000, outcome: 'failed', reason: 'runner: par' }],
})

const inCinema = () => ({
  mediaItemId: ITEM, kind: 'movie', generatedAt: Date.now(), active: false, done: true,
  headline: { state: 'done', title: 'In cinema, confirmed 18:36', detail: 'confirmed · cinema item 81244' },
  stages: stages(['done', 'done', 'done', 'done', 'done', 'done']),
})

const unconfirmed = () => ({
  mediaItemId: ITEM, kind: 'movie', generatedAt: Date.now(), active: false, done: false,
  headline: { state: 'todo', title: 'In the library, not confirmed in cinema', detail: 'not confirmed' },
  stages: stages(['done', 'done', 'done', 'done', 'todo', 'todo'], ['', '', '', '1 file in the library', 'no scan request on record', 'not confirmed']),
})

async function movie(page: Page, body: () => unknown) {
  const calls: number[] = []
  await page.route(`**/api/v1/library/${ITEM}`, route => route.fulfill({
    json: {
      id: ITEM, kind: 'movie', title: 'Past Lives', year: 2023, monitored: true, path: '/20t/movies/Past Lives (2023)',
      genres: [], ratings: [], ids: {}, files: [], copies: [], seasons: [], overview: 'A long overview. '.repeat(40),
      downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1, runtime: 106,
    },
  }))
  await page.route(`**/api/v1/library/${ITEM}/status`, route => {
    calls.push(Date.now())
    return route.fulfill({ json: body() })
  })
  await page.goto(`/library/${ITEM}`)
  return calls
}

async function noSideScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1)
}

for (const width of [390, 1280]) {
  test.describe(`item status panel at ${width}px`, () => {
    test.beforeEach(async ({ page }) => page.setViewportSize({ width, height: 844 }))

    test('sits above Identity and polls every 2 s while downloading without moving scroll or focus', async ({ page }) => {
      const calls = await movie(page, downloading)
      const panel = page.getByTestId('item-status')
      await expect(panel.getByText('Downloading, 3 min 48 s left')).toBeVisible()
      await expect(panel.locator('.item-track > li')).toHaveCount(6)
      await expect(panel.getByRole('progressbar', { name: 'Download progress' })).toHaveAttribute('aria-valuenow', '34')
      await expect(panel).toContainText("48.2 MB/s (runner's average)")
      const identity = page.getByRole('heading', { name: 'Identity' })
      const [p, i] = [await panel.boundingBox(), await identity.boundingBox()]
      expect(p!.y).toBeLessThan(i!.y)
      await noSideScroll(page)

      // Scroll somewhere and focus something; the next polls must leave both.
      await identity.scrollIntoViewIfNeeded()
      const attempts = panel.getByText('Earlier attempts (1)')
      await attempts.focus()
      const y = await page.evaluate(() => scrollY)
      const before = calls.length
      await expect.poll(() => calls.length, { timeout: 7_000 }).toBeGreaterThanOrEqual(before + 2)
      expect(await page.evaluate(() => scrollY)).toBe(y)
      await expect(attempts).toBeFocused()
    })

    test('cancel download confirms in a dialog and shows its result in the viewport', async ({ page }) => {
      let cancelled = false
      await page.route('**/api/v1/queue/501?fromClient=true', route => {
        cancelled = route.request().method() === 'DELETE'
        return route.fulfill({ status: 204 })
      })
      await movie(page, downloading)
      const panel = page.getByTestId('item-status')
      await panel.getByRole('button', { name: 'Cancel download' }).click()
      const dialog = page.getByRole('dialog', { name: 'Cancel download' })
      await expect(dialog).toBeInViewport()
      await dialog.getByRole('button', { name: 'Cancel download' }).click()
      const notice = panel.getByRole('status').filter({ hasText: 'Download cancelled in Runner.' })
      await expect(notice).toBeInViewport({ ratio: 1 })
      expect(cancelled).toBe(true)
      await noSideScroll(page)
    })

    test('a finished item collapses to one line and never hides', async ({ page }) => {
      await movie(page, inCinema)
      const panel = page.getByTestId('item-status')
      const line = panel.getByRole('button', { name: /In cinema, confirmed 18:36/ })
      await expect(line).toBeVisible()
      await expect(panel.locator('.item-track')).toHaveCount(0)
      await line.click()
      await expect(panel.locator('.item-track > li')).toHaveCount(6)
      // The expanded track opens where the line was, inside the viewport.
      await expect(panel.locator('.item-track > li').first()).toBeInViewport()
      await noSideScroll(page)
    })

    test('ask cinema to confirm reports what it queued in the viewport', async ({ page }) => {
      await page.route(`**/api/v1/library/${ITEM}/status/confirm`, route => route.fulfill({ status: 202, json: { queued: 1 } }))
      await movie(page, unconfirmed)
      const panel = page.getByTestId('item-status')
      const ask = panel.getByRole('button', { name: 'Ask cinema to confirm' })
      await ask.click()
      await expect(panel.getByRole('status').filter({ hasText: 'Asked cinema to confirm 1 folder' })).toBeInViewport({ ratio: 1 })
      await noSideScroll(page)
    })

    test('a season pack is shown once, on its season, and episodes open their own track', async ({ page }) => {
      const D = 'done', A = 'active', T = 'todo'
      await page.route(`**/api/v1/library/${SERIES}`, route => route.fulfill({
        json: {
          id: SERIES, kind: 'series', title: 'Slow Horses', year: 2022, monitored: true, path: '', genres: [], ratings: [], ids: {},
          files: [], copies: [], downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1,
          seasons: [{ number: 4, monitored: true, episodes: [] }],
        },
      }))
      await page.route(`**/api/v1/library/${SERIES}/status*`, route => {
        const season = new URL(route.request().url()).searchParams.get('season')
        const pack = { id: 8812, release: 'Slow.Horses.S04.1080p.WEB-DL', state: 'downloading', bytes: 9.8e9, total: 9.8e9, progress: 1, stage: 'par_repair', stageSince: Date.now() - 60_000, episodes: 4 }
        const summary = { season: 4, episodes: 6, inCinema: 2, onDisk: 2, active: true, summary: '2 of 6 in cinema · 4 in one season pack', download: pack }
        if (season === null) {
          return route.fulfill({ json: { mediaItemId: SERIES, kind: 'series', generatedAt: Date.now(), active: true, done: false,
            headline: { state: 'active', title: '2 of 6 episodes in cinema', detail: '1 season downloading or importing' }, seasons: [summary] } })
        }
        const eps = [1, 2, 3, 4, 5, 6].map(n => ({
          episodeId: 100 + n, season: 4, episode: n, inPack: n > 2,
          summary: n > 2 ? 'in season pack · repairing' : 'in cinema · Sep 4',
          stages: stages(n > 2 ? [D, D, A, T, T, T] : [D, D, D, D, D, D]),
        }))
        return route.fulfill({ json: { mediaItemId: SERIES, kind: 'series', generatedAt: Date.now(), active: true, done: false,
          headline: { state: 'active', title: 'Season 4' }, seasons: [summary], episodes: eps } })
      })
      await page.goto(`/library/${SERIES}`)
      const panel = page.getByTestId('item-status')
      await expect(panel.locator('.item-pack')).toHaveCount(1)
      await expect(panel.locator('.item-pack')).toContainText('Season pack in runner: par repair')
      const season = panel.getByRole('button', { name: /Season 4/ })
      await expect(season).toHaveAttribute('aria-expanded', 'false')
      await season.click()
      const rows = panel.locator('.item-ep')
      await expect(rows).toHaveCount(6)
      await expect(panel.locator('.item-pack')).toHaveCount(1)
      await rows.nth(2).click()
      const track = panel.getByRole('list', { name: 'Stages for 4×03' })
      await expect(track).toBeInViewport()
      await expect(track.locator('li[data-state="active"]')).toHaveAttribute('data-stage', 'processed')
      await noSideScroll(page)
    })
  })
}
