import { rmSync } from 'node:fs'
import { expect, test } from '@playwright/test'

// Phase 3: the automation loop, end to end. Earlier specs imported the
// movie/series/book at cutoff quality, so the wanted index starts empty.
// We then delete the movie file from disk, rescan, watch it become wanted,
// let the RSS sync grab it back unattended, and see the webhook notifier
// told about the import. Calendar + backups round it out.
test.describe.configure({ mode: 'serial' })

const ARR = `http://127.0.0.1:${process.env.FAKE_ARR_PORT ?? 7799}`
const MOVIE = 'The.Test.Movie.2024.1080p.WEB-DL.x264-E2E'

async function runTask(request: any, name: string) {
  const res = await request.post(`/api/v1/system/tasks/${name}/run`)
  expect(res.ok()).toBe(true)
}

test('calendar lists the aired episodes with file flags', async ({ request }) => {
  const entries = await (
    await request.get('/api/v1/calendar?start=2020-01-01&end=2020-01-31')
  ).json()
  expect(entries.length).toBe(2)
  expect(entries[0].kind).toBe('episode')
  expect(entries[0].title).toBe('The Test Show')
  expect(entries[0].detail).toContain('S01E01')
  expect(entries[0].hasFile).toBe(true)
})

test('wanted index is empty while everything is at cutoff', async ({ request }) => {
  const wanted = await (await request.get('/api/v1/wanted')).json()
  expect(wanted).toEqual([])
})

test('webhook notifier: test endpoint delivers to the sink', async ({ request }) => {
  let res = await request.post('/api/v1/notifiers/test', {
    data: { type: 'webhook', name: 'sink', settings: { url: `${ARR}/webhook-sink` } },
  })
  expect(res.status()).toBe(200)
  res = await request.post('/api/v1/notifiers', {
    data: { type: 'webhook', name: 'sink', settings: { url: `${ARR}/webhook-sink` } },
  })
  expect(res.status()).toBe(201)
  const listed = await (await request.get('/api/v1/notifiers')).json()
  expect(listed.length).toBe(1)

  const log = await (await request.get(`${ARR}/webhook-sink/log`)).json()
  expect(log.some((n: any) => n.event === 'test')).toBe(true)
})

test('deleted file → rescan → wanted → RSS auto-grab → re-imported + notified', async ({ request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const movieId = movies[0].id
  const detail = await (await request.get(`/api/v1/library/${movieId}`)).json()
  expect(detail.files.length).toBe(1)

  // A human deletes the file on disk (blueprint: tolerate filesystem edits).
  rmSync(detail.files[0].path)
  await request.post('/api/v1/library/scan')

  // The reconcile prunes the record and the movie becomes wanted.
  await expect
    .poll(
      async () => {
        const wanted = await (await request.get('/api/v1/wanted')).json()
        return wanted.some((w: any) => w.mediaItemId === movieId && w.missing)
      },
      { timeout: 15_000, intervals: [500] },
    )
    .toBe(true)

  // One RSS pass grabs it back without anyone clicking anything.
  await runTask(request, 'rss.sync')
  await expect
    .poll(
      async () => {
        await runTask(request, 'queue.refresh')
        const rows = await (await request.get('/api/v1/queue')).json()
        return rows.filter((r: any) => r.title === MOVIE && r.state === 'imported').length >= 2
      },
      { timeout: 20_000, intervals: [500] },
    )
    .toBe(true)

  const after = await (await request.get(`/api/v1/library/${movieId}`)).json()
  expect(after.files.length).toBe(1)

  // The webhook notifier heard about the import.
  await expect
    .poll(
      async () => {
        const log = await (await request.get(`${ARR}/webhook-sink/log`)).json()
        return log.some((n: any) => n.event === 'import' && n.body === MOVIE)
      },
      { timeout: 10_000, intervals: [500] },
    )
    .toBe(true)

  // And the wanted index drained again.
  const wanted = await (await request.get('/api/v1/wanted')).json()
  expect(wanted.filter((w: any) => w.mediaItemId === movieId)).toEqual([])
})

test('backup task writes a snapshot listed by the API', async ({ request }) => {
  await runTask(request, 'backup.run')
  await expect
    .poll(
      async () => {
        const backups = await (await request.get('/api/v1/system/backups')).json()
        return backups.length >= 1 && backups[0].sizeBytes > 0
      },
      { timeout: 10_000, intervals: [500] },
    )
    .toBe(true)
})

test('calendar page renders a month grid', async ({ page }) => {
  await page.goto('/calendar')
  await expect(page.getByRole('heading', { name: 'Calendar' })).toBeVisible()
  // The grid always renders: 7 day-of-week headers even with no entries.
  await expect(page.locator('.cal-dow')).toHaveCount(7)
  await expect(page.locator('.cal-cell').first()).toBeVisible()
})

test('wanted page renders with loop status', async ({ page }) => {
  await page.goto('/wanted')
  await expect(page.getByRole('heading', { name: 'Wanted' })).toBeVisible()
  await expect(page.getByText('RSS sync')).toBeVisible()
})
