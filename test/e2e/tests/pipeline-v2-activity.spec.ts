import { expect, test } from '@playwright/test'
import { attempt, mockActivity } from '../activity-fixture'

test.use({ serviceWorkers: 'block' })

test('Activity preserves the distinct reason and prerequisite from the server', async ({ page }) => {
  await mockActivity(page, () => [{
    id: 951, state: 'held', category: 'attention', title: 'Held.Show.S01E01', mediaItemId: 9,
    reason: 'The download drive lacks sufficient space.',
    prerequisite: 'Free space on the Downloads drive, then retry.',
    actions: [{ kind: 'retry', label: 'Retry', enabled: false }, { kind: 'delete', label: 'Delete', enabled: true }],
  }, {
    id: 952, state: 'failed', category: 'attention', title: 'Missing.Folder.S01E02', mediaItemId: 12,
    reason: 'No library folder is assigned.', prerequisite: 'Assign a library folder, then retry.',
  }])
  await page.goto('/activity')
  await page.getByRole('button', { name: /^Needs attention / }).click()
  const held = attempt(page, 951)
  await expect(held).toContainText('The download drive lacks sufficient space.')
  await expect(held).toContainText('Free space on the Downloads drive, then retry.')
  await expect(held.getByRole('button', { name: 'Retry', exact: true })).toBeDisabled()
  await expect(held.getByRole('button', { name: 'Delete', exact: true })).toBeEnabled()
  await held.getByRole('checkbox').check()
  await expect(page.getByRole('button', { name: 'Delete selected' })).toBeEnabled()
  await expect(attempt(page, 952).getByRole('link', { name: 'Assign folder' })).toHaveAttribute('href', /\/library\/12\?assignFolder=true/)
})
