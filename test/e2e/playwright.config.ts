import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

// End-to-end tests boot the REAL compiled binary (./bin/monarr, built by
// `make build` so the web UI is embedded) against a throwaway data dir plus
// a local fake TMDB server, then drive it with a real browser. Run from
// test/e2e: `npm test`.
//
// PW_CHROMIUM_PATH: optional absolute path to a chromium executable, for
// environments with a preinstalled browser instead of `playwright install`.

const PORT = Number(process.env.E2E_PORT ?? 7677)
const TMDB_PORT = Number(process.env.FAKE_TMDB_PORT ?? 7788)
const ARR_PORT = Number(process.env.FAKE_ARR_PORT ?? 7799)
process.env.FAKE_ARR_PORT = String(ARR_PORT)
const dataDir = mkdtempSync(join(tmpdir(), 'monarr-e2e-'))

// Shared with specs via env: a scratch root folder for library tests.
const mediaRoot = mkdtempSync(join(tmpdir(), 'monarr-e2e-media-'))
process.env.E2E_MEDIA_ROOT = mediaRoot

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  fullyParallel: false, // one shared server; specs are cheap and stateful
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    launchOptions: process.env.PW_CHROMIUM_PATH
      ? { executablePath: process.env.PW_CHROMIUM_PATH }
      : {},
  },
  projects: [
    // The main suite runs desktop-sized; the phone shell has its own spec
    // at an iPhone viewport, run AFTER (it reads state the suite builds).
    { name: 'desktop', testIgnore: /mobile\.spec\.ts/ },
    {
      name: 'mobile',
      testMatch: /mobile\.spec\.ts/,
      dependencies: ['desktop'],
      use: {
        ...devices['iPhone 13'],
        // Keep the preinstalled chromium; the device preset only sets
        // viewport/UA/touch.
        browserName: 'chromium',
        launchOptions: process.env.PW_CHROMIUM_PATH
          ? { executablePath: process.env.PW_CHROMIUM_PATH }
          : {},
      },
    },
  ],
  webServer: [
    {
      command: `node ${join(import.meta.dirname, 'fake-tmdb.mjs')}`,
      url: `http://127.0.0.1:${TMDB_PORT}/search/movie`,
      reuseExistingServer: false,
      timeout: 10_000,
      env: { FAKE_TMDB_PORT: String(TMDB_PORT) },
    },
    {
      command: `node ${join(import.meta.dirname, 'fake-arr.mjs')}`,
      url: `http://127.0.0.1:${ARR_PORT}/api/v2/app/version`,
      reuseExistingServer: false,
      timeout: 10_000,
      env: { FAKE_ARR_PORT: String(ARR_PORT) },
    },
    {
      command: join(import.meta.dirname, '..', '..', 'bin', 'monarr'),
      url: `http://127.0.0.1:${PORT}/api/v1/system/status`,
      reuseExistingServer: false,
      timeout: 15_000,
      env: {
        MONARR_HOST: '127.0.0.1',
        MONARR_PORT: String(PORT),
        MONARR_DATA_DIR: dataDir,
        MONARR_LOG_LEVEL: 'warn',
        MONARR_TMDB_BASE_URL: `http://127.0.0.1:${TMDB_PORT}`,
        // Open Library paths don't collide with TMDB's; one fake serves both.
        MONARR_OPENLIBRARY_BASE_URL: `http://127.0.0.1:${TMDB_PORT}`,
      },
    },
  ],
})
