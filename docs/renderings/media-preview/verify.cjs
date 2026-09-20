// Checks illustrative HTML only. This does not exercise the Monarr app or API.
// From the repo root: PLAYWRIGHT_CHANNEL=chrome node docs/renderings/media-preview/verify.cjs
const { chromium } = require('../../../test/e2e/node_modules/@playwright/test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

(async () => {
  const browser = await chromium.launch({
    headless: true,
    ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}),
  });
  try {
    const page = await browser.newPage({ viewport: { width: 1080, height: 1600 }, deviceScaleFactor: 2 });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('https://**/*', route => route.abort());
    for (const name of ['web', 'mobile']) {
      await page.setViewportSize({ width: 1080, height: 1600 });
      await page.goto(pathToFileURL(path.join(__dirname, `${name}.html`)).href);
      const frame = page.frames().find(candidate => candidate.parentFrame());
      const root = frame.locator(`#monarr-preview-${name}`);
      await root.waitFor();
      await frame.evaluate(() => { document.documentElement.style.colorScheme = 'dark'; });
      const state = async patch => {
        await frame.evaluate(({ name, patch }) => {
          window.dispatchEvent(new CustomEvent('openai:set_globals', {
            detail: { globals: { widgetState: { modelContent: { [`${name}Preview`]: patch } } } },
          }));
        }, { name, patch });
      };
      const screenshot = async filename => {
        assert.equal(await root.evaluate(element =>
          element.scrollWidth <= element.clientWidth && element.scrollHeight <= element.clientHeight
        ), true, `${name}/${filename} must not clip its root content`);
        await root.screenshot({ path: path.join(__dirname, `${filename}.png`) });
      };
      await screenshot(`${name}-preview`);
      if (name === 'web') {
        await state({ kind: 'series' });
        assert.equal(await frame.locator('[data-page-root]').textContent(), '/media/tv');
        assert.match(await frame.locator('[data-summary]').textContent(), /\/media\/tv/);
        assert.match(await frame.locator('[data-meta]').textContent(), /per episode/);
        await screenshot('web-series-preview');
        await state({ kind: 'movie' });
        for (const condition of ['owned', 'conflict', 'ambiguous']) {
          await state({ condition });
          assert.equal(await frame.locator('[data-add]').isDisabled(), condition !== 'owned');
          if (condition === 'owned') assert.equal(await frame.locator('[data-add]').textContent(), 'Open in library');
          if (condition === 'conflict') assert.equal(await frame.locator('.mp-links').isVisible(), false);
          await screenshot(`web-${condition}`);
        }
        await state({ condition: 'ready' });
        await frame.locator('[data-close]').click();
        assert.equal(await frame.locator('.mp-preview').isVisible(), false);
        await screenshot('web-results');
        // The poster is in the stretched details hit area; the sibling Add remains independent.
        const poster = await frame.locator('.mp-row .mp-poster').first().boundingBox();
        assert.ok(poster);
        await page.mouse.click(poster.x + poster.width / 2, poster.y + poster.height / 2);
        assert.equal(await frame.locator('.mp-preview').isVisible(), true);
        await page.keyboard.press('Escape');
        assert.equal(await frame.locator('.mp-preview').isVisible(), false);
        await frame.locator('[data-quick="1"]').click();
        assert.equal(await frame.locator('.mp-preview').isVisible(), false);
        await frame.locator('[data-open="0"]').click();
        await frame.locator('[data-add]').click();
        assert.equal(await frame.locator('[data-quick="0"]').textContent(), 'Added');
        assert.equal(await frame.locator('[data-added]').isVisible(), true);
        await state({ condition: 'ready' });
        await page.setViewportSize({ width: 360, height: 1600 });
        await screenshot('mobile-web-preview');
        await state({ longSynopsis: true, largeText: true });
        await page.setViewportSize({ width: 320, height: 1900 });
        assert.equal(await root.evaluate(e => e.scrollWidth <= e.clientWidth), true);
        await state({ longSynopsis: false, largeText: false });
      } else {
        assert.equal(await frame.locator('[data-back]').isVisible(), false);
        await frame.locator('[data-primary]').click();
        assert.equal(await frame.locator('[data-back]').textContent(), '‹ Details');
        await screenshot('mobile-add-options');
        await frame.locator('[aria-label="Quality profile"]').selectOption('Ultra-HD');
        await frame.locator('[data-view="configure"] input[type=checkbox]').first().uncheck();
        await frame.locator('[data-back]').click();
        await frame.locator('[data-primary]').click();
        assert.equal(await frame.locator('[aria-label="Quality profile"]').inputValue(), 'Ultra-HD');
        assert.equal(await frame.locator('[data-view="configure"] input[type=checkbox]').first().isChecked(), false);
        await frame.locator('[data-close]').click();
        await screenshot('mobile-results');
        await frame.locator('[data-quick]').click();
        assert.equal(await frame.locator('[data-back]').textContent(), '‹ Results');
        await screenshot('mobile-direct-add');
        await frame.locator('[data-back]').click();
        assert.equal(await frame.locator('[data-view="results"]').isVisible(), true);
        await frame.locator('[data-open]').click();
        for (const condition of ['unsupported', 'error']) {
          await state({ condition });
          assert.equal(await frame.locator('[data-primary]').isDisabled(), condition === 'unsupported');
          await screenshot(`mobile-${condition}`);
        }
        await frame.locator('[data-retry]').click();
        assert.equal(await frame.locator('[data-warning]').isVisible(), false);
        await state({ longSynopsis: true, largeText: true });
        await screenshot('mobile-long-text');
        await page.setViewportSize({ width: 320, height: 1900 });
        assert.equal(await root.evaluate(e => e.scrollWidth <= e.clientWidth), true);
        await state({ longSynopsis: false, largeText: false, condition: 'owned' });
        assert.equal(await frame.locator('[data-primary]').textContent(), 'Open in library');
        await frame.locator('[data-primary]').click();
        assert.equal(await frame.locator('[data-view="library"]').isVisible(), true);
      }
      await frame.evaluate(() => { document.documentElement.style.colorScheme = 'light'; });
      assert.equal(await root.evaluate(e => e.scrollWidth <= e.clientWidth), true);
    }
    assert.deepEqual(errors, []);
    console.log('Mockup checks passed: interactions, option retention, states, responsive bounds; screenshots refreshed.');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
