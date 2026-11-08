import { expect, test, type Locator } from '@playwright/test'

// Pipeline contract v2 M5: the delivery log shows Cinema's sub-deliveries in
// Cinema's own words, and a Test that passes with something to say shows it.
// Both are checked on a phone and on a desktop: on screen after the click
// (CLAUDE.md section 5), and the six-column log scrolls inside its own box
// rather than widening the page.

test.use({ serviceWorkers: 'block' })

const onScreen = (l: Locator) =>
  l.evaluate(node => {
    const box = node.getBoundingClientRect()
    return box.top >= -1 && box.left >= -1 && box.bottom <= innerHeight + 1 && box.right <= innerWidth + 1
  })

const dir = '/media/tv/A Show With A Rather Long Name (2019)/Season 01'
const now = Date.now()
const deliveries = [
  { id: 3, notifierId: 1, downloadId: 81, event: 'import', attempts: 1, status: 'ok', createdAt: now - 60_000, updatedAt: now,
    directory: dir, attempt: 1, requestKey: 't-81-x/gen/0123456789abcdef/1', visibleAt: now, itemIds: [1201],
    result: 'visible → plurx item 1201' },
  { id: 2, notifierId: 1, downloadId: 82, event: 'import', attempts: 4, status: 'pending', createdAt: now - 40 * 60_000, updatedAt: now,
    directory: dir.replace('01', '02'), attempt: 1, remoteRequestId: 'sr-9', remoteState: 'queued', waitingOn: 'full_scan:3',
    result: 'queued as sr-9, waiting on full_scan:3', nextAt: now + 3_600_000, warning: true },
  { id: 1, notifierId: 1, downloadId: 83, event: 'import', attempts: 2, status: 'pending', createdAt: now - 60_000, updatedAt: now,
    directory: dir.replace('01', '03'), attempt: 2,
    result: `warning: expected ${dir.replace('01', '03')}/E01.mkv not placed (unsupported extension)`, nextAt: now + 300_000 },
]

for (const width of [390, 1280]) {
  test(`the delivery log reads Cinema's answers and stays inside the page at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 })
    await page.route('**/api/v1/notifiers', route => route.fulfill({ json: [
      { id: 1, type: 'plurx', name: 'living room', settings: { url: 'http://plurxd:8080', apiKey: 'plx_x' },
        onGrab: false, onImport: true, onFailed: false, onHealth: false, enabled: true },
    ] }))
    await page.route('**/api/v1/notifiers/1/deliveries', route => route.fulfill({ json: deliveries }))
    await page.route('**/api/v1/notifiers/1/test', route => route.fulfill({ json: {
      message: 'connected; checking the library paths needs a newer cinema',
    } }))
    await page.goto('/settings')

    const row = page.getByRole('row').filter({ hasText: 'living room' })
    await row.scrollIntoViewIfNeeded()
    const test_ = row.getByRole('button', { name: 'Test' })
    await test_.click()
    const note = row.getByText('✓ connected; checking the library paths needs a newer cinema')
    await expect(note).toBeVisible()
    await expect.poll(() => onScreen(note)).toBe(true)

    await row.getByRole('button', { name: 'Delivery log' }).click()
    const log = page.getByTestId('delivery-log')
    await expect(log).toBeVisible()
    await log.scrollIntoViewIfNeeded()
    await expect(log.getByText('visible → plurx item 1201')).toBeVisible()
    await expect(log.getByText(/^warning: queued in cinema, waiting on full_scan:3, retrying in 1h$/)).toBeVisible()
    await expect(log.getByText(/not placed \(unsupported extension\), retrying in 5m$/)).toBeVisible()
    await expect(log.getByText('· attempt 2')).toBeVisible()
    await expect(log.locator('.pill', { hasText: 'visible' })).toBeVisible()
    await expect(log.locator('.pill', { hasText: 'warning' })).toBeVisible()

    // The log's top edge is on screen once revealed, and the page never
    // scrolls sideways: a phone scrolls the table inside its own box.
    await expect.poll(() => log.evaluate(node => node.getBoundingClientRect().top < innerHeight)).toBe(true)
    const pageOverflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
    expect(pageOverflow).toBeLessThanOrEqual(1)
    const box = await log.evaluate(node => ({ scroll: node.scrollWidth, client: node.clientWidth, right: node.getBoundingClientRect().right }))
    expect(box.right).toBeLessThanOrEqual(width + 1)
    if (width < 640) expect(box.scroll).toBeGreaterThan(box.client)

    await page.screenshot({ path: test.info().outputPath(`notifier-deliveries-${width}.png`) })
  })
}
