import config from './playwright.config'

// Focused Safari-engine coverage for responsive search. Opt in with
// `npx playwright test --config=playwright.webkit.config.ts` after installing
// WebKit (`npx playwright install webkit`); the full CI suite uses Chromium.
export default {
  ...config,
  projects: [{
    name: 'webkit',
    testMatch: /release-layout\.spec\.ts/,
    use: { browserName: 'webkit' as const, launchOptions: {} },
  }],
}
