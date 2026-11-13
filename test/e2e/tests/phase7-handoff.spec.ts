import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { mockActivity, openAttempt } from '../activity-fixture'
import { expect, test } from '@playwright/test'

// The explicit, controllable handoff (runs after phase2/phase3 have grabbed
// and imported real downloads through the fake qBittorrent, so the queue has
// rows with a full handoff trace).
test.describe.configure({ mode: 'serial' })

const MOVIE_RELEASE = 'The.Test.Movie.2024.1080p.WEB-DL.x264-E2E'

test('an imported download lays out its handoff step by step', async ({ page }) => {
  await page.goto('/activity')
  await page.getByRole('button', { name: /^History / }).click()
  await openAttempt(page, MOVIE_RELEASE)

  const trace = page.locator('.handoff-trace').first()
  await expect(trace).toBeVisible()
  // The trace opens at grab and ends at import.
  await expect(page.locator('.handoff-step[data-step="grabbed"]').first()).toBeVisible()
  await expect(page.locator('.handoff-step[data-step="imported"]').first()).toBeVisible()
  // The paths Monarr used are shown (no more black box).
  await expect(page.locator('.handoff .path-chip').first()).toBeVisible()
})

test('the approve-imports toggle on a client persists', async ({ page, request }) => {
  await page.goto('/settings#downloadclients')
  const clientRow = page.locator('#downloadclients tr', { hasText: 'fakeqb' }).first()
  await expect(clientRow).toBeVisible()

  const approvalOf = async () => {
    const clients = await (await request.get('/api/v1/downloadclients')).json()
    return clients.find((c: any) => c.name === 'fakeqb')?.manualApproval
  }

  // Click (not check()) — the checkbox re-renders from the server round-trip,
  // so assert the persisted value rather than the element's transient state.
  // By name, not position: the row carries more than one checkbox now
  // (approve-imports and clean-up-after-import), and a bare checkbox lookup
  // is a strict-mode violation the moment a second one appears.
  await clientRow.getByLabel('Approve imports').click()
  await expect.poll(approvalOf).toBe(true)

  // Restore auto-import so later runs aren't affected.
  await clientRow.getByLabel('Approve imports').click()
  await expect.poll(approvalOf).toBe(false)
})

test('manual import scans a folder and skips samples', async ({ page, request }) => {
  const dir = mkdtempSync(join(process.env.E2E_MEDIA_ROOT!, 'manual-'))
  writeFileSync(join(dir, 'Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv'), 'x')
  writeFileSync(join(dir, 'Another.Movie.2022.720p.WEB-DL.x264-GRP.mkv'), 'x')
  writeFileSync(join(dir, 'sample.mkv'), 's')

  const learnedDefault = await (await request.get('/api/v1/import/default-path')).json()
  await page.goto('/activity')
  let submitted: any
  await page.route('**/api/v1/import/manual', async (route) => {
    submitted = route.request().postDataJSON()
    await route.fulfill({ status: 202, contentType: 'application/json', body: '{"downloadId":991}' })
  })
  await page.goto('/system#storage')
  await page.getByRole('button', { name: 'Import files…', exact: true }).click()

  const panel = page.locator('.manual-import')
  await expect(panel).toBeVisible()
  await expect(panel.getByLabel('Path')).toHaveValue(learnedDefault.path)
  await panel.getByLabel('Path').click()
  await expect(panel.getByRole('button', { name: /Filesystem root/ })).toBeVisible()
  await panel.getByLabel('Path').fill(dir)
  await panel.getByRole('button', { name: 'Scan' }).click()

  await expect(panel.getByText('Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv')).toBeVisible()
  await expect(panel.getByText('Another.Movie.2022.720p.WEB-DL.x264-GRP.mkv')).toBeVisible()
  await expect(panel.getByText('sample.mkv')).toHaveCount(0)
  // Scan results are actionable rows now. A multi-file folder starts with
  // nothing selected so one title cannot accidentally absorb its neighbour.
  await expect(panel.getByRole('button', { name: 'Queue selected (0)' })).toBeDisabled()
  await panel.getByLabel('Select Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv').check()
  // The target-title picker is populated from the library.
  const picker = panel.getByRole('combobox').first()
  await expect(picker.locator('option', { hasText: 'The Test Movie' })).toHaveCount(1)
  await picker.selectOption({ label: 'The Test Movie (2024)' })
  const queue = panel.getByRole('button', { name: 'Queue selected (1)' })
  await expect(queue).toBeEnabled()
  await queue.click()
  await expect(panel).toBeHidden()
  expect(submitted.paths).toEqual([join(dir, 'Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv')])
})

test('an accepted import waiting for a worker is shown as queued, not interrupted', async ({ page }) => {
  await mockActivity(page, () => [{ id:991,mediaItemId:1,title:'Waiting.Release.2026.1080p.WEB-DL',state:'downloaded',importState:'queued',progress:1,protocol:'usenet',quality:'1080p',addedAt:new Date().toISOString(),importControl:{generationKey:'g',epoch:'1',outcome:'',pendingPublications:0,retainedPublications:0,heldStages:0} }])
  await page.goto('/activity')
  await expect(page.locator('[data-activity-id="991"]')).toContainText('Waiting for an import worker')
  const dialog = await openAttempt(page,'Waiting.Release.2026')
  await expect(dialog.getByRole('button',{name:'Cancel import',exact:true})).toBeVisible()
  await expect(dialog.getByRole('button',{name:'Restart import',exact:true})).toHaveCount(0)
})
