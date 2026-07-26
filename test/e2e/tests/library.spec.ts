import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// Phase 1 walking flow, in order: configure → add → browse → scan.
// Serial by design: each step builds on the previous one's state.
test.describe.configure({ mode: 'serial' })

const mediaRoot = process.env.E2E_MEDIA_ROOT!

test('configure TMDB key and a root folder via the settings UI', async ({ page }) => {
  await page.goto('/settings')

  await page.getByPlaceholder(/TMDB API key/i).fill('e2e-test-key')
  await page.getByRole('button', { name: 'Save key' }).click()
  await expect(page.getByText('Saved.')).toBeVisible()

  await page.getByPlaceholder('/absolute/path/to/media').fill(mediaRoot)
  // One directory holds a movie, a series and a book across this suite, which
  // is what a mixed root is for (ADR 0009). The picker defaults to Movies, and
  // leaving it there made every later series and book add fail with "root
  // folder holds a different media kind" — a correct refusal against a root
  // that had never been asked what it holds.
  await page.getByLabel('What the new root folder holds').selectOption('mixed')
  await page.getByRole('button', { name: 'Add root folder' }).click()
  await expect(page.getByRole('cell', { name: mediaRoot })).toBeVisible()
  await expect(page.locator('.pill-ok', { hasText: 'ok' })).toBeVisible()
})

test('search TMDB and add a movie', async ({ page }) => {
  await page.goto('/add')
  await page.getByPlaceholder(/Search movies/i).fill('test');
  await expect(page.getByText('The Test Movie')).toBeVisible()

  await page.getByRole('button', { name: 'Add', exact: true }).click()

  // Adding keeps you on the add screen so the next one is a single click; the
  // item page is one link away for when you actually want it.
  await expect(page.getByTestId('added-strip')).toContainText('Added 1 item')
  await page.getByRole('link', { name: 'Open' }).first().click()

  // The detail page, fully hydrated.
  await expect(page.getByRole('heading', { name: /The Test Movie/ })).toBeVisible()
  await expect(page.getByText('101 min')).toBeVisible()
  await expect(page.getByText('No files on disk yet', { exact: false })).toBeVisible()
})

test('add a series with hydrated seasons and episodes', async ({ page }) => {
  await page.goto('/add')
  await page.getByRole('tab', { name: 'Series' }).click()
  await page.getByPlaceholder(/Search series/i).fill('test')
  await expect(page.getByText('The Test Show')).toBeVisible()

  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await page.getByRole('link', { name: 'Open' }).first().click()

  await expect(page.getByRole('heading', { name: /The Test Show/ })).toBeVisible()
  // Appears twice now: the season summary and the Status completeness pill.
  await expect(page.getByText('0/2 on disk').first()).toBeVisible()
  await expect(page.getByRole('cell', { name: 'Pilot' })).toBeVisible()
  await expect(page.getByRole('cell', { name: 'Finale' })).toBeVisible()
})

test('library grid lists both items and filters by kind', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('.poster-card')).toHaveCount(2)

  await page.getByRole('tab', { name: 'Movies' }).click()
  await expect(page.locator('.poster-card')).toHaveCount(1)
  await expect(page.getByText('The Test Movie')).toBeVisible()

  // The library calls series "TV", the same word the review window uses.
  await page.getByRole('tab', { name: 'TV' }).click()
  await expect(page.getByText('The Test Show')).toBeVisible()
})

test('re-adding the same movie is rejected as already in library', async ({ page, request }) => {
  const res = await request.post('/api/v1/library', {
    data: { kind: 'movie', tmdbId: 601 },
  })
  expect(res.status()).toBe(409)

  await page.goto('/add')
  await page.getByPlaceholder(/Search movies/i).fill('test')
  await expect(page.getByText('in library')).toBeVisible()
})

test('disk scan links episode files and reports unmatched folders', async ({ page, request }) => {
  // Lay files down on disk exactly where the series folder points:
  // <root>/The Test Show (2020)/Season 1/…S01E01…
  const showDir = join(mediaRoot, 'The Test Show (2020)', 'Season 1')
  mkdirSync(showDir, { recursive: true })
  writeFileSync(join(showDir, 'The.Test.Show.S01E01.1080p.mkv'), 'fake video bytes')
  mkdirSync(join(mediaRoot, 'Totally Unknown Show'), { recursive: true })

  // Trigger the scan and wait for the report.
  const trigger = await request.post('/api/v1/library/scan')
  expect(trigger.status()).toBe(202)
  await expect
    .poll(
      async () => {
        const res = await request.get('/api/v1/library/scan/report')
        if (!res.ok()) return null
        return (await res.json()) as { filesLinked: number }
      },
      { timeout: 10_000 },
    )
    .not.toBeNull()

  const report = await (await request.get('/api/v1/library/scan/report')).json()
  expect(report.filesLinked).toBe(1)
  expect(report.unmatchedDirs.map((d: { name: string }) => d.name)).toContain(
    'Totally Unknown Show',
  )

  // The series detail now shows the file against S01E01.
  await page.goto('/')
  await page.getByText('The Test Show').click()
  await expect(page.getByText('1/2 on disk').first()).toBeVisible()
  await expect(page.getByRole('cell', { name: /S01E01/ })).toBeVisible()

  // And the library page surfaces the unmatched folder.
  await page.goto('/')
  await expect(page.getByText('unmatched folder')).toBeVisible()
  await expect(page.getByText('Totally Unknown Show')).toBeVisible()
})
