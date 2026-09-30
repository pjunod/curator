import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

for (const width of [390, 1280]) {
  test(`deep episode search and edit open in the viewport at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    await page.route('**/api/v1/library/9875', route => route.fulfill({ json: {
      id: 9875, kind: 'series', title: 'Long Series', monitored: true,
      path: '', genres: [], ratings: [], ids: {}, files: [], copies: [],
      downloadPriority: 0, downloadPriorityOverride: null,
      seasons: [{ number: 1, monitored: true, episodes: Array.from({ length: 80 }, (_, i) => ({
        id: i + 1, seasonNumber: 1, episodeNumber: i + 1, title: `Episode ${i + 1}`, monitored: true,
      })) }],
    } }))
    let finishSearch: () => void = () => {}
    const pending = new Promise<void>(resolve => { finishSearch = resolve })
    await page.route('**/api/v1/library/9875/releases?*', async route => {
      await pending
      await route.fulfill({ json: [] })
    })
    await page.goto('/library/9875')
    const trigger = page.getByRole('row').filter({ hasText: 'Episode 80' }).getByRole('button', { name: 'Search', exact: true })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Interactive search — S01E80' })
    await expect(dialog.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await expect(dialog.getByRole('heading')).toBeFocused()
    await expect(dialog.getByText('Searching indexers…')).toBeInViewport({ ratio: 1 })
    finishSearch()
    await expect(dialog.getByText('No releases found on any enabled indexer.')).toBeInViewport({ ratio: 1 })
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
    await expect(trigger).toBeInViewport({ ratio: 1 })

    // Another target, then reopening the same target, must each reveal its work.
    const pack = page.getByRole('button', { name: 'Search pack', exact: true })
    for (let i = 0; i < 2; i++) {
      await pack.click()
      const packDialog = page.getByRole('dialog', { name: 'Interactive search — Season 1 pack' })
      await expect(packDialog.getByRole('heading')).toBeInViewport({ ratio: 1 })
      await packDialog.getByRole('button', { name: 'Close', exact: true }).click()
      await expect(pack).toBeFocused()
    }

    const edit = page.getByRole('button', { name: 'Edit', exact: true })
    await edit.click()
    const editor = page.getByRole('dialog', { name: 'Edit', exact: true })
    await expect(editor.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await expect(editor.getByRole('heading')).toBeFocused()
    await editor.getByRole('button', { name: 'Save', exact: true }).scrollIntoViewIfNeeded()
    await expect(editor.getByRole('button', { name: 'Close', exact: true })).toBeInViewport({ ratio: 1 })
    await page.keyboard.press('Escape')
    await expect(edit).toBeFocused()
  })

  test(`profile forms open in the viewport at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    await page.goto('/settings')
    const panel = page.locator('#profiles')
    const edit = panel.getByRole('button', { name: 'Edit', exact: true }).first()
    await edit.click()
    const editor = page.getByRole('dialog', { name: 'Edit profile', exact: true })
    await expect(editor.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await expect(editor.getByRole('heading')).toBeFocused()
    await page.keyboard.press('Escape')
    await expect(edit).toBeFocused()
    const create = panel.getByRole('button', { name: 'New profile', exact: true })
    await create.click()
    const creator = page.getByRole('dialog', { name: 'New profile', exact: true })
    await expect(creator.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await expect(creator.getByRole('heading')).toBeFocused()
    await creator.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(create).toBeFocused()
    await expect(create).toBeInViewport({ ratio: 1 })
  })

  test(`search errors and deep-row grab feedback stay visible at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    await page.route('**/api/v1/library/9874', route => route.fulfill({ json: {
      id: 9874, kind: 'movie', title: 'Feedback', monitored: true,
      path: '', genres: [], ratings: [], ids: {}, files: [], copies: [], seasons: [],
    } }))
    await page.route('**/api/v1/library/9874/releases', route => route.fulfill({ status: 500, json: { message: 'Indexer unavailable' } }))
    await page.goto('/library/9874')
    await page.getByRole('button', { name: 'Interactive search', exact: true }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog.getByRole('status')).toContainText('Indexer unavailable')
    await expect(dialog.getByRole('status')).toBeInViewport({ ratio: 1 })
    await page.keyboard.press('Escape')

    await page.route('**/api/v1/library/9874/releases', route => route.fulfill({ json: Array.from({ length: 40 }, (_, i) => ({
      title: `Release.${i}.1080p.WEB-DL`, indexer: 'Test', protocol: 'torrent',
      size: 1024, quality: 'WEBDL-1080p', accepted: true, rejections: [],
      match: { matched: false }, candidateToken: `token-${i}`,
    })) }))
    await page.getByRole('button', { name: 'Interactive search', exact: true }).click()
    const grab = dialog.getByRole('button', { name: 'Grab', exact: true }).last()
    await page.route('**/api/v1/grab', route => route.fulfill({ status: 500, json: { message: 'Client unavailable' } }))
    await grab.click()
    await expect(dialog.getByRole('status')).toContainText('Client unavailable')
    await expect(dialog.getByRole('status')).toBeInViewport({ ratio: 1 })
    await page.route('**/api/v1/grab', route => route.fulfill({ json: { id: 1 } }))
    await grab.click()
    await expect(dialog.getByRole('status')).toContainText('Grabbed')
    await expect(dialog.getByRole('status')).toBeInViewport({ ratio: 1 })
    await expect(dialog.getByRole('button', { name: 'Close', exact: true })).toBeInViewport({ ratio: 1 })
  })
}
