import { test, expect } from '@playwright/test'

test('series monitoring modes persist and keep manual selections on ordinary edits', async ({ page, request }) => {
  const response = await request.get('/api/v1/library?kind=series')
  const items = await response.json()
  let item = items.find((entry: { title: string }) => entry.title === 'The Test Show')
  if (!item) {
    // Standalone run: the shared fixture show is normally added by library.spec.ts.
    await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
    const added = await request.post('/api/v1/library', { data: { kind: 'series', tmdbId: 700, monitored: true } })
    expect([200, 201, 409]).toContain(added.status())
    const again = await (await request.get('/api/v1/library?kind=series')).json()
    item = again.find((entry: { title: string }) => entry.title === 'The Test Show')
  }
  expect(item).toBeTruthy()
  await page.goto(`/library/${item.id}`)
  await page.locator('.item-seasons > details > summary').click()
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  const picker = page.getByRole('combobox', { name: 'Episode monitoring' })
  await expect(picker.getByRole('option')).toHaveCount(7)
  await picker.selectOption('none')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByRole('checkbox', { name: 'Monitor Season 1', exact: true })).not.toBeChecked()
  await page.reload()
  await page.locator('.item-seasons > details > summary').click()
  await expect(page.getByText('Manual selection', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  await picker.selectOption('all')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByRole('checkbox', { name: 'Monitor Season 1', exact: true })).toBeChecked()
  const episode = page.getByRole('checkbox', { name: 'Monitor episode 1x1', exact: true })
  await Promise.all([
    page.waitForResponse((response) => response.url().includes(`/library/${item.id}/episodes/`) && response.request().method() === 'PATCH'),
    episode.click(),
  ])
  await expect(episode).not.toBeChecked()
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  await expect(picker).toHaveValue('')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.reload()
  await page.locator('.item-seasons > details > summary').click()
  await expect(episode).not.toBeChecked()
  // Leave the shared fixture as it was for later specs.
  await episode.click()
  await expect(episode).toBeChecked()
})
