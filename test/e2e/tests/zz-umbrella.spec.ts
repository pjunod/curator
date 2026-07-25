import { mkdirSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// One provider title covering several folders — the case where a match exists
// and still cannot be applied. Runs last (hence the filename) and against its
// own root folder, because it adds a library item and the earlier specs count
// the posters on the library page.
//
// Twice now this has reached a real library as a chip that looked like the
// answer, took a click and returned a conflict error. The Go rules have unit
// tests; what those cannot see is whether the chip is a button.
test.describe.configure({ mode: 'serial' })

const umbrellaRoot = mkdtempSync(join(tmpdir(), 'monarr-e2e-umbrella-'))
const britain = join(umbrellaRoot, 'Panel Show on Britain')
const earth = join(umbrellaRoot, 'Panel Show on Earth')

async function review(page: import('@playwright/test').Page) {
  await page.goto('/settings')
  await page.getByRole('button', { name: /Review \d+ folders/ }).click()
  await expect(page.getByRole('dialog', { name: 'Folders needing review' })).toBeVisible()
}

// Scan is async and adoption reads the last report, so adopting too early
// proposes from a stale one. The review queue then still lists the folders —
// it unions the queue with what the scan found — but as bare rows with no
// candidates, which looks like a matcher failure and is not one.
async function scanThenMatch(request: import('@playwright/test').APIRequestContext, want: number) {
  expect((await request.post('/api/v1/library/scan')).status()).toBe(202)
  await expect
    .poll(
      async () => {
        const res = await request.get('/api/v1/library/scan/report?limit=200')
        if (!res.ok()) return -1
        const report = await res.json()
        if (!report?.unmatchedDirs) return -1 // no scan has finished yet
        return report.unmatchedDirs.filter((d: { name: string }) =>
          d.name.startsWith('Panel Show'),
        ).length
      },
      { timeout: 15_000 },
    )
    .toBe(want)
  expect((await request.post('/api/v1/library/adopt')).status()).toBe(200)
}

async function queued(request: import('@playwright/test').APIRequestContext, name: string) {
  const page = await (await request.get('/api/v1/library/review?limit=200')).json()
  return page.items.find((p: { name: string }) => p.name === name)
}

test('two folders answering to one title say so before either is adopted', async ({
  page,
  request,
}) => {
  mkdirSync(britain, { recursive: true })
  mkdirSync(earth, { recursive: true })

  // Self-sufficient: the walking flow in library.spec.ts sets the key, but
  // this file should also pass when run on its own.
  expect(
    (await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })).status(),
  ).toBe(204)

  const res = await request.post('/api/v1/rootfolders', {
    data: { path: umbrellaRoot, kind: 'series' },
  })
  expect(res.status()).toBe(201)

  await scanThenMatch(request, 2)
  expect((await queued(request, 'Panel Show on Earth')).sharedWith).toHaveLength(1)

  await review(page)
  const rows = page.locator('.review-row', { hasText: 'Panel Show on' })
  await expect(rows).toHaveCount(2)
  // Both rows name the collision, and both chips are still live: one of them
  // can legitimately have the entry.
  await expect(page.locator('.review-note').first()).toContainText('2 folders answer to')
  await expect(page.locator('.candidate.taken')).toHaveCount(0)
})

test('once one holds the title the other stops offering it as a match', async ({
  page,
  request,
}) => {
  const adopt = await request.post('/api/v1/library/adopt/one', {
    data: { path: britain, kind: 'series', tmdbId: 900, title: 'Panel Show on...', year: 2018 },
  })
  expect(adopt.status()).toBe(204)

  await scanThenMatch(request, 1)
  expect((await queued(request, 'Panel Show on Earth')).heldBy).toBe(britain)

  await review(page)
  const row = page.locator('.review-row', { hasText: 'Panel Show on Earth' })
  await expect(row.locator('.review-note')).toContainText('is already this library')
  await expect(row.locator('.review-note')).toContainText('Panel Show on Britain')

  // The chip is present and inert. Adopting it cannot succeed, so it must not
  // be something you can press: a click that answers with a conflict error is
  // a worse way of saying what the note already says.
  const taken = row.locator('.candidate.taken')
  await expect(taken).toHaveCount(1)
  await expect(taken).toContainText('Panel Show on...')
  await expect(row.getByRole('button', { name: /Panel Show on\.\.\./ })).toHaveCount(0)

  await taken.click({ force: true })
  await expect(row.locator('.review-error')).toHaveCount(0)

  // What is left is honest: search for a separate entry, or stop being asked.
  await expect(row.getByRole('button', { name: 'search…' })).toBeVisible()
  await expect(row.getByRole('button', { name: 'stop offering' })).toBeVisible()
})

// The series chain (ADR 0011). The fake provider holds one show the fake
// TMDB has never heard of, so a folder named after it can only be placed by
// the second link — and the item that lands must be keyed on its TVDB id,
// which is what lets a TheTVDB adapter take over later without re-matching.
test('a folder only the chain can place is adopted and keyed on its TVDB id', async ({
  request,
}) => {
  const dir = join(umbrellaRoot, 'The Chain Only Show')
  mkdirSync(dir, { recursive: true })

  expect((await request.post('/api/v1/library/scan')).status()).toBe(202)
  await expect
    .poll(
      async () => {
        const res = await request.get('/api/v1/library/scan/report?limit=200')
        if (!res.ok()) return -1
        const report = await res.json()
        return (report?.unmatchedDirs ?? []).filter((d: { name: string }) =>
          d.name.startsWith('The Chain Only'),
        ).length
      },
      { timeout: 15_000 },
    )
    .toBe(1)
  expect((await request.post('/api/v1/library/adopt')).status()).toBe(200)

  // Proposed as a confident match, carrying the chain's id and its name.
  const page = await (await request.get('/api/v1/library/review?limit=200')).json()
  const row = page.items.find((p: { name: string }) => p.name === 'The Chain Only Show')
  expect(row.confidence).toBe('exact')
  expect(row.candidates[0].tvdbId).toBe(510000)
  expect(row.candidates[0].source).toBe('tvmaze')
  expect(row.candidates[0].tmdbId).toBe(0)

  expect((await request.post('/api/v1/library/adopt/exact')).status()).toBe(200)

  const list = await (await request.get('/api/v1/library?kind=series')).json()
  const added = list.find((m: { title: string }) => m.title === 'The Chain Only Show')
  expect(added).toBeTruthy()
  expect(added.path).toBe(dir)

  const detail = await (await request.get(`/api/v1/library/${added.id}`)).json()
  expect(detail.ids.tvdb).toBe(510000)
  expect(detail.ids.tmdb).toBe(0) // nothing invented a TMDB id for it
  expect(detail.seasons[0].episodes).toHaveLength(2)
})
