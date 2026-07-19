import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// The explicit, controllable handoff (runs after phase2/phase3 have grabbed
// and imported real downloads through the fake qBittorrent, so the queue has
// rows with a full handoff trace).
test.describe.configure({ mode: 'serial' })

const MOVIE_RELEASE = 'The.Test.Movie.2024.1080p.WEB-DL.x264-E2E'

test('an imported download lays out its handoff step by step', async ({ page }) => {
  await page.goto('/activity')
  const row = page.locator('tr', { hasText: MOVIE_RELEASE }).first()
  await expect(row).toBeVisible()

  await row.getByRole('button', { name: 'Show handoff' }).click()

  const trace = page.locator('.handoff-trace').first()
  await expect(trace).toBeVisible()
  // The trace opens at grab and ends at import.
  await expect(page.locator('.handoff-step[data-step="grabbed"]').first()).toBeVisible()
  await expect(page.locator('.handoff-step[data-step="imported"]').first()).toBeVisible()
  // The paths Monarr used are shown (no more black box).
  await expect(page.locator('.handoff .path-chip').first()).toBeVisible()
})

test('the approve-imports toggle on a client persists', async ({ page, request }) => {
  await page.goto('/settings')
  const clientRow = page.locator('#downloadclients tr', { hasText: 'fakeqb' }).first()
  await expect(clientRow).toBeVisible()

  const approvalOf = async () => {
    const clients = await (await request.get('/api/v1/downloadclients')).json()
    return clients.find((c: any) => c.name === 'fakeqb')?.manualApproval
  }

  // Click (not check()) — the checkbox re-renders from the server round-trip,
  // so assert the persisted value rather than the element's transient state.
  await clientRow.getByRole('checkbox').click()
  await expect.poll(approvalOf).toBe(true)

  // Restore auto-import so later runs aren't affected.
  await clientRow.getByRole('checkbox').click()
  await expect.poll(approvalOf).toBe(false)
})

test('manual import scans a folder and skips samples', async ({ page }) => {
  const dir = mkdtempSync(join(process.env.E2E_MEDIA_ROOT!, 'manual-'))
  writeFileSync(join(dir, 'Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv'), 'x')
  writeFileSync(join(dir, 'sample.mkv'), 's')

  await page.goto('/activity')
  await page.getByRole('button', { name: 'Manual import' }).click()

  const panel = page.locator('.manual-import')
  await expect(panel).toBeVisible()
  await panel.getByLabel('Path').fill(dir)
  await panel.getByRole('button', { name: 'Scan' }).click()

  await expect(panel.getByText('Some.Movie.2021.1080p.WEB-DL.x264-GRP.mkv')).toBeVisible()
  await expect(panel.getByText('sample.mkv')).toHaveCount(0)
  // The target-title picker is populated from the library.
  const picker = panel.getByRole('combobox').first()
  await expect(picker.locator('option', { hasText: 'The Test Movie' })).toHaveCount(1)
})
