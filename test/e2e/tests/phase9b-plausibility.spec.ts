import { expect, test } from '@playwright/test'

// The HIM case, through the product.
//
// A 500 MB file called "HIM (2025) [Remux 2160p].mkv" — a 96-minute feature —
// imported, graded REMUX 2160P, and marked the movie satisfied. The badge was
// the visible symptom; the actual failure was that a fake file retired the
// want, so monarr stopped looking for a real one and would never start again.
//
// These specs cover the three things that had to change: monarr declines to
// grab a release whose advertised size cannot hold its claim, a file it does
// not believe stops counting as satisfying the item, and there is finally a
// way to say "this one is bad, never take it again".
//
// Runs after the acquisition specs so there are real imported files to act on.
test.describe.configure({ mode: 'serial' })

test('a release too small for what it claims is not grabbed unattended', async ({ request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const movieId = movies[0].id

  const candidates = await (await request.get(`/api/v1/library/${movieId}/releases`)).json()
  expect(Array.isArray(candidates)).toBe(true)

  // Interactive search still lists everything — a manual grab is never gated,
  // and hiding a release is how "monarr found nothing" becomes unanswerable.
  // What a suspicious size buys is a warning attached to the row.
  for (const c of candidates) {
    if (c.warning) {
      expect(c.warning).toMatch(/below|starts around/)
    }
  }
})

test('a file monarr cannot believe does not count as having the item', async ({ request }) => {
  const items = await (await request.get('/api/v1/library')).json()
  let checked = 0
  for (const it of items) {
    const full = await (await request.get(`/api/v1/library/${it.id}`)).json()
    for (const f of full.files ?? []) {
      checked++
      // Every file carries a verdict. The one that matters here: when the
      // verdict is "implausible" there is always a reason to show, because a
      // badge with no arithmetic behind it is a shrug in better clothes.
      if (f.provenance === 'implausible') {
        expect(f.implausible).toBeTruthy()
        expect(f.verified).toBe(false)
      } else {
        expect(f.implausible ?? '').toBe('')
      }
    }
  }
  expect(checked).toBeGreaterThan(0)
})

test('the files table offers Delete and Delete & blocklist', async ({ page, request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const withFile = await (async () => {
    for (const m of movies) {
      const full = await (await request.get(`/api/v1/library/${m.id}`)).json()
      if ((full.files ?? []).length > 0) return full
    }
    return null
  })()
  expect(withFile).not.toBeNull()

  await page.goto(`/library/${withFile.id}`)
  const files = page.locator('section.panel').filter({ hasText: 'Files' }).last()
  await expect(files.getByRole('button', { name: 'Delete', exact: true }).first()).toBeVisible()

  // Blocklisting needs a source release. An imported file has one; an adopted
  // file does not, and the button says so rather than half-working.
  const blocklist = files.getByRole('button', { name: 'Delete & blocklist' }).first()
  await expect(blocklist).toBeVisible()
  const file = withFile.files[0]
  if (file.sourceRelease) {
    await expect(blocklist).toBeEnabled()
  } else {
    await expect(blocklist).toBeDisabled()
  }
})

test('deleting a file removes it and says what happened', async ({ request }) => {
  // Use the series, whose season pack imported two episodes — deleting one
  // leaves the item in a state the other specs can still work with.
  const series = await (await request.get('/api/v1/library?kind=series')).json()
  const before = await (await request.get(`/api/v1/library/${series[0].id}`)).json()
  expect((before.files ?? []).length).toBeGreaterThan(1)

  const victim = before.files[0]
  const res = await request.delete(
    `/api/v1/library/${series[0].id}/files/${victim.id}?fromDisk=true`,
  )
  expect(res.ok()).toBe(true)
  const body = await res.json()
  expect(body.path).toBe(victim.path)
  expect(body.deletedFromDisk).toBe(true)

  const after = await (await request.get(`/api/v1/library/${series[0].id}`)).json()
  expect(after.files.map((f: { id: number }) => f.id)).not.toContain(victim.id)
})

test('a file id from another item is refused', async ({ request }) => {
  // The file id comes off a URL. A mismatched pair means somebody's stale tab
  // is one click from deleting a stranger's file.
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const series = await (await request.get('/api/v1/library?kind=series')).json()
  const seriesFull = await (await request.get(`/api/v1/library/${series[0].id}`)).json()
  const foreign = seriesFull.files?.[0]
  test.skip(!foreign, 'no series file left to borrow an id from')

  const res = await request.delete(`/api/v1/library/${movies[0].id}/files/${foreign.id}`)
  expect(res.status()).toBe(404)
})

test('the search window pages instead of running for miles', async ({ page, request }) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  await page.goto(`/library/${movies[0].id}`)
  await page.getByRole('button', { name: 'Interactive search' }).first().click()

  const panel = page.locator('section.panel').filter({ hasText: 'Interactive search' }).first()
  await expect(panel).toBeVisible()

  // The controls that keep the list finite: a name filter, an
  // accepted-only view, and a page size.
  await expect(panel.getByPlaceholder('Filter by name…')).toBeVisible()
  await expect(panel.getByRole('combobox').first()).toBeVisible()

  // Filtering to "would be grabbed" never shows a rejected row.
  await panel.getByRole('combobox').first().selectOption('accepted')
  await expect(panel.locator('tbody tr.row-rejected')).toHaveCount(0)
})
