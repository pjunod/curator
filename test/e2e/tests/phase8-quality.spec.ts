import { expect, test } from '@playwright/test'

// Phase 6 "quality truth" through the UI: ADR 0013 (on-disk quality is
// measured) and ADR 0014 (a profile is a target). Runs after the acquisition
// specs so there is a real imported file on disk to describe.
test.describe.configure({ mode: 'serial' })

const PROFILE = 'E2E Scope'

test('quality profiles are editable, and the editor says what a profile will do', async ({
  page,
}) => {
  await page.goto('/settings')
  const panel = page.locator('#profiles')
  await expect(panel).toBeVisible()

  // The seeded default is named for what it does now. "Any" is retired: a
  // hunting profile with no opinion is not a thing (ADR 0014 §1).
  await expect(panel.getByRole('cell', { name: '1080p', exact: true })).toBeVisible()
  await expect(panel.locator('tbody')).not.toContainText('Any')
  await expect(panel.locator('tbody')).toContainText('hunts the best release up to WEB-DL 1080p')

  // A profile in use cannot be deleted out from under the items pointing at it.
  const inUseRow = panel.locator('tr', { hasText: '1080p' }).first()
  await expect(inUseRow.getByRole('button', { name: 'Delete' })).toBeDisabled()

  await panel.getByRole('button', { name: 'New profile' }).click()
  await panel.getByLabel('Profile name').fill(PROFILE)
  await panel.getByLabel('Target source').selectOption('bluray')
  await panel.getByLabel('Set a floor — below this, do not grab at all').check()

  // The sentence is the spec, and the editor shows it BEFORE you commit —
  // a picker whose consequence you only learn after saving is a guessing game.
  const preview = panel.getByTestId('profile-preview')
  await expect(preview).toContainText('hunts the best release up to Bluray 1080p, then stops')
  await expect(preview).toContainText('never below')

  await panel.getByRole('button', { name: 'Create' }).click()
  const row = panel.locator('tr', { hasText: PROFILE }).first()
  await expect(row).toBeVisible()
  await expect(row).toContainText('hunts the best release up to Bluray 1080p, then stops')
})

test('the item page says what its profile does, and where the quality came from', async ({
  page,
  request,
}) => {
  const movies = await (await request.get('/api/v1/library?kind=movie')).json()
  const movieId = movies[0].id

  // Assign the new profile through the item's own editor.
  await page.goto(`/library/${movieId}`)
  const openEditor = async () => {
    await page.getByRole('button', { name: 'Edit' }).first().click()
    return page
      .locator('section.panel')
      .filter({ has: page.getByRole('heading', { name: 'Edit' }) })
      .first()
  }
  let editor = await openEditor()
  await editor.getByLabel('Quality profile').selectOption({ label: PROFILE })
  await editor.getByRole('button', { name: 'Save' }).click()

  // The profile row is a sentence now. The old model needed an apology here
  // ("Any — upgrades until WEB-DL 1080p, then stops"); deleting that apology
  // was part of the fix, so its absence is worth asserting.
  const facts = page.locator('.facts, .fact-label').first()
  await expect(facts).toBeVisible()
  await expect(page.getByText('hunts the best release up to Bluray 1080p')).toBeVisible()
  await expect(page.locator('body')).not.toContainText('upgrades until')

  // The Files table says what each file is and how monarr knows. The imported
  // file's quality came from the release name (its payload is not a real
  // container), so the badge reads "from release" rather than "measured" —
  // which is exactly the distinction ADR 0013 exists to make visible.
  const filesPanel = page.locator('section.panel', { hasText: 'Files' }).last()
  await expect(filesPanel.getByRole('columnheader', { name: 'Quality' })).toBeVisible()
  await expect(filesPanel.getByRole('columnheader', { name: 'How we know' })).toBeVisible()
  await expect(filesPanel.locator('.prov-badge').first()).toBeVisible()

  // Put it back so later specs see the library they expect.
  editor = await openEditor()
  await editor.getByLabel('Quality profile').selectOption({ label: '1080p' })
  await editor.getByRole('button', { name: 'Save' }).click()
})

test('a profile nothing references can be deleted', async ({ page }) => {
  await page.goto('/settings')
  const panel = page.locator('#profiles')
  const row = panel.locator('tr', { hasText: PROFILE }).first()
  await expect(row).toBeVisible()
  await row.getByRole('button', { name: 'Delete' }).click()
  await expect(panel.locator('tbody')).not.toContainText(PROFILE)
})

test('a measured file reports its facts, not an apology', async ({ request }) => {
  // Every file monarr knows about now carries a provenance, even when the
  // answer is "we could not read it". The empty-quality-with-no-explanation
  // state is what ADR 0013 removed.
  const items = await (await request.get('/api/v1/library')).json()
  let seen = 0
  for (const it of items) {
    const full = await (await request.get(`/api/v1/library/${it.id}`)).json()
    for (const f of full.files ?? []) {
      seen++
      expect(f.provenanceLabel).toBeTruthy()
      expect(typeof f.verified).toBe('boolean')
    }
  }
  expect(seen).toBeGreaterThan(0)
})
