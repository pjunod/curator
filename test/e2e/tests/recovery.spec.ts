import { expect, test } from '@playwright/test'

test('recovery enablement saves with unmet advisory prerequisites', async ({ page, request }) => {
  await page.goto('/settings')
  await page.getByRole('navigation', { name: 'Settings sections' }).getByRole('button', { name: 'Dev', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Enable Runner recovery' })).toBeVisible()
  await page.getByLabel('Curator recovery mount').fill('/missing/e2e-recovery')
  await page.getByLabel('Enable automatic recovery status refresh').check()
  await page.getByRole('button', { name: 'Save recovery settings' }).click()
  await expect.poll(async () => (await (await request.get('/api/v1/import/recovery/settings')).json()).settings.enabled).toBe(true)
  await expect(page.getByText('local mount available: unmet')).toBeVisible()
  await page.getByLabel('Enable automatic recovery status refresh').uncheck()
  await page.getByRole('button', { name: 'Save recovery settings' }).click()
  await page.getByRole('navigation', { name: 'Settings sections' }).getByRole('button', { name: 'Recovery', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Runner recovery imports' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Preview import' })).toBeDisabled()
})
