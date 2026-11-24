import { expect, test, type Locator, type Page } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

test.use({ serviceWorkers: 'block' })

async function inViewport(locator: Locator) {
  // Chromium can round scroll offsets by a fraction of a CSS pixel. The
  // bounding-box assertion below still rejects any meaningful clipping.
  await expect(locator).toBeInViewport({ ratio: 0.99 })
  // Check overflow ancestors too: percentage alone can hide several clipped
  // pixels on a wide control. Permit at most one CSS pixel at each edge.
  await expect.poll(() => locator.evaluate(node => new Promise<number>(resolve => {
    const observer = new IntersectionObserver(([entry]) => {
      observer.disconnect()
      const a = entry.boundingClientRect, b = entry.intersectionRect
      resolve(Math.max(b.left - a.left, b.top - a.top, a.right - b.right, a.bottom - b.bottom))
    })
    observer.observe(node)
  }))).toBeLessThanOrEqual(1)
  await expect.poll(() => locator.evaluate(node => {
    const box = node.getBoundingClientRect()
    return box.top >= 0 && box.left >= 0 && box.bottom <= innerHeight + 1 && box.right <= innerWidth + 1
  })).toBe(true)
}
async function screenshot(page: Page, name: string) {
  const directory = process.env.LIFECYCLE_SCREENSHOTS_DIR
  if (!directory) return
  mkdirSync(directory, { recursive: true })
  await page.screenshot({ path: join(directory, `${name}-${test.info().project.name}.png`) })
}
async function emptyActivity(page: Page) {
  await page.route('**/api/v1/queue/summary', route => route.fulfill({ json: { total: 0, active: 0, counts: {} } }))
  await page.route('**/api/v1/queue?*', route => route.fulfill({ json: [] }))
  await page.route('**/api/v1/system/tasks', route => route.fulfill({ json: [] }))
}

test('storage comparison previews impact, waits for refresh and accepts exact reviewed identities', async ({ page }) => {
  await emptyActivity(page)
  const expectedIdentity = 'linux-v1\u0000ext4\u0000UUID=original-volume\u0000/'
  const observedIdentity = 'linux-v1\u0000ext4\u0000UUID=replacement-volume\u0000/'
  let accepted = false
  let submitted: Record<string, unknown> | undefined
  const root = { owner: 'Curator', role: 'downloads', status: 'unavailable', path: '/completed/replacement', observedAt: new Date().toISOString(), storageIdentity: expectedIdentity, observedStorageIdentity: observedIdentity, error: 'Expected storage changed: original volume; observed replacement volume' }
  await page.route('**/api/v1/system/storage-observations', route => route.fulfill({ json: { roots: [{ ...root, ...(accepted ? { storageIdentity: observedIdentity, error: '' } : {}) }], cleanup: {} } }))
  let previews = 0
  let releasePreview: (() => void) | undefined
  await page.route('**/api/v1/downloads/inventory/storage-change/preview', async route => {
    previews++
    if (previews > 1) await new Promise<void>(resolve => { releasePreview = resolve })
    expect(route.request().postDataJSON()).toMatchObject({ root: root.path, expectedIdentity, observedIdentity })
    await route.fulfill({ json: { root: root.path, expectedIdentity, observedIdentity, reviewToken: `exact-review-${previews}`, expectedDescription: 'ext4 · original volume', observedDescription: 'ext4 · replacement volume', existingPayloads: 2, existingRequests: 3, inFlightRequests: 1, activeImports: 0 } })
  })
  await page.route('**/api/v1/downloads/inventory/storage-change/accept', async route => { submitted = route.request().postDataJSON(); accepted = true; await route.fulfill({ status: 204 }) })
  await page.goto('/system#storage')
  await page.getByRole('button', { name: 'Review replacement storage' }).click()
  const dialog = page.getByRole('dialog', { name: 'Review replacement storage' })
  await expect(dialog.getByText(/Existing jobs keep their original storage requirements/)).toBeVisible()
  await expect(dialog.getByText('ext4 · original volume')).toBeVisible()
  await expect(dialog.getByText('ext4 · replacement volume')).toBeVisible()
  await inViewport(dialog.getByRole('heading'))
  await screenshot(page, 'storage-comparison')
  const accept = dialog.getByRole('button', { name: 'Accept replacement storage' })
  await dialog.getByRole('button', { name: 'Refresh review' }).click()
  await expect(dialog.getByText('Checking the current drive and affected requests…')).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Accept replacement storage', disabled: false })).toHaveCount(0)
  await expect.poll(() => releasePreview !== undefined).toBe(true)
  releasePreview!()
  await expect(accept).toBeEnabled()
  await accept.click()
  await expect(dialog).toHaveCount(0)
  expect(submitted).toMatchObject({ root: root.path, expectedIdentity, observedIdentity, reviewToken: 'exact-review-2' })
  await inViewport(page.getByRole('status').filter({ hasText: 'Replacement storage accepted' }))
})

async function fileReviewFixture(page: Page, kind: 'movie' | 'series' = 'movie') {
  let files = [{ id: 44, path: '/library/Reviewed/file.mkv', size: 1234, episodeIds: kind === 'series' ? [101, 102] : [], copyId: 0,
    fileGeneration: 'file-generation-44', sourceGeneration: 'source-generation-44', sourceTransfer: 'transfer-44', sourceRelease: 'Reviewed.Source', sourceGuid: 'source-guid', sourceIndexerId: 8, sourceIndexer: 'Indexer' }]
  const item = () => ({ id: 9878, kind, title: 'Reviewed lifecycle', monitored: true, path: '/library/Reviewed', genres: [], ratings: [], ids: {}, copies: [], files,
    seasons: kind === 'series' ? [{ number: 1, monitored: true, episodes: [{ id: 101, seasonNumber: 1, episodeNumber: 1, title: 'First selected episode', monitored: true }, { id: 102, seasonNumber: 1, episodeNumber: 2, title: 'Healthy sibling', monitored: true }] }] : [] })
  await page.route('**/api/v1/library/9878', route => route.fulfill({ json: item() }))
  await page.route('**/api/v1/library/9878/replacement-operations', async route => {
    if (route.request().method() === 'GET') await route.fulfill({ json: { operations: [], openReports: [] } })
    else await route.fallback()
  })
  await page.route('**/api/v1/library/9878/files/44/replacement-observations', route => route.fulfill({ status: 202, json: { observationId: 'review-44', requestId: 'review-request', state: 'pending', fileGeneration: 'file-generation-44' } }))
  await page.route('**/api/v1/replacement-observations/review-44', route => route.fulfill({ json: { observationId: 'review-44', requestId: 'review-request', state: 'ready', fileGeneration: 'file-generation-44', sourceGeneration: 'source-generation-44', sha256: 'b'.repeat(64), size: 1234 } }))
  await page.route('**/api/v1/library/9878/files/44?*', async route => { files = []; await route.fulfill({ json: { deletedFromDisk: true, note: 'File removed from disk and library.' } }) })
  await page.goto('/library/9878')
  await page.locator('.item-files > details > summary').click()
}

test('report and file-deletion results remain visible and restore useful focus', async ({ page }) => {
  await fileReviewFixture(page)
  let report: Record<string, unknown> | undefined
  await page.route('**/api/v1/library/9878/files/44/report', async route => { report = route.request().postDataJSON(); await route.fulfill({ status: 204 }) })
  const trigger = page.getByRole('button', { name: 'Report', exact: true })
  await trigger.click()
  const dialog = page.getByRole('dialog', { name: 'Report library file' })
  await dialog.locator('textarea').fill('Playback stops at the same frame.')
  await dialog.getByRole('button', { name: 'Save report' }).click()
  await expect(dialog.getByRole('status')).toContainText('Report saved')
  await inViewport(dialog.getByRole('status'))
  expect(report).toMatchObject({ fileGeneration: 'file-generation-44', reason: 'Playback stops at the same frame.' })
  await dialog.getByRole('button', { name: 'Done' }).click()
  await expect(trigger).toBeFocused()
  await page.getByRole('button', { name: 'Delete…', exact: true }).click()
  await page.getByRole('dialog', { name: 'Delete library file' }).getByRole('button', { name: 'Delete file', exact: true }).click()
  const result = page.getByRole('dialog', { name: 'File deletion result' })
  await expect(page.locator('.item-files tbody tr')).toHaveCount(0)
  await inViewport(result.getByRole('status'))
  await screenshot(page, 'file-delete-result')
  await result.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.locator('.item-files > details > summary')).toBeFocused()
})

for (const intent of ['replace_bad', 'replace_quality'] as const) {
  test(`${intent} submits reviewed episode scope and shows successful background operation`, async ({ page }) => {
    await fileReviewFixture(page, 'series')
    let submitted: Record<string, unknown> | undefined
    await page.route('**/api/v1/library/9878/replacement-operations', async route => {
      if (route.request().method() === 'GET') { await route.fulfill({ json: { operations: [], openReports: [] } }); return }
      submitted = route.request().postDataJSON()
      await route.fulfill({ status: 202, json: { operationId: 'operation-44', requestId: submitted!.requestId, state: 'searching', intent, scope: 'episodes', blacklistSourceRelease: false } })
    })
    await page.route('**/api/v1/replacement-operations/operation-44', route => route.fulfill({ json: { operationId: 'operation-44', requestId: 'request-44', state: 'searching', intent, scope: 'episodes', blacklistSourceRelease: false } }))
    const trigger = page.getByRole('button', { name: intent === 'replace_bad' ? 'Mark bad & replace' : 'Replace quality', exact: true })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Review replacement' })
    await dialog.getByRole('checkbox', { name: /S01E01.*First selected episode/ }).check()
    await dialog.getByRole('button', { name: 'Prepare 1 file review', exact: true }).click()
    await expect(dialog.getByText('Reviewed all 1 files in the selected source scope.')).toBeVisible()
    await dialog.getByRole('radio', { name: 'No, exclude it only from this request' }).check()
    await dialog.getByRole('button', { name: intent === 'replace_bad' ? 'Confirm bad & replace' : 'Start quality replacement' }).click()
    const success = page.getByRole('dialog', { name: 'Replacement started' })
    await expect(success.getByText(/It continues after this dialog closes/)).toBeVisible()
    await inViewport(success.getByRole('heading'))
    expect(submitted).toMatchObject({ intent, scope: 'episodes', episodeIds: [101], blacklistSourceRelease: false, observedFiles: [{ id: 44, fileGeneration: 'file-generation-44', observationId: 'review-44' }] })
    await screenshot(page, intent)
    await success.getByRole('button', { name: 'Done' }).click()
    await expect(trigger).toBeFocused()
  })
}

test('blacklisted release override requires its explicit one-time confirmation', async ({ page }) => {
  await fileReviewFixture(page)
  await page.route('**/api/v1/library/9878/releases', route => route.fulfill({ json: [{ title: 'Reviewed.Source.1080p', indexer: 'Indexer', protocol: 'torrent', downloadUrl: 'magnet:blocked', size: 1024, quality: 'WEBDL-1080p', accepted: false, rejections: [{ code: 'blocklist', reason: 'previous failure' }], blacklistReason: 'previous failure', match: { matched: true }, candidateToken: 'blocked-candidate' }] }))
  let submitted: Record<string, unknown> | undefined
  await page.route('**/api/v1/grab', async route => { submitted = route.request().postDataJSON(); await route.fulfill({ status: 201, json: { id: 55 } }) })
  await page.getByRole('button', { name: 'Interactive search', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: /Grab anyway|Download anyway|Grab/, exact: true }).click()
  const confirm = page.getByRole('dialog', { name: 'Download blacklisted release anyway?' })
  await expect(confirm).toBeVisible()
  expect(submitted).toBeUndefined()
  await inViewport(confirm.getByRole('heading'))
  await confirm.getByRole('button', { name: 'Download anyway', exact: true }).click()
  await expect.poll(() => submitted).toMatchObject({ downloadAnyway: true, candidateToken: 'blocked-candidate', requestId: expect.any(String) })
  const completedSearch = page.getByRole('dialog', { name: /Interactive search/ })
  await inViewport(completedSearch.getByRole('status'))
})

test('native Runner torrent settings keep explicit protocol choice and unsupported capability feedback', async ({ page }) => {
  await page.route('**/api/v1/downloadclients', route => route.fulfill({ json: [] }))
  let tested: Record<string, unknown> | undefined
  await page.route('**/api/v1/downloadclients/test', async route => {
    tested = route.request().postDataJSON()
    await route.fulfill({ status: 400, json: { message: 'Runner native torrent acquisition is unsupported: native-torrent-deliveries-v1 is required.' } })
  })
  await page.goto('/settings#downloadclients')
  const settings = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Download clients', exact: true }) }).first()
  const type = settings.getByRole('combobox').first()
  await expect(type.locator('option[value="nzbd"]')).toHaveText('Runner · Usenet')
  await type.selectOption('runner-torrent')
  await settings.getByPlaceholder('Host (192.168.1.10)').fill('runner.fixture')
  await expect(settings.getByPlaceholder('Token, or password')).toBeVisible()
  await expect(settings.getByText(/separate managed copy/)).toBeVisible()
  await settings.getByRole('button', { name: 'Test', exact: true }).last().click()
  await expect(settings.getByText(/unsupported: native-torrent-deliveries-v1 is required/)).toBeVisible()
  expect(tested?.type).toBe('runner-torrent')
  expect(tested?.url).toBe('runner.fixture:6789')
  await settings.getByText(/unsupported: native-torrent-deliveries-v1 is required/).scrollIntoViewIfNeeded()
  await inViewport(settings.getByRole('button', { name: 'Add client', exact: true }))
  await screenshot(page, 'native-runner-torrent-settings')
})
