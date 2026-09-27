import { expect, test } from '@playwright/test'

// Route fixtures must own API requests even after the PWA would take control.
test.use({ serviceWorkers: 'block' })

// Isolated API fixtures keep the layout checks independent of indexers and
// other specs. Assert geometry as well as behavior: text can be present while
// its column has collapsed to one character per line.
for (const width of [320, 390, 430, 820, 1280]) {
  test(`interactive search stays readable at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 844 })
    const releases = Array.from({ length: 51 }, (_, i) => ({
      title: `Oceans.Our.Blue.Planet.2018.2160p.UHD.BluRay.REMUX.HDR.HEVC.DTS-HD.MA.5.1-Group${i}`,
      infoUrl: `https://indexer.example/details/${i}`,
      downloadUrl: `https://indexer.example/download/${i}`,
      indexer: 'AnIndexWithAnUnusuallyLongUnbrokenName',
      protocol: i % 2 ? 'torrent' : 'usenet',
      size: 18.2 * 1024 ** 3, seeders: 123, age: '2d',
      quality: 'REMUX 2160p', score: 100, formats: ['HDR'],
      accepted: i !== 0, isUpgrade: i !== 0,
      match: { version: 1, matched: true, reason: 'Canonical title and year match.' },
      candidateToken: `candidate-${i}`,
      warning: i === 0 ? 'Release size is unusually small for the claimed quality.' : '',
      rejections: i === 0 ? [
        { code: 'quality', reason: 'Quality is not allowed by the profile.' },
        { code: 'score', reason: 'Custom format score is below the minimum.' },
      ] : [],
    }))
    await page.route('**/api/v1/library/9876', route => route.fulfill({ json: {
      id: 9876, kind: 'movie', title: 'Oceans: Our Blue Planet', year: 2018,
      monitored: true, path: '', genres: [], ratings: [], ids: {},
      seasons: [], files: [], copies: [], downloadPriority: 0,
    } }))
    await page.route('**/api/v1/library/9876/releases', route => route.fulfill({ json: releases }))
    let grabbed: unknown
    await page.route('**/api/v1/grab', async route => {
      grabbed = route.request().postDataJSON()
      await route.fulfill({ json: { id: 1 } })
    })

    await page.goto('/library/9876')
    await page.getByRole('button', { name: 'Interactive search', exact: true }).click()
    const panel = page.locator('.release-search')
    const rows = panel.locator('tbody tr')
    await expect(rows).toHaveCount(50)
    const title = rows.first().locator('.release-title')
    const titleBox = await title.boundingBox()
    expect(titleBox!.width).toBeGreaterThan(240)
    expect(titleBox!.height).toBeLessThan(130)

    if (width < 768) {
      // Neither the panel nor its controls should require sideways panning.
      expect(await panel.evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1)
      expect(await panel.locator('.release-table-wrap').evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1)
      const titleCell = await rows.first().locator('td').first().boundingBox()
      const qualityCell = await rows.first().locator('[data-label="Quality"]').boundingBox()
      expect(qualityCell!.y).toBeGreaterThanOrEqual(titleCell!.y + titleCell!.height)
      for (const label of ['Quality', 'Size', 'Age', 'Seed', 'Indexer']) {
        await expect(rows.first().locator(`[data-label="${label}"]`)).toBeVisible()
      }
    } else {
      const titleCell = await rows.first().locator('td').first().boundingBox()
      const qualityCell = await rows.first().locator('[data-label="Quality"]').boundingBox()
      expect(qualityCell!.x).toBeGreaterThanOrEqual(titleCell!.x + titleCell!.width - 1)
    }

    await rows.first().getByRole('button', { name: '+1 more' }).click()
    await expect(rows.first()).toContainText('Custom format score is below the minimum.')
    await panel.evaluate(el => el.scrollIntoView({ block: 'start' }))
    await page.evaluate(() => window.scrollBy(0, -70))
    await page.screenshot({ path: testInfo.outputPath('interactive-search.png') })

    await panel.getByRole('button', { name: 'Next ›' }).first().click()
    await expect(rows).toHaveCount(1)
    await expect(rows.first()).toContainText('Group50')
    await panel.getByRole('searchbox').fill('Group0')
    await expect(rows).toHaveCount(1)
    await rows.first().getByRole('button', { name: 'Grab', exact: true }).click()
    await expect(panel.locator('.banner')).toContainText('Grabbed')
    expect(grabbed).toMatchObject({
      mediaItemId: 9876, title: releases[0].title, candidateToken: 'candidate-0',
    })
    await panel.getByRole('button', { name: 'Close', exact: true }).click()
    await expect(panel).toHaveCount(0)
  })
}
