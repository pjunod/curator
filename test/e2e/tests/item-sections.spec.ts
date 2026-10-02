import { expect, test, type Page } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

async function fixture(page: Page, count: number, kind = 'series') {
  let files = Array.from({ length: count }, (_, i) => ({
    id: i + 1, path: `/library/Series/episode-${String(i + 1).padStart(3, '0')}.mkv`,
    size: 1024, episodeIds: [i + 1], quality: 'WEB-DL 1080p',
    provenance: 'probe', provenanceLabel: 'measured',
  }))
  const item = () => ({
    id: 9871, kind, title: 'Long Series', monitored: true,
    path: '', genres: [], ratings: [], ids: {}, files, copies: [],
    downloadPriority: 0, downloadPriorityOverride: null,
    seasons: kind === 'series' ? [1, 2].map(number => ({
      number, monitored: true, episodes: [{
        id: number, seasonNumber: number, episodeNumber: 1,
        title: `Episode ${number}`, monitored: true,
      }],
    })) : [],
  })
  await page.route('**/api/v1/library/9871', route => route.fulfill({ json: item() }))
  await page.route('**/api/v1/library/9871/files/*', async route => {
    const id = Number(new URL(route.request().url()).pathname.split('/').pop())
    files = files.filter(file => file.id !== id)
    await route.fulfill({ json: { deletedFromDisk: true } })
  })
  await page.goto('/library/9871')
}

for (const width of [390, 1280]) {
  test(`item sections collapse and files paginate at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 })
    await fixture(page, 51)
    const seasons = page.locator('.item-seasons > details')
    const files = page.locator('.item-files > details')
    const seasonSummary = seasons.locator(':scope > summary')
    const fileSummary = files.locator(':scope > summary')
    await expect(seasonSummary).toHaveText('Seasons (2)')
    await expect(fileSummary).toHaveText('Files (51)')
    await expect(seasons).toHaveJSProperty('open', false)
    await expect(files).toHaveJSProperty('open', false)
    await expect(seasons.getByRole('button', { name: 'Search pack' }).first()).toBeHidden()
    await seasonSummary.focus()
    await seasonSummary.press('Enter')
    await expect(seasons.getByLabel('Monitor Season 1')).toBeVisible()
    await expect(seasons.getByLabel('Monitor episode 1x1')).toBeVisible()
    await expect(seasons.getByRole('button', { name: /Season 2/ })).toHaveAttribute('aria-expanded', 'false')
    await seasonSummary.press('Space')
    await expect(seasons).toHaveJSProperty('open', false)

    await fileSummary.focus()
    await fileSummary.press('Enter')
    const rows = files.locator('tbody tr')
    const controls = files.locator('[aria-label="File pagination"]')
    await expect(rows).toHaveCount(25)
    await expect(rows.first()).toContainText('episode-001.mkv')
    await expect(controls).toContainText('1–25 of 51')
    await expect(controls.getByRole('button', { name: '‹ Prev' })).toBeDisabled()
    // Use the bottom pager: the new page must be revealed above, with focus.
    await files.getByRole('button', { name: 'Next ›' }).last().click()
    await expect(rows.first()).toContainText('episode-026.mkv')
    await expect(controls).toContainText('26–50 of 51')
    await expect(controls).toBeFocused()
    await expect(controls).toBeInViewport({ ratio: 1 })
    await expect(rows.first().locator('td').first()).toBeInViewport({ ratio: 1 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1)

    await fileSummary.click()
    await expect(files).toHaveJSProperty('open', false)
    await fileSummary.click()
    await expect(controls).toContainText('Page 2 of 3')
    await controls.getByTitle('Last page', { exact: true }).click()
    await expect(rows).toHaveCount(1)
    await expect(rows.first()).toContainText('episode-051.mkv')
    await expect(controls.getByRole('button', { name: 'Next ›' })).toBeDisabled()
    // Removing the only final-page row must leave a populated preceding page.
    await rows.first().getByRole('button', { name: 'Delete', exact: true }).click()
    await rows.first().getByRole('button', { name: 'Delete', exact: true }).click()
    await expect(fileSummary).toHaveText('Files (50)')
    await expect(rows).toHaveCount(25)
    await expect(controls).toContainText('Page 2 of 2')
    await expect(files).toHaveJSProperty('open', true)

    await files.getByRole('combobox', { name: 'Files per page' }).selectOption('50')
    await expect(rows).toHaveCount(50)
    await expect(controls).toContainText('50 shown')
    await expect(controls.getByRole('button')).toHaveCount(0)
    await files.getByRole('combobox', { name: 'Files per page' }).selectOption('0')
    await expect(rows).toHaveCount(50)
    await files.getByRole('combobox', { name: 'Files per page' }).selectOption('25')
    await expect(rows).toHaveCount(25)
    await expect(controls).toContainText('Page 1 of 2')
    await page.reload()
    await expect(files).toHaveJSProperty('open', false)
    await expect(seasons).toHaveJSProperty('open', false)
  })

  for (const count of [0, 25]) {
    test(`movie files handle ${count} rows at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 844 })
      await fixture(page, count, 'movie')
      await expect(page.locator('.item-seasons')).toHaveCount(0)
      const files = page.locator('.item-files > details')
      await expect(files).toHaveJSProperty('open', false)
      await files.locator('summary').click()
      if (count === 0) {
        await expect(files.getByText(/No files on disk yet/)).toBeVisible()
        await expect(files.locator('table')).toHaveCount(0)
        await expect(files.getByRole('combobox')).toHaveCount(0)
      } else {
        await expect(files.locator('tbody tr')).toHaveCount(25)
        await expect(files.getByText('25 shown', { exact: true })).toBeVisible()
      }
      await expect(files.getByRole('button', { name: 'Next ›' })).toHaveCount(0)
    })
  }
}
