import { expect, test } from '@playwright/test'

// Phase 4: the Sonarr/Radarr personalities on the real binary — auth,
// case-insensitivity, version strings, and library visibility through the
// shim (the movie/series added by earlier specs must appear).
test.describe.configure({ mode: 'serial' })

let apiKey = ''

test('monarr generates and reveals its API key', async ({ request }) => {
  const settings = await (await request.get('/api/v1/settings')).json()
  expect(settings.apiKey).toBeTruthy()
  apiKey = settings.apiKey
})

test('personalities require X-Api-Key and identify as Sonarr/Radarr', async ({ request }) => {
  let res = await request.get('/sonarr/api/v3/system/status')
  expect(res.status()).toBe(401)

  res = await request.get('/sonarr/api/v3/system/status', {
    headers: { 'X-Api-Key': apiKey },
  })
  expect(res.status()).toBe(200)
  const sonarr = await res.json()
  expect(sonarr.appName).toBe('Sonarr')
  expect(sonarr.version).toMatch(/^4\./)

  const radarr = await (
    await request.get('/radarr/api/v3/system/status', { headers: { 'X-Api-Key': apiKey } })
  ).json()
  expect(radarr.appName).toBe('Radarr')
  expect(radarr.version).toMatch(/^5\./)
})

test('routes are case-insensitive like the real apps', async ({ request }) => {
  const res = await request.get('/sonarr/API/V3/System/Status', {
    headers: { 'X-Api-Key': apiKey },
  })
  expect(res.status()).toBe(200)
})

test('the library shows through both personalities', async ({ request }) => {
  const series = await (
    await request.get('/sonarr/api/v3/series', { headers: { 'X-Api-Key': apiKey } })
  ).json()
  expect(series.length).toBe(1)
  expect(series[0].title).toBe('The Test Show')
  expect(series[0].statistics.episodeFileCount).toBe(2)
  expect(series[0].seasons[0].monitored).toBe(true)

  const movies = await (
    await request.get('/radarr/api/v3/movie', { headers: { 'X-Api-Key': apiKey } })
  ).json()
  expect(movies.length).toBe(1)
  expect(movies[0].title).toBe('The Test Movie')
  expect(movies[0].hasFile).toBe(true)

  // Books stay native-only: the personalities never leak the third kind.
  expect(series.some((s: any) => s.title === 'The Test Book')).toBe(false)
  expect(movies.some((m: any) => m.title === 'The Test Book')).toBe(false)
})

test('quality profiles and root folders translate', async ({ request }) => {
  const profiles = await (
    await request.get('/radarr/api/v3/qualityprofile', { headers: { 'X-Api-Key': apiKey } })
  ).json()
  expect(profiles.length).toBe(5)
  expect(profiles.map((p: any) => p.name)).toContain('HD-1080p')

  const roots = await (
    await request.get('/sonarr/api/v3/rootfolder', { headers: { 'X-Api-Key': apiKey } })
  ).json()
  expect(roots.length).toBeGreaterThan(0)
  expect(roots[0].accessible).toBe(true)
})

test('unknown v3 requests 404 with a shim message (and get logged)', async ({ request }) => {
  const res = await request.get('/sonarr/api/v3/importlist', {
    headers: { 'X-Api-Key': apiKey },
  })
  expect(res.status()).toBe(404)
  const body = await res.json()
  expect(body.message).toContain('monarr')
})
