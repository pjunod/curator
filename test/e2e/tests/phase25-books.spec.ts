import { expect, test } from '@playwright/test'

// Phase 2.5 (ADR 0006): the full book loop — search Open Library, add,
// interactive release search (format-graded), grab an EPUB, auto-import
// into the Calibre-friendly Author/Title layout.
// Runs after phase2.spec.ts (alphabetical), which configured the indexer
// and download client; library.spec.ts created the root folder.
test.describe.configure({ mode: 'serial' })

let bookId = 0

async function refreshQueueUntil(request: any, pred: (rows: any[]) => boolean) {
  await expect
    .poll(
      async () => {
        await request.post('/api/v1/system/tasks/queue.refresh/run')
        const rows = await (await request.get('/api/v1/queue')).json()
        return pred(rows)
      },
      { timeout: 15_000, intervals: [500] },
    )
    .toBe(true)
}

test('search Open Library and add the book', async ({ request }) => {
  const results = await (
    await request.get('/api/v1/metadata/search?kind=book&query=test%20book')
  ).json()
  expect(results.length).toBe(1)
  expect(results[0].olid).toBe('OL900E2EW')
  expect(results[0].author).toBe('Test Author')

  const roots = await (await request.get('/api/v1/rootfolders')).json()
  expect(roots.length).toBeGreaterThan(0)

  const res = await request.post('/api/v1/library', {
    data: { kind: 'book', olid: 'OL900E2EW', rootFolderId: roots[0].id, monitored: true },
  })
  expect(res.status()).toBe(201)
  const item = await res.json()
  bookId = item.id
  expect(item.author).toBe('Test Author')
  expect(item.ids.isbn13).toBe('9781000000001')
  // Calibre-friendly derived folder + Ebook profile default.
  expect(item.path).toContain('Test Author/The Test Book')
})

test('book release search grades formats; audiobook rejected by Ebook profile', async ({ request }) => {
  const cands = await (await request.get(`/api/v1/library/${bookId}/releases`)).json()
  expect(cands.length).toBe(2)
  expect(cands[0].accepted).toBe(true)
  expect(cands[0].quality).toBe('EPUB')
  const m4b = cands.find((c: any) => c.title.includes('M4B'))
  expect(m4b.accepted).toBe(false)
  expect(m4b.rejections[0].reason).toContain('not allowed')
})

test('grab EPUB → auto-import → Calibre-friendly file name', async ({ request }) => {
  const cands = await (await request.get(`/api/v1/library/${bookId}/releases`)).json()
  const pick = cands[0]
  const grab = await request.post('/api/v1/grab', {
    data: {
      mediaItemId: bookId, title: pick.title, downloadUrl: pick.downloadUrl,
      indexer: pick.indexer, protocol: pick.protocol, size: pick.size,
    },
  })
  expect(grab.status()).toBe(201)

  await refreshQueueUntil(request, (rows) =>
    rows.some((r) => r.title === pick.title && r.state === 'imported'),
  )

  const detail = await (await request.get(`/api/v1/library/${bookId}`)).json()
  expect(detail.files.length).toBe(1)
  expect(detail.files[0].path).toContain(
    'Test Author/The Test Book/The Test Book - Test Author.epub',
  )
})

test('library Books tab shows the book with its author', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('tab', { name: 'Books' }).click()
  await expect(page.getByText('The Test Book')).toBeVisible()
  await expect(page.getByText('Test Author')).toBeVisible()
})
