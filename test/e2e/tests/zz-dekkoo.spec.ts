import { expect, test } from '@playwright/test'

for (const viewport of [
  { width: 1280, height: 900 },
  { width: 390, height: 844 },
]) {
  test(`Dekkoo combines selected categories and searches within the ${viewport.width}px viewport`, async ({
    page,
    request,
  }) => {
    await page.setViewportSize(viewport)
    await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
    let feedRequests = 0
    page.on('request', (req) => {
      if (req.url().includes('/discover/dekkoo')) feedRequests++
    })
    await page.goto('/discover')
    await expect(page.getByRole('heading', { name: 'Discover', exact: true })).toBeVisible()
    expect(feedRequests).toBe(0)
    await page.getByRole('tab', { name: 'Dekkoo', exact: true }).click()
    await expect(
      page.getByRole('heading', { name: 'Gay films and series from Dekkoo' }),
    ).toBeVisible()
    await expect(page.locator('.dekkoo-article')).toHaveCount(1)
    await expect(page.getByText('A Dekkoo series announcement')).toBeVisible()
    await expect(page.getByText('Unrelated company news')).toHaveCount(0)
    await expect(page.getByRole('link', { name: 'Read on Dekkoo' })).toHaveAttribute(
      'href',
      'https://dekkoo.blog/test-series/',
    )
    await expect(page.getByRole('link', { name: 'Combined RSS feed' })).toHaveAttribute(
      'href',
      /category_name=gay-movies,gay-series,gay-romance,gay-comedy,gay-short-films/,
    )
    await expect(page.getByRole('button', { name: 'Find in Curator' })).toBeDisabled()
    await page.getByLabel('Title to find').fill('The Test Show')
    await page.getByLabel('Media type').selectOption('series')
    const find = page.getByRole('link', { name: 'Find in Curator' })
    const actionBox = await find.boundingBox()
    expect(actionBox).not.toBeNull()
    expect(actionBox!.y).toBeGreaterThanOrEqual(0)
    expect(actionBox!.y + actionBox!.height).toBeLessThan(viewport.height - 64)
    await find.click()
    await expect(page).toHaveURL(/kind=series/)
    await expect(page.getByRole('heading', { name: 'Add media', exact: true })).toBeVisible()
    const result = page.getByRole('button', { name: 'View details for The Test Show' })
    await expect(result).toBeVisible()
    const resultBox = await result.boundingBox()
    expect(resultBox).not.toBeNull()
    expect(resultBox!.y).toBeGreaterThanOrEqual(0)
    expect(resultBox!.y + resultBox!.height).toBeLessThan(viewport.height - 64)
  })
}

test('Dekkoo failures are retryable and stale snapshots are labelled', async ({ page }) => {
  let failed = true
  await page.route('**/api/v1/discover/dekkoo', (route) =>
    failed
      ? route.fulfill({
          status: 502,
          json: { error: 'Could not refresh the Dekkoo feed. Try again shortly.' },
        })
      : route.fulfill({
          json: {
            url: 'https://dekkoo.blog/feed/',
            categories: ['Gay Movies'],
            items: [],
            stale: true,
            fetchedAt: '2026-10-05T12:00:00Z',
          },
        }),
  )
  await page.goto('/discover')
  await page.getByRole('tab', { name: 'Dekkoo', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('Could not refresh')
  failed = false
  await page.getByRole('button', { name: 'Try again' }).click()
  await expect(page.getByText(/Dekkoo is temporarily unavailable/)).toBeVisible()
  await expect(page.getByText('No articles in these categories right now.')).toBeVisible()
})
