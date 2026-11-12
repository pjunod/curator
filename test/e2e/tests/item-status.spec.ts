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
  headline: { state: 'done', title: 'In cinema, confirmed', detail: 'confirmed · cinema item 81244', at: Date.now() - 60_000 },
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
      await page.route('**/api/v1/queue/501?fromClient=true&blocklist=true', route => {
        cancelled = route.request().method() === 'DELETE'
        return route.fulfill({ status: 204 })
      })
      await movie(page, downloading)
      const panel = page.getByTestId('item-status')
      await panel.getByRole('button', { name: 'Cancel download' }).click()
      const dialog = page.getByRole('dialog', { name: 'Cancel download' })
      await expect(dialog).toBeInViewport()
      await dialog.getByRole('button', { name: 'Cancel download' }).click()
      const notice = panel.getByRole('status').filter({ hasText: 'Download cancelled in Runner; the release is blocklisted.' })
      await expect(notice).toBeInViewport({ ratio: 1 })
      expect(cancelled).toBe(true)
      await noSideScroll(page)
    })

    test('a finished item collapses to one line and never hides', async ({ page }) => {
      await movie(page, inCinema)
      const panel = page.getByTestId('item-status')
      const line = panel.getByRole('button', { name: /In cinema, confirmed \d/ })
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
          episodeId: 100 + n, season: 4, episode: n, inPack: n > 2, wantableId: `episode:${SERIES}:4:${n}`,
          summary: n > 2 ? 'in season pack · repairing' : 'in cinema', at: n > 2 ? undefined : Date.now() - 86400_000 * 3,
          stages: stages(n > 2 ? [D, D, A, T, T, T] : [D, D, D, D, D, D]),
        }))
        return route.fulfill({ json: { mediaItemId: SERIES, kind: 'series', generatedAt: Date.now(), active: true, done: false,
          headline: { state: 'active', title: 'Season 4' }, seasons: [summary], episodes: eps } })
      })
      await page.goto(`/library/${SERIES}`)
      const panel = page.getByTestId('item-status')
      await expect(panel.locator('.item-pack')).toHaveCount(1)
      await expect(panel.locator('.item-pack')).toContainText('Season pack in runner: par repair')
      const season = panel.getByRole('button', { name: /Where it stands for season 4/ })
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

    test('a failed grab names the stage and its Search now result lands in the viewport', async ({ page }) => {
      await page.route(`**/api/v1/library/${ITEM}/autosearch`, route => route.fulfill({ json: { grabbed: 1, targets: [{ wantableId: `movie:${ITEM}`, label: 'Past Lives (2023)', seen: 4, matched: 4, accepted: 2, grabbed: 'Past.Lives.2023.2160p.OTHER' }] } }))
      await movie(page, () => ({
        mediaItemId: ITEM, kind: 'movie', generatedAt: Date.now(), active: false, done: false,
        headline: { state: 'failed', title: 'Processed failed', detail: 'failed (runner: par)' },
        stages: ['searched', 'downloading', 'processed', 'moved', 'cinema_notified', 'in_cinema'].map((key, i) => ({
          key, state: ['done', 'done', 'failed', 'todo', 'todo', 'todo'][i],
          detail: i === 2 ? 'failed (runner: par)' : undefined, actions: i === 2 ? ['search', 'open_activity'] : undefined,
        })),
      }))
      const panel = page.getByTestId('item-status')
      await expect(panel.getByText('Processed failed')).toBeVisible()
      await expect(panel.locator('li[data-state="failed"]')).toHaveAttribute('data-stage', 'processed')
      await panel.getByRole('button', { name: 'Search now' }).click()
      await expect(panel.getByRole('status').filter({ hasText: /Past\.Lives\.2023\.2160p\.OTHER|grabbed/i })).toBeInViewport({ ratio: 1 })
      await noSideScroll(page)
    })

    test('a background poll that finds the item done does not fold the panel away', async ({ page }) => {
      let n = 0
      await movie(page, () => (n++ === 0 ? downloading() : inCinema()))
      const panel = page.getByTestId('item-status')
      await expect(panel.getByText('Downloading, 3 min 48 s left')).toBeVisible()
      await expect(panel.getByText('In cinema, confirmed')).toBeVisible({ timeout: 6_000 })
      await expect(panel.locator('.item-track > li')).toHaveCount(6)
      await expect(panel.getByRole('button', { name: /In cinema, confirmed/ })).toHaveCount(0)
    })

    test('episode actions act on that episode and show their result in the viewport', async ({ page }) => {
      const calls: string[] = []
      await page.route(`**/api/v1/library/${SERIES}`, route => route.fulfill({
        json: {
          id: SERIES, kind: 'series', title: 'Slow Horses', year: 2022, monitored: true, path: '', genres: [], ratings: [], ids: {},
          files: [], copies: [], downloadPriority: 0, downloadPriorityOverride: null, qualityProfileId: 1,
          seasons: [{ number: 4, monitored: true, episodes: [] }],
        },
      }))
      await page.route('**/api/v1/wanted/searches', route => {
        calls.push(`search ${route.request().postData()}`)
        return route.fulfill({ status: 202, json: { runId: 'r1', scope: 'target', scopeLabel: 'Slow Horses S04E01', status: 'queued', createdAt: '', targetDelayMs: 0, selected: 1, processed: 0, searched: 0, skipped: 0, failed: 0, grabbed: 0 } })
      })
      await page.route('**/api/v1/queue/702/import', route => { calls.push('import 702'); return route.fulfill({ status: 204 }) })
      await page.route(`**/api/v1/library/${SERIES}/status*`, route => {
        const season = new URL(route.request().url()).searchParams.get('season')
        const summary = { season: 4, episodes: 2, inCinema: 0, onDisk: 0, active: false, summary: '0 of 2 in cinema' }
        if (season === null) {
          return route.fulfill({ json: { mediaItemId: SERIES, kind: 'series', generatedAt: Date.now(), active: false, done: false,
            headline: { state: 'waiting', title: '0 of 2 episodes in cinema' }, seasons: [summary] } })
        }
        const sk = ['searched', 'downloading', 'processed', 'moved', 'cinema_notified', 'in_cinema']
        const ep = (n: number, st: string[], acts: Record<number, string[]>, extra = {}) => ({
          episodeId: 200 + n, season: 4, episode: n, inPack: false, wantableId: `episode:${SERIES}:4:${n}`, summary: n === 1 ? 'not searched' : 'moved · import failed',
          stages: sk.map((key, i) => ({ key, state: st[i], actions: acts[i] })), ...extra,
        })
        return route.fulfill({ json: { mediaItemId: SERIES, kind: 'series', generatedAt: Date.now(), active: false, done: false,
          headline: { state: 'waiting', title: 'Season 4' }, seasons: [summary], episodes: [
            ep(1, ['todo', 'todo', 'todo', 'todo', 'todo', 'todo'], { 0: ['search', 'interactive_search'] }),
            ep(2, ['done', 'done', 'done', 'failed', 'todo', 'todo'], { 3: ['retry_import', 'open_activity'] }, { downloadId: 702 }),
          ] } })
      })
      await page.goto(`/library/${SERIES}`)
      const panel = page.getByTestId('item-status')
      await panel.getByRole('button', { name: /Where it stands for season 4/ }).click()
      const rows = panel.locator('.item-ep')
      await rows.nth(0).click()
      await panel.getByRole('button', { name: 'Search now' }).click()
      await expect(panel.getByRole('status').filter({ hasText: 'Searching Slow Horses S04E01' })).toBeInViewport({ ratio: 1 })
      expect(calls[0]).toContain(`"wantableId":"episode:${SERIES}:4:1"`)
      await rows.nth(1).click()
      await panel.getByRole('button', { name: 'Retry import' }).click()
      await expect(panel.getByRole('status').filter({ hasText: 'Import started again.' })).toBeInViewport({ ratio: 1 })
      expect(calls).toContain('import 702')
      await noSideScroll(page)
    })
  })
}
