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

test('a long file list scrolls instead of growing the page', async ({ page }) => {
  await page.goto('/activity')
  await page.getByRole('button', { name: 'Manual import' }).click()
  // The folder preview and the per-file import report are both unbounded —
  // a season pack is 24 rows and a mis-pointed path can be hundreds.
  const panel = page.locator('.manual-import')
  await expect(panel).toBeVisible()
  await panel.getByRole('textbox').first().fill(process.env.E2E_MEDIA_ROOT ?? '/tmp')
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
