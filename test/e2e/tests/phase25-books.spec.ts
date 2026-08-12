import { expect, test } from '@playwright/test'

// Phase 2.5 (ADR 0006): the full book loop — search Open Library, add,
// interactive release search (format-graded), grab an EPUB, auto-import
// into the Calibre-friendly Author/Title layout.
// Runs after phase2.spec.ts (alphabetical), which configured the indexer
// and download client; library.spec.ts created the root folder.
test.describe.configure({ mode: 'serial' })

let bookId = 0
let audiobookCopyId = 0
let standaloneAudiobookId = 0

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

test('ebook release search stays in ebook categories', async ({ request }) => {
  const cands = await (await request.get(`/api/v1/library/${bookId}/releases`)).json()
  expect(cands.length).toBe(1)
  expect(cands[0].accepted).toBe(true)
  expect(cands[0].quality).toBe('EPUB')
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

test('add the audiobook beside the ebook and curate it independently', async ({ request }) => {
  const roots = await (await request.get('/api/v1/rootfolders')).json()
  const added = await request.post('/api/v1/library', {
    data: {
      kind: 'book', bookType: 'audiobook', olid: 'OL900E2EW',
      rootFolderId: roots[0].id, monitored: true,
    },
  })
  expect(added.status()).toBe(201)
  const item = await added.json()
  expect(item.id).toBe(bookId)
  expect(item.bookType).toBe('ebook')
  expect(item.bookTypes).toEqual(['ebook', 'audiobook'])
  expect(item.qualityProfileId).toBe(4)
  expect(item.copies).toHaveLength(1)
  expect(item.copies[0].bookType).toBe('audiobook')
  expect(item.copies[0].qualityProfileId).toBe(5)
  audiobookCopyId = item.copies[0].id

  const cands = await (
    await request.get(`/api/v1/library/${bookId}/releases?copyId=${audiobookCopyId}`)
  ).json()
  expect(cands).toHaveLength(1)
  expect(cands[0].quality).toBe('M4B')
  expect(cands[0].accepted).toBe(true)
  const pick = cands[0]
  expect((await request.post('/api/v1/grab', {
    data: {
      mediaItemId: bookId, copyId: audiobookCopyId,
      title: pick.title, downloadUrl: pick.downloadUrl,
      indexer: pick.indexer, protocol: pick.protocol, size: pick.size,
    },
  })).status()).toBe(201)
  await refreshQueueUntil(request, (rows) =>
    rows.some((r) => r.title === pick.title && r.copyId === audiobookCopyId && r.state === 'imported'),
  )
  const detail = await (await request.get(`/api/v1/library/${bookId}`)).json()
  expect(detail.files).toHaveLength(2)
  expect(detail.files).toEqual(expect.arrayContaining([
    expect.objectContaining({ copyId: audiobookCopyId, path: expect.stringContaining('The Test Book - Test Author.m4b') }),
    expect.objectContaining({ path: expect.stringContaining('The Test Book - Test Author.epub') }),
  ]))
})

test('a standalone audiobook still imports every multipart track', async ({ request }) => {
  const results = await (
    await request.get('/api/v1/metadata/search?kind=book&query=test%20audiobook')
  ).json()
  expect(results).toHaveLength(1)
  const roots = await (await request.get('/api/v1/rootfolders')).json()
  const added = await request.post('/api/v1/library', {
    data: {
      kind: 'book', bookType: 'audiobook', olid: 'OL901A2AW',
      rootFolderId: roots[0].id, monitored: true,
    },
  })
  expect(added.status()).toBe(201)
  const item = await added.json()
  standaloneAudiobookId = item.id
  expect(item.bookType).toBe('audiobook')
  expect(item.qualityProfileId).toBe(5)

  const cands = await (await request.get(`/api/v1/library/${standaloneAudiobookId}/releases`)).json()
  expect(cands).toHaveLength(1)
  expect(cands[0].quality).toBe('M4B')
  expect(cands[0].accepted).toBe(true)
  const pick = cands[0]
  expect((await request.post('/api/v1/grab', {
    data: {
      mediaItemId: standaloneAudiobookId, title: pick.title, downloadUrl: pick.downloadUrl,
      indexer: pick.indexer, protocol: pick.protocol, size: pick.size,
    },
  })).status()).toBe(201)
  await refreshQueueUntil(request, (rows) =>
    rows.some((r) => r.title === pick.title && r.state === 'imported'),
  )
  const detail = await (await request.get(`/api/v1/library/${standaloneAudiobookId}`)).json()
  expect(detail.files).toHaveLength(3)
  expect(detail.files.map((f: any) => f.path)).toEqual(expect.arrayContaining([
    expect.stringContaining('The Test Audiobook - Audio Author - 001.m4b'),
    expect.stringContaining('The Test Audiobook - Audio Author - 002.m4b'),
    expect.stringContaining('The Test Audiobook - Audio Author - 003.m4b'),
  ]))
})

test('library shows one work in both edition sections', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('tab', { name: 'Ebooks' }).click()
  await expect(page.getByText('The Test Book')).toBeVisible()
  await expect(page.getByText('Test Author')).toBeVisible()
  await expect(page.getByText('The Test Audiobook')).toHaveCount(0)
  await page.getByRole('tab', { name: 'Audiobooks' }).click()
  await expect(page.getByText('The Test Audiobook')).toBeVisible()
  await expect(page.getByText('Audio Author')).toBeVisible()
  await expect(page.getByText('The Test Book')).toBeVisible()

  await page.goto(`/library/${bookId}`)
  await expect(page.getByRole('heading', { name: 'Editions' })).toBeVisible()
  await expect(page.getByRole('row', { name: /Ebook/ })).toBeVisible()
  await expect(page.getByRole('row', { name: /Audiobook/ })).toBeVisible()
})
