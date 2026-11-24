import { expect, test } from '@playwright/test'
import { activityIdentity, attempt, mockActivity } from '../activity-fixture'

test.use({ serviceWorkers: 'block' })

const item = (id: number, category = 'attention') => ({
  id, category, title: `Import title ${id}`, mediaItemId: 88, state: category === 'history' ? 'imported' : 'failed',
  progress: 1, quality: '1080p', protocol: 'manual',
})

for (const width of [1280, 390]) {
  test(`one Activity view retains history, bulk retry results and old import links at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 850 })
    const rows = [
      { ...item(1, 'progress'), state: 'importing', importState: 'running', stage: 'importing', stageDetail: 'Verifying source identity' },
      { ...item(3), actions: [{ kind: 'retry', label: 'Retry', enabled: true }, { kind: 'delete', label: 'Delete', enabled: true }] },
      { ...item(4), actions: [{ kind: 'approve', label: 'Approve import', enabled: true }, { kind: 'delete', label: 'Delete', enabled: true }] },
      ...Array.from({ length: 53 }, (_, i) => item(i + 10, 'history')),
    ]
    await mockActivity(page, () => rows)
    let selectionRequest: unknown
    let actionRequest: Record<string, unknown> | undefined
    await page.route('**/api/v1/activity/selections', async route => {
      selectionRequest = route.request().postDataJSON()
      await route.fulfill({ json: { id: 'matching-imports', total: 2, retryable: 1, targets: [activityIdentity(3), activityIdentity(4)] } })
    })
    const operation = () => ({
      id: actionRequest?.requestId, action: 'retry', state: 'completed', total: 2, completed: 2, succeeded: 1, blocked: 1,
      targets: [
        { ...activityIdentity(3), title: 'Import title 3', state: 'succeeded' },
        { ...activityIdentity(4), title: 'Import title 4', state: 'blocked', message: 'Approve this import to continue.' },
      ],
    })
    await page.route('**/api/v1/activity/actions', async route => {
      actionRequest = route.request().postDataJSON()
      await route.fulfill({ status: 202, json: operation() })
    })
    await page.route('**/api/v1/activity/operations/*', route => route.fulfill({ json: operation() }))

    await page.goto('/imports')
    await expect(page).toHaveURL(/activity\?scope=imports/)
    await expect(page.getByRole('combobox', { name: 'Activity scope' })).toHaveValue('imports')
    await expect(attempt(page, 1).getByText('Verifying source identity')).toBeVisible()
    await expect(attempt(page, 1).locator('.progress-track')).toHaveCount(0)
    await page.getByRole('button', { name: /^History 53/ }).click()
    await expect(page.getByText('1–50 of 53')).toBeVisible()
    await page.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(page.getByText('51–53 of 53')).toBeVisible()
    await page.getByRole('button', { name: /^Needs attention 2/ }).click()
    await page.getByRole('button', { name: 'Select all matching jobs' }).click()
    await expect(page.getByText('2 selected · 1 can retry')).toBeVisible()
    await expect(attempt(page, 4).getByRole('checkbox')).toBeChecked()
    await page.getByRole('button', { name: 'Retry selected' }).click()
    const result = page.getByRole('dialog', { name: 'Retry jobs' })
    await expect(result).toContainText('1 succeeded · 1 blocked')
    await expect(result).toContainText('Approve this import to continue.')
    await expect(result.getByRole('heading')).toBeInViewport({ ratio: 1 })
    expect(selectionRequest).toEqual({ scope: 'imports', view: 'attention', q: '' })
    expect(actionRequest).toMatchObject({ action: 'retry', selection: { id: 'matching-imports', excludeIds: [] } })
    await page.keyboard.press('Escape')
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeFocused()
    await page.getByRole('searchbox', { name: 'Find a title or release' }).fill('no-such-title')
    await expect(page.getByRole('heading', { name: 'No activity matches this search' })).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
  })

  test(`unavailable activity leaves the counts and empty state unknown at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 850 })
    await page.route('**/api/v1/activity?*', route => route.fulfill({ status: 503, json: { message: 'Local status unavailable' } }))
    await page.goto('/activity')
    await expect(page.getByRole('alert')).toContainText('Activity and counts are unavailable')
    await expect(page.getByText('Nothing in progress', { exact: true })).toHaveCount(0)
    await expect(page.getByRole('heading', { name: 'Download folders', exact: true })).toHaveCount(0)
  })
}

test('history shows the outcome and a log correlated to that job', async ({ page }) => {
  await mockActivity(page, () => [
    { ...item(501, 'history'), state: 'failed', reason: 'Verification failed. Job files removed.' },
    { ...item(502, 'history'), reason: 'Added to your library.' },
    { ...item(-88, 'history'), mediaItemId: 0, title: 'Older publication' },
  ])
  await page.route('**/api/v1/activity/jobs/501/log', route => route.fulfill({ json: {
    title: 'Import title 501', summary: 'Verification failed. Job files removed.',
    events: [{ at: '2026-11-09T12:00:00Z', step: 'Verification failed', message: 'Two download blocks were missing.' }],
  } }))
  await page.route('**/api/v1/activity/jobs/502/log', route => route.fulfill({ json: {
    title: 'Import title 502', summary: 'Added to your library.', events: [],
  } }))
  await page.route('**/api/v1/activity/jobs/-88/log', route => route.fulfill({ json: {
    title: 'Older publication', summary: 'Added to your library.',
    events: [{ at: '2026-10-10T12:00:00Z', step: 'Import completed', message: 'A Library file was recorded.' }],
  } }))
  await page.goto('/activity#history')
  await expect(attempt(page, 501)).toContainText('Verification failed. Job files removed.')
  await expect(attempt(page, 501).getByRole('checkbox')).toHaveCount(0)
  await attempt(page, 501).getByRole('button', { name: 'Job log' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('Two download blocks were missing.')
  await page.keyboard.press('Escape')
  await attempt(page, 502).getByRole('button', { name: 'Job log' }).click()
  await expect(dialog).toContainText('No detailed events remain for this job.')
  await expect(dialog).not.toContainText('Two download blocks')
  await page.keyboard.press('Escape')
  await attempt(page, -88).getByRole('button', { name: 'Job log' }).click()
  await expect(dialog).toContainText('A Library file was recorded.')
})
