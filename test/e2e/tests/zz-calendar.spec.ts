import { expect, test } from '@playwright/test'

// The rebuilt calendar (ADR 0016): a month grid that says what state each
// entry is in, an agenda of rich rows, and real air times underneath both.
//
// `zz-` because Playwright orders spec files LEXICALLY and this one seeds its
// own state; serial because the view toggle's persistence is a sequence, not
// three independent facts.
test.describe.configure({ mode: 'serial' })

// The library's fixture air dates are all in 2020 (test/e2e/fake-tmdb.mjs),
// and a rolling-from-today agenda shows none of them. `?date=` is the seam.
const SHOW_DAY = '2020-01-01'
const CROWDED_DAY = '2020-01-15'
const TIME_RE = /\d{1,2}(:\d{2})?\s?(AM|PM)/i

test.beforeAll(async ({ request }) => {
  await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })

  // Five movies on one day, so a month cell overflows into "+N more".
  for (let id = 6200; id <= 6204; id++) {
    await request.post('/api/v1/library', { data: { kind: 'movie', tmdbId: id, monitored: true } })
  }

  // Air times arrive with the metadata refresh, not with the add: enrichment
  // rides the existing task rather than a new one, and this is that claim
  // being exercised rather than asserted.
  const res = await request.post('/api/v1/system/tasks/metadata.refresh/run')
  expect(res.ok()).toBe(true)
  await expect
    .poll(
      async () => {
        const entries = await (
          await request.get(`/api/v1/calendar?start=${SHOW_DAY}&end=${SHOW_DAY}`)
        ).json()
        return entries.some((e: any) => typeof e.airDateUtc === 'string')
      },
      { timeout: 15_000, intervals: [500] },
    )
    .toBe(true)
})

test('the API composes a UTC instant and keeps every old field', async ({ request }) => {
  const entries = await (
    await request.get('/api/v1/calendar?start=2020-01-01&end=2020-01-31')
  ).json()
  const episode = entries.find((e: any) => e.kind === 'episode')
  expect(episode).toBeTruthy()

  // Frozen: plurx reads these in production.
  expect(episode.date).toBe(SHOW_DAY)
  expect(episode.detail).toContain('S01E01')

  // Additive: 21:00 America/New_York on 1 January is 02:00Z the next day.
  expect(episode.airDateUtc).toBe('2020-01-02T02:00:00Z')
  expect(episode.network).toBe('E2E One')
  expect(episode.seasonNumber).toBe(1)
  expect(episode.episodeNumber).toBe(1)

  // A movie has no broadcast slot and must not grow one.
  const movie = entries.find((e: any) => e.kind === 'movie')
  expect(movie.airDateUtc).toBeUndefined()
  expect(movie.network).toBeUndefined()
})

test('agenda: rich rows with poster, episode, time and network', async ({ page }) => {
  await page.goto(`/calendar?view=agenda&date=${SHOW_DAY}`)
  await expect(page.getByRole('heading', { name: 'Calendar' })).toBeVisible()

  // Skeletons are REPLACED by real rows, so count the real ones.
  const rows = page.locator('.cal-row:not(.cal-skeleton)')
  await expect(rows.first()).toBeVisible()

  // The anchored day, by class rather than by text: "January 1" is also a
  // substring of "January 15", which is the crowded day this suite seeds.
  const day = page.locator('.cal-day.cal-today')
  await expect(day).toBeVisible()
  await expect(day.locator('.cal-day-head')).toContainText('January 1')
  const row = day.locator('.cal-row').first()
  await expect(row).toContainText('The Test Show')
  await expect(row).toContainText('S01E01')
  await expect(row.locator('.cal-row-poster')).toBeVisible()
  await expect(row).toContainText('E2E One')
  // Regex, not a literal: the runner's timezone is not pinned.
  await expect(row.locator('.cal-row-when')).toHaveText(TIME_RE)
  // The state pill. Which of the two it is depends on what earlier specs did
  // to this episode's file; what this pins is that an AIRED entry never
  // reads "Upcoming".
  await expect(row.locator('.pill')).toHaveText(/^(On disk|Missing)$/)

  // The whole window stays bounded: an empty stretch of calendar keeps the
  // sentinel on screen, and auto-loading is capped so it cannot walk the
  // horizon out on its own.
  await expect(page.getByText(/days back · \d+ ahead/).first()).toContainText(/· [1-9]\d{0,2} ahead/)
})

test('agenda: a row links to its library item', async ({ page }) => {
  await page.goto(`/calendar?view=agenda&date=${SHOW_DAY}`)
  await page.locator('.cal-row:not(.cal-skeleton)').first().click()
  await expect(page).toHaveURL(/\/library\/\d+/)
})

test('month: a crowded day overflows into a day panel', async ({ page }) => {
  await page.goto(`/calendar?view=month&date=${CROWDED_DAY}`)
  await expect(page.locator('.cal-dow')).toHaveCount(7)

  // Four chips is the density budget; the fifth and beyond collapse.
  const more = page.getByRole('button', { name: /\+\d+ more/ })
  await expect(more).toBeVisible()
  await more.click()

  const panel = page.locator('.modal')
  await expect(panel).toBeVisible()
  await expect(panel.locator('.cal-row')).toHaveCount(5)
  await expect(panel).toContainText('Crowded Alpha')
  await panel.getByRole('button', { name: 'Close' }).click()
  await expect(page.locator('.modal')).toHaveCount(0)
})

test('month: chips carry state and the legend explains it', async ({ page }) => {
  await page.goto(`/calendar?view=month&date=${SHOW_DAY}`)
  // The imported episode is on disk; the crowded movies are not, and their
  // release date is long past, so they read as missing.
  await expect(page.locator('.cal-entry.cal-have').first()).toBeVisible()
  await expect(page.locator('.cal-legend')).toContainText('On disk')
  await expect(page.locator('.cal-legend')).toContainText('Aired, missing')
  // A time on the chip, once the schedule is known.
  await expect(page.locator('.cal-entry-time').first()).toHaveText(TIME_RE)
})

test('the view toggle persists, and ?view overrides it', async ({ page }) => {
  await page.goto(`/calendar?date=${SHOW_DAY}`)
  await expect(page.locator('.cal-grid')).toBeVisible()

  await page.getByRole('button', { name: 'Agenda', exact: true }).click()
  await expect(page.locator('.cal-agenda')).toBeVisible()
  await expect(page.locator('.cal-grid')).toHaveCount(0)

  // Reload: the choice survives, because it is in localStorage.
  await page.goto(`/calendar?date=${SHOW_DAY}`)
  await expect(page.locator('.cal-agenda')).toBeVisible()

  // ...and an explicit param beats the stored choice.
  await page.goto(`/calendar?view=month&date=${SHOW_DAY}`)
  await expect(page.locator('.cal-grid')).toBeVisible()

  // Put it back, so a later spec in this file starts from the default.
  await page.getByRole('button', { name: 'Month', exact: true }).click()
})
