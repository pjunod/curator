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

test('Choose files imports only an unresolved file of the selected job with an explicit episode mapping', async ({ page }) => {
  const publicationPath = '/completed/exact-job'
  await mockActivity(page, () => [{
    category: 'attention', title: 'Scoped show', kind: 'series',
    download: { id: 71, state: 'failed', mediaItemId: 23, title: 'Scoped release', publications: [{
      id: 419, path: publicationPath, state: 'import_failed',
      files: [
        { path: 'eligible.mkv', bytes: 450, outcome: 'unresolved', reason: 'not selected for import; place or discard it in Resolve' },
        { path: 'already-added.mkv', outcome: 'placed' },
        { path: 'held.mkv', outcome: 'unresolved', reason: 'kept by an operator in Resolve' },
      ],
    }] },
    actions: [{ kind: 'choose_files', label: 'Choose files', enabled: true }],
  }])
  await page.route('**/api/v1/library/23', route => route.fulfill({ json: {
    id: 23, title: 'Scoped show', kind: 'series', copies: [],
    seasons: [{ number: 1, episodes: [{ episodeNumber: 2, title: 'Second episode' }] }],
  } }))
  await page.route('**/api/v1/import/plan', route => route.fulfill({ json: [
    { path: `${publicationPath}/eligible.mkv`, destination: '/library/Scoped show/S01E02.mkv', mode: 'runner' },
  ] }))
  let submitted: unknown
  await page.route('**/api/v1/queue/71/publications/419/files/place', route => {
    submitted = route.request().postDataJSON()
    return route.fulfill({ status: 204 })
  })
  await page.goto('/activity#attention')
  await attempt(page, 71).getByRole('button', { name: 'Choose files' }).click()
  const chooser = page.getByRole('dialog', { name: 'Import into Scoped show' })
  await expect(chooser.getByRole('heading')).toBeInViewport({ ratio: 1 })
  await expect(chooser.getByRole('textbox', { name: 'Folder path' })).toHaveCount(0)
  await expect(chooser.getByRole('checkbox')).toHaveCount(1)
  await chooser.getByRole('checkbox', { name: /eligible\.mkv/ }).check()
  await chooser.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(chooser.getByRole('button', { name: 'Import 1 file', exact: true })).toBeDisabled()
  await chooser.getByRole('combobox', { name: 'Episode for eligible.mkv' }).selectOption('1:2')
  await chooser.getByRole('button', { name: 'Import 1 file', exact: true }).click()
  await expect.poll(() => submitted).toEqual({ mediaItemId: 23, files: [{ path: 'eligible.mkv', season: 1, episodes: [2] }] })
})
