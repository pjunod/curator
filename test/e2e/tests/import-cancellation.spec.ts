import { expect, test } from '@playwright/test'
import { activityIdentity, attempt, mockActivity } from '../activity-fixture'

test.use({ serviceWorkers: 'block' })

for (const width of [390, 1280]) {
  test(`delete waits for confirmation and checks the selected attempt at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 820 })
    let generationKey = 'original'
    let request: Record<string, unknown> | undefined
    const identity = activityIdentity(71, generationKey, '1')
    await mockActivity(page, () => Array.from({ length: 21 }, (_, index) => ({
      id: 51 + index,
      title: `Interrupted job ${51 + index}`, mediaItemId: 1, state: 'failed',
      protocol: 'usenet', category: 'attention', progress: 1,
      importControl: { generationKey, epoch: '1' },
      reason: 'Copy stopped before completion.',
    })))
    const operation = () => ({
      id: request?.requestId, action: 'delete', state: 'completed', total: 1, completed: 1, succeeded: 0, blocked: 1,
      targets: [{ ...identity, state: 'blocked', title: 'Interrupted job 71', message: 'This attempt changed. Refresh and select it again.' }],
    })
    await page.route('**/api/v1/activity/actions', async route => {
      request = route.request().postDataJSON()
      await route.fulfill({ status: 202, json: operation() })
    })
    await page.route('**/api/v1/activity/operations/*', route => request ? route.fulfill({ json: operation() }) : route.fulfill({ status: 404 }))
    await page.goto('/activity')
    await page.getByRole('button', { name: /^Needs attention / }).click()
    const trigger = attempt(page, 71).getByRole('button', { name: 'Delete', exact: true })
    await trigger.click()
    const confirmation = page.getByRole('dialog', { name: 'Delete 1 job and files?' })
    await expect(confirmation.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await expect(confirmation.getByRole('heading')).toBeFocused()
    await expect(confirmation).toContainText('job-owned downloads and temporary files')
    await expect(confirmation).toContainText('Media already installed in your library')
    expect(request).toBeUndefined()
    await page.keyboard.press('Escape')
    await expect(trigger).toBeFocused()
    await trigger.click()
    generationKey = 'replacement'
    await confirmation.getByRole('button', { name: 'Delete jobs and files', exact: true }).click()
    const result = page.getByRole('dialog', { name: 'Delete jobs' })
    await expect(result).toContainText('This attempt changed. Refresh and select it again.')
    expect(request).toMatchObject({ action: 'delete', targets: [identity] })
    await expect(result.getByRole('heading')).toBeInViewport({ ratio: 1 })
  })
}
