import { expect, test } from '@playwright/test'
import { activityIdentity, attempt, mockActivity } from '../activity-fixture'

test.use({ serviceWorkers: 'block' })

test('a frozen cross-page delete survives route changes and reload with truthful partial results', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  let complete = false
  let newMatch = false
  let submissions = 0
  let action: Record<string, unknown> | undefined
  const targets = Array.from({ length: 51 }, (_, index) => activityIdentity(index + 1))
  const rows = (includeNewMatch: boolean) => [...targets, ...(includeNewMatch ? [activityIdentity(52)] : [])].map(target => ({
    id: target.id, title: `Interrupted job ${target.id}`, state: 'failed', category: 'attention',
    reason: 'Copy stopped before completion.',
  }))
  await mockActivity(page, () => rows(newMatch))
  await page.route('**/api/v1/activity/selections', async route => {
    expect(route.request().postDataJSON()).toEqual({ view: 'attention', scope: 'all', q: '' })
    newMatch = true
    await route.fulfill({ json: { id: 'attention-51', total: 51, retryable: 0, targets } })
  })
  const operation = () => ({
    id: action?.requestId, action: 'delete', state: complete ? 'completed' : 'running',
    total: 50, completed: complete ? 50 : 0, succeeded: complete ? 49 : 0, blocked: complete ? 1 : 0,
    targets: targets.filter(target => target.id !== 51).map(target => ({
      ...target, title: `Interrupted job ${target.id}`,
      state: !complete ? 'pending' : target.id === 2 ? 'blocked' : 'succeeded',
      message: target.id === 2 ? 'Reconnect the Media drive to finish deletion.' : '',
    })),
  })
  await page.route('**/api/v1/activity/actions', async route => {
    submissions++
    action = route.request().postDataJSON()
    await route.fulfill({ status: 202, json: operation() })
  })
  await page.route('**/api/v1/activity/operations/*', route => {
    return action && route.request().url().endsWith(String(action.requestId))
      ? route.fulfill({ json: operation() })
      : route.fulfill({ status: 404 })
  })

  await page.goto('/activity')
  await page.getByRole('button', { name: /^Needs attention / }).click()
  await page.getByRole('button', { name: 'Select all matching jobs' }).click()
  await expect(page.getByText('51 selected · 0 can retry')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry selected' })).toBeDisabled()
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(attempt(page, 51).getByRole('checkbox')).toBeChecked()
  await expect(attempt(page, 52).getByRole('checkbox')).not.toBeChecked()
  await attempt(page, 51).getByRole('checkbox').uncheck()

  await page.getByRole('button', { name: 'Delete selected' }).click()
  const confirmation = page.getByRole('dialog', { name: 'Delete 50 jobs and files?' })
  await expect(confirmation.getByRole('heading')).toBeInViewport({ ratio: 1 })
  expect(submissions).toBe(0)
  await confirmation.getByRole('button', { name: 'Delete jobs and files', exact: true }).click()
  const result = page.getByRole('dialog', { name: 'Delete jobs' })
  await expect(result).toContainText('0 of 50 jobs processed')
  expect(action).toMatchObject({ action: 'delete', selection: { id: 'attention-51', excludeIds: [51] } })
  expect(action).not.toHaveProperty('targets')

  await page.goto('/system#storage')
  await page.goto('/activity')
  await page.reload()
  const resume = page.getByRole('button', { name: 'Action results' })
  await resume.click()
  await expect(result).toContainText('0 of 50 jobs processed')
  complete = true
  await expect(result).toContainText('49 succeeded · 1 blocked')
  await expect(result).toContainText('Reconnect the Media drive to finish deletion.')
  await expect(result.getByRole('button', { name: 'Retry deletion' })).toBeVisible()
  expect(submissions).toBe(1)
})
