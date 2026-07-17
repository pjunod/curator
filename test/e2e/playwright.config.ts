import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defineConfig } from '@playwright/test'

// End-to-end tests boot the REAL compiled binary (./bin/monarr, built by
// `make build` so the web UI is embedded) against a throwaway data dir, then
// drive it with a real browser. Run from test/e2e: `npm test`.
//
// PW_CHROMIUM_PATH: optional absolute path to a chromium executable, for
// environments with a preinstalled browser instead of `playwright install`.

const PORT = Number(process.env.E2E_PORT ?? 7677)
const dataDir = mkdtempSync(join(tmpdir(), 'monarr-e2e-'))

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  fullyParallel: false, // one shared server; specs are cheap
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    launchOptions: process.env.PW_CHROMIUM_PATH
      ? { executablePath: process.env.PW_CHROMIUM_PATH }
      : {},
  },
  webServer: {
    command: join(import.meta.dirname, '..', '..', 'bin', 'monarr'),
    url: `http://127.0.0.1:${PORT}/api/v1/system/status`,
    reuseExistingServer: false,
    timeout: 15_000,
    env: {
      MONARR_HOST: '127.0.0.1',
      MONARR_PORT: String(PORT),
      MONARR_DATA_DIR: dataDir,
      MONARR_LOG_LEVEL: 'warn',
    },
  },
})
