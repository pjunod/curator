import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

for (const width of [390, 1280]) {
  for (const [path, label] of [['/', 'Scan disk'], ['/settings', 'Scan now']]) {
    test(`${label} follows a slow scan in the viewport at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 700 })
      let state = 'idle'
      let polls = 0
      let reportReads = 0
      let posts = 0
      let releaseRequest = () => {}
      const requestGate = new Promise<void>((resolve) => { releaseRequest = resolve })
      await page.route('**/api/v1/library/scan', async (route) => {
        if (route.request().method() === 'POST') {
          posts++
          await requestGate
          state = 'queued'
          await route.fulfill({ status: 202, json: { id: 42, state, requestedAt: '2026-10-26T12:00:00Z' } })
        } else {
          polls++
          await route.fulfill({ json: { id: state === 'idle' ? undefined : 42, state, requestedAt: state === 'idle' ? undefined : '2026-10-26T12:00:00Z' } })
        }
      })
      await page.route('**/api/v1/library/scan/report', (route) => {
        reportReads++
        return route.fulfill({ json: {
          scannedAt: '2026-10-26T12:00:00Z', rootsScanned: 1,
          itemsScanned: state === 'completed' ? 12 : 3,
          filesLinked: 8, filesRemoved: 2, unmatchedDirs: [], missingItems: [], missingPaths: [],
        } })
      })
      await page.goto(path)
      await expect.poll(() => polls).toBeGreaterThan(0)
      const trigger = page.getByRole('button', { name: label, exact: true })
      await trigger.scrollIntoViewIfNeeded()
      await trigger.click()
      const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
      const heading = dialog.getByRole('heading', { name: 'Disk scan', exact: true })
      const status = dialog.getByRole('status', { name: 'Disk scan status' })
      await expect(heading).toBeFocused()
      await expect(heading).toBeInViewport({ ratio: 1 })
      await expect(status).toContainText('Requesting disk scan…')
      await expect(status).toBeInViewport({ ratio: 1 })
      releaseRequest()
      await expect(status).toContainText('Disk scan queued')
      await dialog.getByRole('button', { name: 'Close', exact: true }).click()
      const runningTrigger = page.getByRole('button', { name: 'Scanning…', exact: true })
      await expect(runningTrigger).toBeFocused()
      const scroll = await page.evaluate(() => window.scrollY)
      const runningPolls = polls
      state = 'running'
      // Stay active past the old one-shot 1.5-second refresh. Background
      // polling must not pull the user's scroll or focus back to the result.
      await expect.poll(() => polls, { timeout: 10_000 }).toBeGreaterThan(runningPolls + 1)
      expect(await page.evaluate(() => window.scrollY)).toBe(scroll)
      await expect(runningTrigger).toBeFocused()
      await runningTrigger.click()
      await expect(status).toContainText('Scanning disk…')
      await expect(status).toBeInViewport({ ratio: 1 })
      await expect(heading).toBeFocused()
      expect(posts).toBe(1)
      const previousReads = reportReads
      state = 'completed'
      await expect(status).toContainText('12 items scanned · 8 files linked · 2 removed')
      await expect(status).toBeInViewport({ ratio: 1 })
      expect(reportReads).toBeGreaterThan(previousReads)
      await page.screenshot({ path: test.info().outputPath(`disk-scan-${width}.png`) })
      await page.keyboard.press('Escape')
      await expect(dialog).toHaveCount(0)
      await expect(trigger).toBeFocused()
    })
  }
}

for (const [path, label] of [['/', 'Scan disk'], ['/settings', 'Scan now']]) {
  test(`${label} surfaces a rejected request and supports retry`, async ({ page }) => {
    let rejected = true
    let queued = false
    await page.route('**/api/v1/library/scan', (route) => {
      if (route.request().method() === 'POST') {
        if (rejected) return route.fulfill({ status: 500, json: { message: 'queue unavailable' } })
        queued = true
        return route.fulfill({ status: 202, json: { state: 'queued', id: 57 } })
      }
      return route.fulfill({ json: { state: queued ? 'queued' : 'idle', id: queued ? 57 : undefined } })
    })
    await page.goto(path)
    await page.getByRole('button', { name: label, exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
    await expect(dialog.getByRole('alert')).toContainText('Could not start disk scan: queue unavailable')
    rejected = false
    await dialog.getByRole('button', { name: 'Try again', exact: true }).click()
    await expect(dialog.getByRole('status')).toContainText('Disk scan queued')
  })
}

test('scan status survives navigation and exposes retries and worker failures', async ({ page }) => {
  let state = 'running'
  let error = ''
  let posts = 0
  await page.route('**/api/v1/library/scan', (route) => {
    if (route.request().method() === 'POST') posts++
    return route.fulfill({ json: { id: 53, state, error, attempts: 2 } })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Scanning…', exact: true }).click()
  let dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
  await expect(dialog.getByRole('status')).toContainText('Scanning disk…')
  await page.keyboard.press('Escape')
  await page.goto('/settings')
  await page.getByRole('button', { name: 'Scanning…', exact: true }).click()
  dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
  state = 'queued'
  error = 'disk unavailable'
  await expect(dialog.getByRole('status')).toContainText('waiting to retry after 2 attempts: disk unavailable')
  state = 'failed'
  await expect(dialog.getByRole('alert')).toContainText('Disk scan failed: disk unavailable')
  await expect(dialog.getByRole('button', { name: 'Try again', exact: true })).toBeEnabled()
  expect(posts).toBe(0)
})

test('a lost POST response cannot hide a scan observed by polling', async ({ page }) => {
  let accepted = false
  await page.route('**/api/v1/library/scan', (route) => {
    if (route.request().method() === 'POST') {
      accepted = true
      return route.abort('failed')
    }
    return route.fulfill({ json: accepted ? { id: 64, state: 'running' } : { state: 'idle' } })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Scan disk', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
  await expect(dialog.getByRole('status')).toContainText('Scanning disk…')
  await expect(dialog.getByRole('alert')).toHaveCount(0)
})

test('completion never labels a cached previous report as current results', async ({ page }) => {
  let started = false
  let completed = false
  let reportAvailable = false
  await page.route('**/api/v1/library/scan', (route) => {
    if (route.request().method() === 'POST') started = true
    return route.fulfill({ status: route.request().method() === 'POST' ? 202 : 200, json: {
      id: started ? 72 : 71, state: started && !completed ? 'running' : 'completed',
      requestedAt: started ? '2026-10-26T12:00:00Z' : '2026-10-25T12:00:00Z',
    } })
  })
  let reportReads = 0
  await page.route('**/api/v1/library/scan/report', (route) => {
    reportReads++
    if (started && !reportAvailable) return route.fulfill({ status: 500, json: { message: 'report unavailable' } })
    return route.fulfill({ json: {
      scannedAt: started ? '2026-10-26T12:00:00Z' : '2026-10-25T12:00:00Z', rootsScanned: 1,
      itemsScanned: started ? 12 : 888, filesLinked: 8, filesRemoved: 2,
      unmatchedDirs: [], missingItems: [], missingPaths: [],
    } })
  })
  await page.goto('/')
  await expect.poll(() => reportReads).toBeGreaterThan(0)
  await page.getByRole('button', { name: 'Scan disk', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
  await expect(dialog.getByRole('status')).toContainText('Scanning disk…')
  completed = true
  await expect(dialog.getByRole('alert')).toContainText('Could not load scan results: report unavailable', { timeout: 15_000 })
  await expect(dialog).not.toContainText('888 items scanned')
  reportAvailable = true
  await expect(dialog.getByRole('status')).toContainText('12 items scanned', { timeout: 10_000 })
})

test('a stalled status GET cannot prevent the scan POST', async ({ page }) => {
  let posts = 0
  let unblockStatus = () => {}
  const statusGate = new Promise<void>((resolve) => { unblockStatus = resolve })
  await page.route('**/api/v1/library/scan', async (route) => {
    if (route.request().method() === 'POST') {
      posts++
      return route.fulfill({ status: 202, json: { id: 81, state: 'queued' } })
    }
    await statusGate
    return route.fulfill({ json: { id: 81, state: 'queued' } })
  })
  try {
    await page.goto('/')
    await page.getByRole('button', { name: 'Scan disk', exact: true }).click()
    await expect.poll(() => posts).toBe(1)
    await expect(page.getByRole('dialog', { name: 'Disk scan', exact: true }).getByRole('status')).toContainText('Disk scan queued')
  } finally {
    unblockStatus()
  }
})

test('an old completion cannot hide a rejected request when initial status was unknown', async ({ page }) => {
  let gets = 0
  let unblockStatus = () => {}
  const statusGate = new Promise<void>((resolve) => { unblockStatus = resolve })
  await page.route('**/api/v1/library/scan', async (route) => {
    if (route.request().method() === 'POST') return route.fulfill({ status: 500, json: { message: 'queue unavailable' } })
    gets++
    await statusGate
    return route.fulfill({ json: { id: 91, state: 'completed', requestedAt: '2026-10-25T12:00:00Z' } })
  })
  try {
    await page.goto('/')
    await expect.poll(() => gets).toBeGreaterThan(0)
    await page.getByRole('button', { name: 'Scan disk', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
    await expect(dialog.getByRole('alert')).toContainText('Could not start disk scan: queue unavailable')
    unblockStatus()
    const priorGets = gets
    await expect.poll(() => gets).toBeGreaterThan(priorGets)
    await expect(dialog.getByRole('alert')).toContainText('Could not start disk scan: queue unavailable')
    await expect(dialog.getByRole('status')).toHaveCount(0)
  } finally {
    unblockStatus()
  }
})

for (const width of [390, 1280]) {
  for (const [path, label] of [['/', 'Scan disk'], ['/settings', 'Scan now']]) {
    test(`${label} promotes an existing queued scan at ${width}px and exposes priority failures`, async ({ page }) => {
      await page.setViewportSize({ width, height: 700 })
      let posts = 0
      let reject = true
      const scan = { id: 99, state: 'queued', attempts: 0, requestedAt: '2026-10-26T10:00:00Z' }
      await page.route('**/api/v1/library/scan', (route) => {
        if (route.request().method() === 'POST') {
          posts++
          return reject
            ? route.fulfill({ status: 500, json: { message: 'queue unavailable' } })
            : route.fulfill({ status: 202, json: scan })
        }
        return route.fulfill({ json: scan })
      })
      await page.goto(path)
      const trigger = page.getByRole('button', { name: 'Scanning…', exact: true })
      await trigger.scrollIntoViewIfNeeded()
      await trigger.click()
      const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
      await expect(dialog.getByRole('alert')).toContainText('Could not prioritize disk scan: queue unavailable')
      await expect(dialog.getByRole('alert')).toBeInViewport({ ratio: 1 })
      expect(posts).toBe(1)
      reject = false
      await dialog.getByRole('button', { name: 'Retry priority request', exact: true }).click()
      await expect(dialog.getByRole('status')).toContainText('Disk scan queued')
      await expect(dialog.getByRole('status')).toBeInViewport({ ratio: 1 })
      expect(posts).toBe(2)
      await page.keyboard.press('Escape')
      await expect(trigger).toBeFocused()
    })
  }
}

for (const nextState of ['running', 'queued', 'completed']) {
  test(`a failed priority request yields to ${nextState} progress of the same scan`, async ({ page }) => {
    let progressed = false
    let posts = 0
    await page.route('**/api/v1/library/scan', (route) => {
      if (route.request().method() === 'POST') {
        posts++
        return route.fulfill({ status: 500, json: { message: 'queue unavailable' } })
      }
      return route.fulfill({ json: {
        id: 101, state: progressed ? nextState : 'queued', attempts: progressed ? 1 : 0,
        error: progressed && nextState === 'queued' ? 'disk unavailable' : '',
        requestedAt: '2026-10-26T10:00:00Z',
      } })
    })
    await page.route('**/api/v1/library/scan/report', (route) => route.fulfill({ json: {
      scannedAt: '2026-10-26T11:00:00Z', rootsScanned: 1, itemsScanned: 12,
      filesLinked: 8, filesRemoved: 2, unmatchedDirs: [], missingItems: [], missingPaths: [],
    } }))
    await page.goto('/')
    await page.getByRole('button', { name: 'Scanning…', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Disk scan', exact: true })
    await expect(dialog.getByRole('alert')).toContainText('Could not prioritize disk scan')
    progressed = true
    await expect(dialog.getByRole('status')).toContainText(nextState === 'running' ? 'Scanning disk…'
      : nextState === 'queued' ? 'waiting to retry after 1 attempt: disk unavailable' : '12 items scanned')
    await expect(dialog.getByRole('alert')).toHaveCount(0)
    expect(posts).toBe(1)
  })
}
