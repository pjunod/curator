import { expect, test } from '@playwright/test'
import { openFixtureDatabase } from '../fixture-db'

// Phase 2 full loop against fake torznab + fake qBittorrent:
// configure → interactive search (with rejection reasons) → grab →
// client completes → auto-import → correctly named file in the library.
// Runs after library.spec.ts (alphabetical), which added the movie/series.
test.describe.configure({ mode: 'serial' })

const ARR = `http://127.0.0.1:${process.env.FAKE_ARR_PORT ?? 7799}`

async function itemIdByKind(request: any, kind: string): Promise<number> {
  const items = await (await request.get(`/api/v1/library?kind=${kind}`)).json()
  expect(items.length).toBeGreaterThan(0)
  return items[0].id
}

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

test('configure indexer and download client via the API', async ({ request }) => {
  let res = await request.post('/api/v1/indexers/test', {
    data: { name: 'fake', url: ARR, apiKey: 'k', protocol: 'torrent' },
  })
  expect(res.status()).toBe(200)
  res = await request.post('/api/v1/indexers', {
    data: { name: 'fake', url: ARR, apiKey: 'k', protocol: 'torrent', enabled: true },
  })
  expect(res.status()).toBe(201)

  res = await request.post('/api/v1/downloadclients/test', {
    data: { type: 'qbittorrent', name: 'fakeqb', url: ARR, username: 'a', password: 'b' },
  })
  expect(res.status()).toBe(200)
  res = await request.post('/api/v1/downloadclients', {
    data: { type: 'qbittorrent', name: 'fakeqb', url: ARR, username: 'a', password: 'b', category: 'monarr', enabled: true },
  })
  expect(res.status()).toBe(201)
})

test('movie: search shows ranked candidates with rejection reasons', async ({ request }) => {
  const movieId = await itemIdByKind(request, 'movie')
  const cands = await (await request.get(`/api/v1/library/${movieId}/releases`)).json()
  expect(cands.length).toBe(2)
  // Best first: accepted WEB-DL 1080p.
  expect(cands[0].accepted).toBe(true)
  expect(cands[0].quality).toBe('WEB-DL 1080p')
  // The CAM release is present but rejected with a reason.
  const cam = cands.find((c: any) => c.title.includes('HDCAM'))
  expect(cam.accepted).toBe(false)
  expect(cam.rejections[0].reason).toContain('not allowed')
})

test('movie: explicit add imports a separately identified copy', async ({ request }) => {
  const movieId = await itemIdByKind(request, 'movie')
  const cands = await (await request.get(`/api/v1/library/${movieId}/releases`)).json()
  const pick = cands[0]

  const grab = await request.post('/api/v1/grab', {
    data: {
      mediaItemId: movieId, title: pick.title, downloadUrl: pick.downloadUrl,
      indexer: pick.indexer, protocol: pick.protocol, size: pick.size,
      candidateToken: pick.candidateToken, intent: 'additional_copy', requestId: 'phase2-movie-grab', downloadAnyway: false,
    },
  })
  expect(grab.status()).toBe(201)

  await refreshQueueUntil(request, (rows) =>
    rows.some((r) => r.title === pick.title && r.state === 'imported'),
  )

  const detail = await (await request.get(`/api/v1/library/${movieId}`)).json()
  expect(detail.files.length).toBe(1)
  expect(detail.files[0].path).toContain('The Test Movie (2024) [WEB-DL 1080p].mkv')
  expect(detail.files[0].copyId).toBeGreaterThan(0)
  expect(detail.files[0].path).toContain('(Manual copy ')
})

test('series: season pack grab fans out to every episode', async ({ request }) => {
  const seriesId = await itemIdByKind(request, 'series')
  const cands = await (
    await request.get(`/api/v1/library/${seriesId}/releases?season=1`)
  ).json()
  const pack = cands.find((c: any) => c.title.includes('S01.1080p') && c.title.includes('-E2E'))
  expect(pack).toBeTruthy()

  const grab = await request.post('/api/v1/grab', {
    data: {
      mediaItemId: seriesId, season: 1, title: pack.title,
      downloadUrl: pack.downloadUrl, indexer: pack.indexer, protocol: pack.protocol,
      candidateToken: pack.candidateToken, intent: 'additional_copy', requestId: 'phase2-series-pack', downloadAnyway: false,
    },
  })
  expect(grab.status()).toBe(201)

  await refreshQueueUntil(request, (rows) =>
    rows.some((r) => r.title === pack.title && r.state === 'imported'),
  )

  const detail = await (await request.get(`/api/v1/library/${seriesId}`)).json()
  const s1 = detail.seasons.find((s: any) => s.number === 1)
  for (const ep of s1.episodes) {
    expect(ep.hasFile).toBe(true)
  }
  const renamed = detail.files.filter((f: any) => f.path.includes('[WEB-DL 1080p]'))
  expect(renamed.length).toBeGreaterThanOrEqual(2)
})

test('automatic RSS fills monitored primary copies beside explicit additions', async ({ request }) => {
  const db = openFixtureDatabase()
  db.prepare('UPDATE indexers SET next_rss_at=0').run()
  db.close()
  const response = await request.post('/api/v1/system/tasks/rss.sync/run')
  expect(response.ok()).toBe(true)

  const movieId = await itemIdByKind(request, 'movie')
  const seriesId = await itemIdByKind(request, 'series')
  await expect.poll(async () => {
    await request.post('/api/v1/system/tasks/queue.refresh/run')
    const movie = await (await request.get(`/api/v1/library/${movieId}`)).json()
    const series = await (await request.get(`/api/v1/library/${seriesId}`)).json()
    const season = series.seasons.find((row: any) => row.number === 1)
    const db = openFixtureDatabase()
    const primaryEpisodes = db.prepare(`
      SELECT DISTINCT mfe.episode_id
      FROM media_files mf
      JOIN media_file_episodes mfe ON mfe.media_file_id = mf.id
      WHERE mf.media_item_id = ? AND mf.copy_id IS NULL
    `).all(seriesId)
    const autoPackFiles = db.prepare(`
      SELECT COUNT(*) AS count FROM media_files
      WHERE media_item_id = ? AND copy_id IS NULL AND source_release LIKE '%-AUTO'
    `).get(seriesId).count
    db.close()
    return [
      movie.files.some((file: any) => !file.path.includes('(Manual copy ')),
      primaryEpisodes.length === season.episodes.length && autoPackFiles >= season.episodes.length,
    ]
  }, { timeout: 15_000, intervals: [500] }).toEqual([true, true])

  await expect.poll(async () => {
    const wanted = await (await request.get('/api/v1/wanted')).json()
    return wanted.filter((row: any) => [movieId, seriesId].includes(row.mediaItemId) && row.missing)
  }, { timeout: 5_000, intervals: [250] }).toEqual([])
})

test('activity page shows the imported downloads', async ({ page }) => {
  await page.goto('/activity')
  // The counts are always on, even with everything collapsed: the page has
  // to answer "how much is there" before it answers "what is it".
  await expect(page.getByTestId('activity-counts')).toContainText('finished')
  // Finished is history and starts collapsed - that is the whole point of
  // the section. Open it to see the receipts.
  await page.getByTestId('toggle-imported').click()
  await expect(page.getByText('The.Test.Movie.2024.1080p.WEB-DL.x264-E2E')).toBeVisible()
  await expect(page.locator('.pill-ok', { hasText: 'imported' }).first()).toBeVisible()
})
