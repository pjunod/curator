import { expect, test } from '@playwright/test'

// ADR 0026: claimed and measured HDR badges in interactive search, and the
// pre-flight evidence panel. Route fixtures own the API, as in
// release-layout.spec.ts, so the checks do not depend on indexers or Runner.
test.use({ serviceWorkers: 'block' })

const probe = {
  status: 'measured', file: 'Title.2021.2160p.mkv', path: ['yenc', 'rar-store'],
  articles: 6, bytes: 3_984_211, millis: 4200, container: 'mkv',
  facts: '2160p · HEVC 10-bit · DV P7 FEL · HDR10 · TrueHD', tier: 'dv-p7-fel',
  dolbyVision: {
    profile: 7, level: 6, rpu: true, el: true, bl: true, blCompatId: 6, elType: 'fel',
    cmVersion: '2.9', masteringMaxNits: 1000, source: 'rpu', label: 'DV P7 FEL',
  },
}

const releases = [
  {
    title: 'Title.2021.2160p.UHD.BluRay.REMUX.DV.HDR.HEVC.TrueHD.7.1.Atmos-GRP',
    downloadUrl: 'https://indexer.example/a', indexer: 'idx', protocol: 'usenet',
    size: 71.4 * 1024 ** 3, seeders: 0, age: '2d', quality: 'Remux 2160p', score: 0,
    accepted: true, isUpgrade: false, rejections: [], candidateToken: 'a',
    match: { version: 1, matched: true, reason: 'Canonical title and year match.' },
    hdr: { tier: 'dv-p7', label: 'DV P7', dvProfile: 7, confidence: 'medium', measured: false },
    measuredHdr: { tier: 'dv-p7-fel', label: 'DV P7 FEL', dvProfile: 7, elType: 'fel', measured: true },
    probe,
  },
  {
    title: 'a8f3c1e9d04b77e2c519f0a6',
    downloadUrl: 'https://indexer.example/b', indexer: 'idx', protocol: 'usenet',
    size: 66 * 1024 ** 3, seeders: 0, age: '2d', quality: 'Remux 2160p', score: 0,
    accepted: true, isUpgrade: false, rejections: [], candidateToken: 'b',
    match: { version: 1, matched: true, reason: 'Canonical title and year match.' },
    probe: { status: 'unavailable', reason: 'encrypted', detail: 'Title.part1.rar is password-protected' },
  },
]

for (const width of [390, 1280]) {
  test(`pre-flight evidence opens in view and returns focus at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 })
    await page.route('**/api/v1/library/7001', route => route.fulfill({ json: {
      id: 7001, kind: 'movie', title: 'Title', year: 2021, monitored: true, path: '',
      genres: [], ratings: [], ids: {}, seasons: [], files: [], copies: [], aliases: [],
      downloadPriority: 0, discEnhancementLayer: 'fel',
    } }))
    await page.route('**/api/v1/library/7001/releases', route => route.fulfill({ json: releases }))

    await page.goto('/library/7001')
    await expect(page.getByTestId('disc-master')).toHaveText('FEL on disc')
    await page.getByRole('button', { name: 'Interactive search', exact: true }).click()
    const panel = page.locator('.release-search')
    await expect(panel.getByTestId('probe-summary')).toContainText('top 2 probed')

    const rows = panel.locator('tbody tr')
    await expect(rows.first().locator('.hdr-badge.claim')).toHaveText('DV P7?')
    const measured = rows.first().getByRole('button', { name: 'DV P7 FEL' })
    await measured.click()
    const evidence = panel.getByTestId('probe-evidence')
    await expect(evidence).toBeInViewport({ ratio: 1 })
    await expect(evidence).toBeFocused()
    await expect(evidence).toContainText('FEL · residual present')
    await expect(evidence).toContainText('RAR (store)')
    if (width < 768) {
      expect(await panel.evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1)
    }
    await page.keyboard.press('Escape')
    await expect(evidence).toHaveCount(0)
    await expect(measured).toBeFocused()

    const refused = rows.nth(1).getByRole('button', { name: /Can't peek · encrypted/ })
    await refused.click()
    await expect(panel.getByTestId('probe-evidence')).toContainText('password-protected')
  })
}
