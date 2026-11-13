import { mockActivity, openAttempt } from '../activity-fixture'
import { expect, test, type Page } from '@playwright/test'

// Pipeline contract v2 in Activity (IMPL_CURATOR section 5.9): a held row
// shows the hold text and Runner's deadline, a provisional failure says so,
// and an unresolved publication file offers Place / Discard / Keep with the
// result shown inline, on screen. The queue is mocked: these rows need a
// Runner that holds jobs and publishes generations, which the fake clients
// in this suite do not.

const now = () => new Date().toISOString()
const inTwoHours = () => new Date(Date.now() + 2 * 3600_000).toISOString()

const held = {
  id: 951, mediaItemId: 9, clientId: 3, title: 'Held.Show.S01E01.1080p.WEB-DL-GRP', state: 'held', progress: 0.42,
  protocol: 'usenet', quality: 'WEBDL-1080p', addedAt: now(), heldReason: 'waiting for disk', stale: false,
  deadlineAt: inTwoHours(),
}
const stale = {
  id: 952, mediaItemId: 9, clientId: 3, title: 'Stale.Show.S01E02.1080p.WEB-DL-GRP', state: 'held', progress: 0.1,
  protocol: 'usenet', quality: 'WEBDL-1080p', addedAt: now(), heldReason: 'held (stale): Runner has not reported since', stale: true,
  deadlineAt: new Date(Date.now() - 60_000).toISOString(),
}
const provisional = {
  id: 953, mediaItemId: 9, clientId: 3, title: 'Lost.Show.S01E03.1080p.WEB-DL-GRP', state: 'failed', progress: 0.5,
  protocol: 'usenet', quality: 'WEBDL-1080p', addedAt: now(), error: 'Runner no longer lists this transfer',
  runnerOutcome: 'failed', runnerReason: 'lost', provisional: true, blamesRelease: false,
}
const withPublication = {
  id: 954, mediaItemId: 9, clientId: 3, title: 'Published.Show.S01.1080p.WEB-DL-PACK', state: 'importing', progress: 1,
  protocol: 'usenet', quality: 'WEBDL-1080p', addedAt: now(), runnerOutcome: 'completed', runnerReason: '', provisional: false,
  publications: [{
    id: 77, kind: 'completed', state: 'import_failed', cohort: 'live', path: '/completed/Published.Show.S01.job-9.runner-ab12',
    generation: 'ab12', manifestSource: 'file', error: 'a file is unresolved',
    files: [
      { path: 'Published.Show.S01E01.mkv', outcome: 'placed', bytes: 10, libraryPath: '/library/Published Show/Season 01/E01.mkv' },
      { path: 'Published.Show.S01E01.nfo', outcome: 'unresolved', bytes: 1, reason: 'a sidecar with no parent' },
    ],
  }],
}

async function mockQueue(page: Page, resolveCalls: { path: string; action: string }[]) {
  await mockActivity(page,()=>[held,stale,provisional,withPublication])
  await page.route('**/api/v1/downloads/deliveries?*', route => route.fulfill({ json: { items: [], nextBeforeId: 0 } }))
  await page.route('**/api/v1/queue/954/publications/77/files/resolve', async route => {
    resolveCalls.push(route.request().postDataJSON())
    await route.fulfill({ status: 204, body: '' })
  })
}

async function expectOnScreen(page: Page, locator: ReturnType<Page['locator']>) {
  await expect(locator).toBeVisible()
  const box = await locator.boundingBox()
  const viewport = page.viewportSize()
  expect(box, 'element has a layout box').not.toBeNull()
  expect(viewport).not.toBeNull()
  if (box && viewport) {
    expect(box.y).toBeGreaterThanOrEqual(0)
    expect(box.y + box.height).toBeLessThanOrEqual(viewport.height)
    expect(box.x).toBeGreaterThanOrEqual(0)
    expect(box.x + box.width).toBeLessThanOrEqual(viewport.width + 1)
  }
}

for (const viewport of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }]) {
  test.describe(`pipeline v2 Activity (${viewport.name})`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height } })

    test('held rows show the hold and its deadline, stale holds say so, provisional failures say so', async ({ page }) => {
      await mockQueue(page, [])
      await page.goto('/activity')
      await openAttempt(page,held.title)
      await expect(page.getByRole('dialog').getByText(/Held: waiting for disk · Runner ends it in (1h|2h)/)).toBeVisible()
      await page.keyboard.press('Escape')
      await openAttempt(page,stale.title)
      await expect(page.getByRole('dialog').getByText(/Held \(stale\)/)).toBeVisible()
      await page.keyboard.press('Escape')

      await page.getByRole('button',{name:/^History /}).click()
      await openAttempt(page,provisional.title)
      await expect(page.getByText(/Provisional: lost — re-opens if Runner reports this transfer/)).toBeVisible()
    })

    test('an unresolved publication file offers Place, Discard and Keep, and shows the result on screen', async ({ page }) => {
      const calls: { path: string; action: string }[] = []
      await mockQueue(page, calls)
      await page.goto('/activity')
      await openAttempt(page,withPublication.title)

      const file = page.locator('.publication-files li[data-file="Published.Show.S01E01.nfo"]')
      await expect(file).toBeVisible()
      for (const name of ['Place', 'Discard', 'Keep']) {
        await expect(file.getByRole('button', { name })).toBeVisible()
      }
      // A placed file offers nothing to decide.
      await expect(page.locator('.publication-files li[data-file="Published.Show.S01E01.mkv"]').getByRole('button')).toHaveCount(0)

      const discard = file.getByRole('button', { name: 'Discard' })
      await discard.scrollIntoViewIfNeeded()
      await discard.click()
      const result = file.getByRole('status')
      await expect(result).toHaveText(/Marked for cleanup/)
      await expectOnScreen(page, result)
      // The decided file's buttons go with its outcome: focus moves to the
      // result, never left on a removed control (CLAUDE.md section 5).
      await expect(result).toBeFocused()
      expect(calls).toEqual([{ path: 'Published.Show.S01E01.nfo', action: 'discard' }])
    })
  })
}
