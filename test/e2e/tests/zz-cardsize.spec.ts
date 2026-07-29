import { expect, test } from '@playwright/test'

// The poster size control (S / M / L), shared by the Library grid and the
// Discover strips through one `data-card-size` attribute on <html>.
//
// Measured rather than asserted on classes: the whole feature is a number,
// and a CSS custom property that stopped reaching one of the two grids would
// leave every class name and every attribute exactly as this test expects.
//
// The two surfaces consume the token differently, and the assertions say so.
// A strip card is exactly --card-w. A grid column is `minmax(--card-w, 1fr)`
// under auto-fill, so it is that value STRETCHED to divide the row evenly —
// at large the library card measured 247px against the token's 210. Pinning
// them equal is what a first draft of this test did, and it was the test that
// was wrong.
//
// zz- because it needs the library items and the TMDB key the earlier specs
// build; it adds nothing itself. Sorts before zz-discover and zz-umbrella.
test.describe.configure({ mode: 'serial' })

// Real cards only. A row that has not fetched yet renders skeletons under the
// same class, and measuring one races the swap: visible when asked, detached
// a moment later, which surfaces as a null box and a confusing failure.
const LIBRARY_CARD = '.poster-card'
const DISCOVER_CARD = '[data-list="tmdb-trending-movies"] .discover-card:not(.discover-skeleton)'

type Page = import('@playwright/test').Page

/** The resolved value of --card-w, in pixels — the one number this whole
 *  feature is. Reading it rather than hardcoding 110/150/210 keeps the test
 *  about the wiring instead of about the design's current taste. */
async function token(page: Page): Promise<number> {
  const raw = await page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue('--card-w'),
  )
  const px = parseFloat(raw)
  expect(px, `--card-w resolved to ${JSON.stringify(raw)}`).toBeGreaterThan(0)
  return px
}

async function cardWidth(page: Page, selector: string): Promise<number> {
  const card = page.locator(selector).first()
  await expect(card).toBeVisible()
  const box = await card.boundingBox()
  if (!box) throw new Error(`no box for ${selector}`)
  return box.width
}

function pick(page: Page, size: 'Small' | 'Medium' | 'Large') {
  return page
    .getByRole('group', { name: 'Poster size' })
    .getByRole('button', { name: new RegExp(size) })
    .click()
}

test.beforeAll(async ({ request }) => {
  await request.put('/api/v1/settings', { data: { tmdbApiKey: 'e2e-test-key' } })
})

test('the library grid follows the token, and unset means unchanged', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 900 })
  await page.goto('/')

  // Nothing chosen yet: medium, which is the 150px the grid has always been.
  // An install that never touches this control looks exactly as it did.
  expect(await token(page)).toBe(150)
  const medium = await cardWidth(page, LIBRARY_CARD)
  expect(medium).toBeGreaterThanOrEqual(150)

  await pick(page, 'Small')
  const smallToken = await token(page)
  const small = await cardWidth(page, LIBRARY_CARD)
  expect(smallToken).toBeLessThan(150)
  expect(small).toBeGreaterThanOrEqual(smallToken)
  expect(small).toBeLessThan(medium)

  await pick(page, 'Large')
  const largeToken = await token(page)
  const large = await cardWidth(page, LIBRARY_CARD)
  expect(largeToken).toBeGreaterThan(150)
  expect(large).toBeGreaterThanOrEqual(largeToken)
  expect(large).toBeGreaterThan(medium)

  // The point of the control: strictly more titles per row at S than at L.
  await pick(page, 'Small')
  expect(await page.locator(LIBRARY_CARD).count()).toBeGreaterThan(0)
  expect(await cardWidth(page, LIBRARY_CARD)).toBe(small)
})

test('the choice survives a reload and reaches Discover too', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 900 })
  await page.goto('/')
  await pick(page, 'Large')
  const large = await cardWidth(page, LIBRARY_CARD)

  await page.reload()
  expect(await cardWidth(page, LIBRARY_CARD)).toBe(large)
  // The pre-paint script in index.html is what makes that true before the
  // grid draws; the picker reads the attribute back so its highlight agrees.
  expect(await page.getAttribute('html', 'data-card-size')).toBe('l')
  await expect(
    page.getByRole('group', { name: 'Poster size' }).getByRole('button', { name: /Large/ }),
  ).toHaveAttribute('aria-pressed', 'true')

  // One preference, both pages. Discover was never told anything — it reads
  // the same custom property, and a strip card is exactly that wide.
  await page.goto('/discover')
  expect(await cardWidth(page, DISCOVER_CARD)).toBe(await token(page))
  const largeStrip = await cardWidth(page, DISCOVER_CARD)

  await pick(page, 'Small')
  const smallStrip = await cardWidth(page, DISCOVER_CARD)
  expect(smallStrip).toBe(await token(page))
  expect(smallStrip).toBeLessThan(largeStrip)

  // ...and back the other way: the Library page picks up a change made on
  // Discover, without either page knowing about the other.
  await page.goto('/')
  expect(await cardWidth(page, LIBRARY_CARD)).toBeLessThan(large)

  // Leave the suite on the default for whatever runs next.
  await pick(page, 'Medium')
  expect(await token(page)).toBe(150)
})
