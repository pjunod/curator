import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// Layout regressions are invisible to every other kind of test: the data is
// right, the API is right, and the page is unusable. These assert the two
// things that actually went wrong — rows growing to six lines because a
// column was starved, and columns overlapping because a fixed-width one was
// too narrow for its own content.
test.describe.configure({ mode: 'serial' })

test('queue rows stay one or two lines tall', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/activity')
  await page.getByTestId('toggle-imported').click() // finished rows start collapsed
  const table = page.locator('.queue-table').last()
  await expect(table).toBeVisible()

  const rows = table.locator('tbody tr:not(.handoff-row)')
  const count = await rows.count()
  expect(count).toBeGreaterThan(0)

  for (let i = 0; i < count; i++) {
    const box = await rows.nth(i).boundingBox()
    if (!box) continue
    // A row with a wrapped title and two rows of buttons is ~80px. The
    // version that prompted this was over 300: every action button on its
    // own line, and "1h 30m 28s ago" wrapped onto four.
    expect(box.height, `queue row ${i} is ${box.height}px tall`).toBeLessThan(140)
  }
})

test('queue columns do not overlap each other', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/activity')
  await page.getByTestId('toggle-imported').click() // finished rows start collapsed
  const firstRow = page.locator('.queue-table tbody tr:not(.handoff-row)').first()
  await expect(firstRow).toBeVisible()

  // table-layout: fixed does not grow a column to fit its content — a column
  // too narrow OVERFLOWS into its neighbour instead of wrapping, which had
  // the quality text sitting on top of the state pill.
  const cells = firstRow.locator('td')
  const boxes = []
  for (let i = 0; i < (await cells.count()); i++) {
    boxes.push(await cells.nth(i).boundingBox())
  }
  for (let i = 1; i < boxes.length; i++) {
    const prev = boxes[i - 1]
    const cur = boxes[i]
    if (!prev || !cur) continue
    expect(cur.x, `column ${i} starts before column ${i - 1} ends`).toBeGreaterThanOrEqual(
      prev.x + prev.width - 1,
    )
  }
})

for (const width of [390, 1280]) {
  test(`reservation errors stay compact and readable at ${width}px`, async ({ page }) => {
    const error = 'The client accepted the request but its exact handle could not be committed; Curator is reconciling by transfer token while protecting this reservation.'
    await page.route('**/api/v1/queue/summary', route => route.fulfill({ json: {
      total: 1, active: 1, counts: { grabbed: 1 }, retentionDays: 30,
    } }))
    await page.route('**/api/v1/queue?*', route => route.fulfill({ json: [{
      id: 778, mediaItemId: 8, title: 'A.Long.Release.Name.2026.1080p.WEB-DL', state: 'grabbed',
      progress: 0, protocol: 'usenet', quality: 'WEB-DL 1080p', addedAt: new Date().toISOString(),
      submissionPhase: 'uncertain', error, parkedReason: error,
    }] }))

    await page.setViewportSize({ width, height: 900 })
    await page.goto('/activity')
    const row = page.locator('.queue-table tbody tr:not(.section-row)').first()
    await expect(row).toBeVisible()
    const box = await row.boundingBox()
    expect(box?.height ?? 999).toBeLessThan(width < 500 ? 260 : 140)
    let toggle = row.getByRole('button', { name: /handoff/ })
    if (await toggle.getAttribute('aria-expanded') === 'true') {
      await toggle.click()
      toggle = row.getByRole('button', { name: 'Show handoff' })
    }
    await toggle.focus()
    await page.keyboard.press('Enter')
    await expect(toggle).toHaveAttribute('aria-expanded', 'true')
    await expect(page.locator('.handoff').getByText(error, { exact: false })).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
    await page.screenshot({ path: test.info().outputPath(`queue-diagnostic-${width}.png`) })
  })
}

test('a long file list scrolls instead of growing the page', async ({ page }) => {
  const dir = mkdtempSync(join(process.env.E2E_MEDIA_ROOT!, 'long-import-'))
  for (let i = 1; i <= 30; i++) {
    writeFileSync(join(dir, `Test.Show.S01E${String(i).padStart(2, '0')}.1080p.WEB-DL.mkv`), 'x')
  }
  await page.goto('/activity')
  await page.getByRole('button', { name: 'Resolve', exact: true }).click()
  // The folder preview and the per-file import report are both unbounded —
  // a season pack is 24 rows and a mis-pointed path can be hundreds.
  const panel = page.locator('.manual-import')
  await expect(panel).toBeVisible()
  await panel.getByRole('textbox').first().fill(dir)
  await panel.getByRole('button', { name: 'Scan' }).click()

  // The media root the suite created has files in it, so the preview must
  // render — a conditional assertion here would pass while testing nothing.
  const scroller = panel.locator('.log-scroll').first()
  await expect(scroller).toBeVisible()
  const max = await scroller.evaluate((el) => getComputedStyle(el).maxHeight)
  expect(max).not.toBe('none')
  const box = await scroller.boundingBox()
  expect(box?.height ?? 0).toBeLessThanOrEqual(360)
})

test('all failed rows can be cleared from their grouping after confirmation', async ({ page }) => {
  let failed = 2
  const finished = 3
  let deletes = 0

  await page.route('**/api/v1/queue/summary', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ total: failed + finished, active: 0, counts: { failed, imported: finished }, retentionDays: 30 }),
    })
  })
  await page.route('**/api/v1/queue?*', async (route) => {
    await route.fulfill({ contentType: 'application/json', body: '[]' })
  })
  await page.route('**/api/v1/queue/failed', async (route) => {
    expect(route.request().method()).toBe('DELETE')
    deletes += 1
    const cleared = failed
    failed = 0
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ cleared }) })
  })

  await page.goto('/activity')
  const failedGroup = page.locator('.activity-group').filter({ has: page.getByTestId('toggle-failed') })
  const finishedGroup = page.locator('.activity-group').filter({ has: page.getByTestId('toggle-imported') })
  await expect(failedGroup.getByTestId('clear-failed')).toBeVisible()
  await expect(finishedGroup.getByTestId('clear-finished')).toBeVisible()

  await failedGroup.getByTestId('clear-failed').click()
  await expect(page.getByText('Clear 2 failed rows?')).toBeVisible()
  await page.getByRole('button', { name: 'Yes, clear' }).click()

  await expect.poll(() => deletes).toBe(1)
  await expect(page.getByTestId('activity-counts')).toContainText('0 failed')
  await expect(page.getByTestId('clear-failed')).toHaveCount(0)
  await expect(finishedGroup.getByTestId('clear-finished')).toBeVisible()
})
