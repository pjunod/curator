import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

for (const width of [390, 1280]) {
  test(`leftover legacy row opens exact Resolve and submits reviewed files at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    const payloadPath = '/completed/Legacy.Show.S01E01-S01E02'
    const selected = `${payloadPath}/Legacy.Show.S01E01.mkv`
    const sibling = `${payloadPath}/Legacy.Show.S01E02.mkv`
    const fingerprint = 'inventory-fingerprint-7'
    await page.route('**/api/v1/downloads/inventory', route => route.fulfill({ json: {
      roots: [{ path: '/completed', checkedAt: new Date().toISOString(), lastCompleteAt: new Date().toISOString(), error: '',
        entries: [{ path: payloadPath, fingerprint, bytes: 2048, files: 2, status: 'failed', reason: 'pre-delivery history requires explicit review',
          legacyReview: true, deliveryManaged: false, receipts: [{ downloadId: 73, mediaItemId: 8, copyId: 0, title: 'Legacy.Show', live: true, legacyReview: true }],
          reviewFiles: [
            { path: selected, sizeBytes: 1024, modifiedAt: new Date().toISOString(), mode: '-rw-r--r--', snapshotIdentity: 'snapshot-episode-1' },
            { path: sibling, sizeBytes: 1024, modifiedAt: new Date().toISOString(), mode: '-rw-r--r--', snapshotIdentity: 'snapshot-episode-2' },
          ],
        }], bytes: 2048 }], bytes: 2048, attention: 1,
    } }))
    await page.route('**/api/v1/system/tasks', route => route.fulfill({ json: [] }))
    await page.route('**/api/v1/queue/summary', route => route.fulfill({ json: {
      total: 0, active: 0, counts: { grabbed: 0, downloading: 0, downloaded: 0, awaiting_import: 0, importing: 0, imported: 0, failed: 0 },
    } }))
    await page.route('**/api/v1/queue?*', route => route.fulfill({ json: [] }))
    await page.route('**/api/v1/library', route => route.fulfill({ json: [{ id: 8, kind: 'series', title: 'Legacy Show', year: 2020 }] }))
    await page.route('**/api/v1/import/scan?*', route => route.fulfill({ json: [
      { path: selected, name: 'Legacy.Show.S01E01.mkv', size: 1024, kind: 'video', quality: 'WEBDL-1080p', season: 1, episodes: [1], snapshotIdentity: 'snapshot-episode-1' },
      { path: sibling, name: 'Legacy.Show.S01E02.mkv', size: 1024, kind: 'video', quality: 'WEBDL-1080p', season: 1, episodes: [2], snapshotIdentity: 'snapshot-episode-2' },
    ] }))
    let submitted: Record<string, unknown> | undefined
    await page.route('**/api/v1/import/manual', async route => {
      submitted = route.request().postDataJSON()
      await route.fulfill({ status: 202, json: { downloadId: 900 } })
    })
    await page.goto('/activity')
    await page.getByText('Storage entries (1)').click()
    const row = page.getByRole('listitem').filter({ hasText: 'Legacy.Show.S01E01-S01E02' })
    const trigger = row.getByRole('button', { name: 'Review legacy batch' })
    await trigger.scrollIntoViewIfNeeded()
    await expect(trigger).toBeInViewport({ ratio: 1 })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Resolve' })
    const heading = dialog.locator('.action-dialog-head h2')
    await expect(heading).toBeInViewport({ ratio: 1 })
    await expect(heading).toBeFocused()
    await expect(dialog.getByRole('heading', { name: 'Resolve', exact: true })).toHaveCount(1)
    await dialog.getByRole('button', { name: 'Inspect files and choose destination' }).click()
    await expect(dialog.getByText(/tied to this inventory snapshot/)).toBeVisible()
    await dialog.getByRole('button', { name: 'Scan', exact: true }).click()
    await expect(dialog.getByRole('checkbox', { name: `Select Legacy.Show.S01E01.mkv` })).toBeVisible()
    await dialog.getByRole('checkbox', { name: 'Select Legacy.Show.S01E01.mkv' }).check()
    await dialog.getByLabel('Import into').selectOption('8')
    const queue = dialog.getByRole('button', { name: 'Queue selected (1)' })
    await queue.scrollIntoViewIfNeeded()
    await expect(queue).toBeInViewport({ ratio: 1 })
    await expect(queue).toBeEnabled()
    await expect(queue).toHaveText('Queue selected (1)')
    await expect.poll(() => queue.evaluate(node => {
      const style = getComputedStyle(node)
      return style.color !== style.backgroundColor
    })).toBe(true)
    await page.screenshot({ path: test.info().outputPath(`resolve-${width}.png`) })
    await queue.click()
    await expect.poll(() => submitted).toMatchObject({
      path: payloadPath, paths: [selected], mediaItemId: 8, reviewLegacy: true,
      reviewLegacyFingerprint: fingerprint,
      reviewLegacyFiles: [{ path: selected, snapshotIdentity: 'snapshot-episode-1' }],
    })
    expect((submitted?.paths as string[])).not.toContain(sibling)
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })

  test(`Runner lifecycle trace and retries stay reviewable at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    await page.route('**/api/v1/queue/summary', route => route.fulfill({ json: {
      total: 1, active: 1, counts: { grabbed: 0, downloading: 1, downloaded: 0, awaiting_import: 0, importing: 0, imported: 0, failed: 0 },
    } }))
    await page.route('**/api/v1/queue?*', route => route.fulfill({ json: [{
      id: 41, mediaItemId: 8, clientId: 2, title: 'Lifecycle.Show.S01E01', state: 'downloading',
      progress: 0.8, protocol: 'usenet', quality: 'WEBDL-1080p', addedAt: new Date().toISOString(),
      handoff: [{ step: 'grabbed', at: Date.now() - 120000 }, { step: 'downloading', at: Date.now() - 60000 }],
    }] }))
    await page.route('**/api/v1/downloads/deliveries?*', route => route.fulfill({ json: { items: [{
      receiptId: 91, clientId: 2, downloadId: 41, generationKey: 'generation-41', mediaItemId: 8, copyId: 0,
      deliveryId: 'delivery-91', publicationGeneration: 'publication-91', manifestDigest: 'a'.repeat(64),
      runnerInstanceId: 'runner-2', jobId: '41', requestToken: 'transfer-41', publicationPath: '/processing/41',
      mappedPath: '/completed/Lifecycle.Show.S01E01', readyAt: new Date().toISOString(), terminalOutcome: 'PAR_FAILURE',
      failureStage: 'par_verify', failureCause: 'one recovery block is missing', keepSourceOnFailure: true,
      state: 'imported', acceptance: 'accepted', consumerReceiptId: 'consumer-91', attempts: 2,
      blockedSince: new Date(Date.now() - 35 * 60 * 1000).toISOString(), nextAttemptAt: '', error: 'Runner source cleanup is blocked',
      manifestFiles: [{ path: 'Season 01/Episode 01.mkv', sizeBytes: 1024, sha256: 'b'.repeat(64) }],
      sourceFileOutcomes: [{ sourcePath: 'Season 01/Episode 01.mkv', publicationPath: 'Season 01/Episode 01.mkv', classification: 'selected_output', outcome: 'published', reason: 'verified output', sourceCleanupStatus: 'retained' }],
      sourceCleanup: { status: 'retry', attempts: 2, lastAttemptAt: Math.floor(Date.now() / 1000) - 60, warning: 'recovery source retained', attemptHistory: [
        { attempt: 1, startedAtMs: Date.now() - 20000, endedAtMs: Date.now() - 16000, workMs: 4000, waitMs: 0, outcome: 'retry', error: 'temporary read error' },
        { attempt: 2, startedAtMs: Date.now() - 10000, endedAtMs: null, workMs: null, waitMs: 6000, outcome: 'running' },
      ] },
      attemptHistory: [
        { id: 1, stage: 'ack', attempt: 1, startedAt: new Date(Date.now() - 20000).toISOString(), finishedAt: new Date(Date.now() - 16000).toISOString(), result: 'failed', error: 'temporary outage' },
        { id: 2, stage: 'ack', attempt: 2, startedAt: new Date(Date.now() - 10000).toISOString(), finishedAt: new Date().toISOString(), result: 'succeeded' },
      ],
      sourceCleanupAttempts: 2, sourceCleanupError: 'permission denied',
      directoryCleanupState: 'retry', directoryCleanupAttempts: 3, directoryCleanupError: 'NAS temporarily unavailable',
      curatorFiles: [{ path: 'Season 01/Episode 01.mkv', sizeBytes: 1024, sha256: 'b'.repeat(64), disposition: 'placed', cleanupState: 'retry', reason: 'placed in the selected copy', cleanupError: 'NAS temporarily unavailable', cleanupAttempts: 3 }],
    }], nextBeforeId: 0 } }))
    await page.route('**/api/v1/downloads/deliveries/91/retry-handoff', route => route.fulfill({ status: 202, body: '' }))
    await page.route('**/api/v1/downloads/deliveries/91/retry-runner-cleanup', route => route.fulfill({ status: 202, body: '' }))
    await page.route('**/api/v1/downloads/deliveries/91/retry-curator-cleanup', route => route.fulfill({ status: 202, body: '' }))
    await page.goto('/activity')
    await page.locator('.delivery-lifecycle-archive > summary').click()
    const trace = page.locator('.delivery-lifecycle').filter({ hasText: 'delivery-91' })
    await expect(trace).toBeVisible()
    await expect(trace.locator(':scope > summary')).toContainText('Runner handoff · Lifecycle.Show.S01E01 · imported')
    await trace.locator(':scope > summary').click()
    await expect(trace.getByText('/completed/Lifecycle.Show.S01E01', { exact: false }).first()).toBeVisible()
    await expect(trace.getByText(/acknowledgement: accepted/)).toBeVisible()
    await expect(trace.getByText(/accepted accepted/)).toHaveCount(0)
    await trace.getByText('Curator attempt history (2)', { exact: true }).click()
    await trace.getByText('Runner source cleanup attempts (2)', { exact: true }).click()
    await trace.getByText('Per-file outcomes (1 publication files)', { exact: true }).click()
    await expect(trace.getByText(/blocked for at least 30 minutes/)).toBeVisible()
    await expect(trace.getByText(/work 4s/).first()).toBeVisible()
    await expect(trace.getByText(/wait before attempt 6s/)).toBeVisible()
    await expect(trace.getByText(/work duration not recorded/)).toBeVisible()
    await expect(trace.getByText(/Season 01\/Episode 01\.mkv/).first()).toBeVisible()
    for (const name of ['Retry handoff', 'Retry Runner source cleanup', 'Retry Curator cleanup']) {
      const button = trace.getByRole('button', { name })
      await button.scrollIntoViewIfNeeded()
      await expect.poll(() => button.evaluate(node => {
        const box = node.getBoundingClientRect()
        return box.top >= -1 && box.left >= -1 && box.bottom <= innerHeight + 1 && box.right <= innerWidth + 1
      })).toBe(true)
      await expect(button).toBeEnabled()
    }
    await trace.locator(':scope > summary').scrollIntoViewIfNeeded()
    await page.screenshot({ path: test.info().outputPath(`lifecycle-${width}.png`) })
  })

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
    await page.locator('.item-seasons > details > summary').click()
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

  test(`replacement confirmation stays visible and returns focus at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 })
    await page.route('**/api/v1/library/9872', route => route.fulfill({ json: {
      id: 9872, kind: 'movie', title: 'Reviewed File', monitored: true, path: '/library/Reviewed File',
      genres: [], ratings: [], ids: {}, seasons: [], copies: [],
      files: [{ id: 44, path: '/library/Reviewed File/movie.mkv', size: 1234, episodeIds: [], copyId: 0,
        fileGeneration: 'file-generation-44', sourceGeneration: 'download-generation-44', sourceTransfer: 'transfer-44',
        sourceRelease: 'Reviewed.Movie.1080p', sourceGuid: 'guid-44', sourceIndexerId: 8, sourceIndexer: 'Index' }],
    } }))
    await page.route('**/api/v1/library/9872/files/44/replacement-observations', route => route.fulfill({ status: 202, json: {
      observationId: 'obs-44', requestId: 'obs-request', state: 'pending', fileGeneration: 'file-generation-44',
    } }))
    await page.route('**/api/v1/replacement-observations/obs-44', route => route.fulfill({ json: {
      observationId: 'obs-44', requestId: 'obs-request', state: 'ready', fileGeneration: 'file-generation-44',
      sourceGeneration: 'download-generation-44', sha256: 'a'.repeat(64), size: 1234,
    } }))
    let submitted: unknown
    await page.route('**/api/v1/library/9872/replacement-operations', async route => {
      if (route.request().method() === 'GET') {
        await route.fulfill({ json: { operations: [], openReports: [] } })
        return
      }
      submitted = route.request().postDataJSON()
      await route.fulfill({ status: 409, json: { message: 'The selected file changed after review.' } })
    })
    await page.goto('/library/9872')
    await page.locator('.item-files details > summary').click()
    const trigger = page.getByRole('button', { name: 'Mark bad & replace' })
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Review replacement' })
    await expect(dialog.getByRole('heading')).toBeFocused()
    await expect(dialog.getByRole('heading')).toBeInViewport({ ratio: 1 })
    await dialog.getByRole('button', { name: 'Prepare 1 file review', exact: true }).click()
    await expect(dialog.getByText('Reviewed all 1 files in the selected source scope.')).toBeVisible()
    await expect(dialog.getByRole('button', { name: 'Confirm bad & replace' })).toBeDisabled()
    await dialog.getByRole('radio', { name: 'No, exclude it only from this request' }).check()
    const reasonField = dialog.locator('.replacement-reason-field')
    const reason = reasonField.locator('textarea')
    await expect(reasonField).toBeVisible()
    await expect(reason).toBeVisible()
    await expect.poll(() => reasonField.evaluate(node => getComputedStyle(node).display)).toBe('grid')
    await expect.poll(() => reason.evaluate(node => Number.parseFloat(getComputedStyle(node).width))).toBeGreaterThan(280)
    await page.screenshot({ path: test.info().outputPath(`replacement-${width}.png`) })
    await dialog.getByRole('button', { name: 'Confirm bad & replace' }).click()
    const error = dialog.getByRole('status')
    await expect(error).toContainText('selected file changed after review')
    await expect.poll(() => error.evaluate(node => {
      const box = node.getBoundingClientRect()
      return box.top >= -1 && box.left >= -1 && box.bottom <= innerHeight + 1 && box.right <= innerWidth + 1
    })).toBe(true)
    expect(submitted).toMatchObject({
      requestId: expect.any(String), sourceGeneration: 'download-generation-44', scope: 'download',
      blacklistSourceRelease: false,
      observedFiles: [{ id: 44, fileGeneration: 'file-generation-44', observationId: 'obs-44' }],
    })
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
    await expect(trigger).toBeInViewport({ ratio: 1 })
  })
}

for (const width of [390, 820, 1280]) {
  for (const layout of ['classic', 'theater']) {
    test(`repeated inline feedback clears ${layout} navigation at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 700 })
      await page.addInitScript(value => localStorage.setItem('monarr-layout', value), layout)
      await page.route('**/api/v1/library/9873', route => route.fulfill({ json: {
        id: 9873, kind: 'series', title: 'Monitoring feedback', monitored: true,
        path: '', genres: [], ratings: [], ids: {}, files: [], copies: [],
        seasons: [{ number: 1, monitored: true, episodes: Array.from({ length: 80 }, (_, i) => ({
          id: i + 1, seasonNumber: 1, episodeNumber: i + 1, title: `Episode ${i + 1}`, monitored: true,
        })) }],
      } }))
      await page.route('**/api/v1/library/9873/episodes/*', route => route.fulfill({
        status: 500, json: { message: 'Monitoring unavailable' },
      }))
      await page.goto('/library/9873')
      await page.locator('.item-seasons > details > summary').click()
      const checkbox = page.getByRole('checkbox', { name: 'Monitor episode 1x80', exact: true })
      const notice = page.getByRole('status').filter({ hasText: 'could not change episode monitoring' })
      for (let attempt = 0; attempt < 2; attempt++) {
        await checkbox.click()
        await expect(notice).toContainText('Monitoring unavailable')
        // Allow one CSS pixel of browser rounding, not partial visibility.
        await expect.poll(() => notice.evaluate(node => {
          const box = node.getBoundingClientRect()
          return box.top >= -1 && box.left >= -1 && box.bottom <= innerHeight + 1 && box.right <= innerWidth + 1
        })).toBe(true)
        // IntersectionObserver visibility alone misses opaque navigation that
        // covers a banner. Assert its geometry against the actual shell bars.
        await expect.poll(() => notice.evaluate(node => {
          const box = node.getBoundingClientRect()
          return Array.from(document.querySelectorAll('.mobile-top, .mobile-tabs, [data-layout="theater"] .sidebar')).every(bar => {
            const style = getComputedStyle(bar)
            if (style.position !== 'fixed' && style.position !== 'sticky') return true
            const nav = bar.getBoundingClientRect()
            return box.bottom <= nav.top || box.top >= nav.bottom || box.right <= nav.left || box.left >= nav.right
          })
        })).toBe(true)
        await expect(checkbox).toBeChecked()
      }
    })
  }
}
