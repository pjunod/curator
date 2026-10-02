import { expect, test } from '@playwright/test'

test.use({ serviceWorkers: 'block' })

for (const width of [390, 1280]) {
  for (const count of [0, 3, 4, 30]) {
    test(`alternate names disclosure with ${count} names at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 844 })
      let aliases = Array.from({ length: count }, (_, i) => ({
        id: i + 1, title: `Alternate title ${i + 1}`, role: 'alternate', source: 'tmdb',
      }))
      const item = () => ({
        id: 9876, kind: 'series', title: 'American Horror Story', year: 2011,
        monitored: true, path: '', genres: [], ratings: [], ids: {},
        seasons: [], files: [], copies: [], downloadPriority: 0, aliases,
      })
      await page.route('**/api/v1/library/9876', route => route.fulfill({ json: item() }))
      await page.route('**/api/v1/library/9876/aliases', async route => {
        const { title } = route.request().postDataJSON()
        const alias = { id: 100, title, role: 'manual', source: 'manual' }
        aliases = [...aliases, alias]
        await route.fulfill({ json: alias })
      })
      await page.route('**/api/v1/library/9876/aliases/100', async route => {
        aliases = aliases.filter(alias => alias.id !== 100)
        await route.fulfill({ status: 204 })
      })
      await page.goto('/library/9876')
      const identity = page.locator('section.panel').filter({ has: page.getByRole('heading', { name: 'Identity', exact: true }) })
      const disclosure = identity.locator('details')
      const summary = disclosure.locator('summary')
      const names = disclosure.locator('li')
      if (count === 0) {
        await expect(disclosure).toHaveCount(0)
      } else {
        await expect(summary).toHaveText(`Alternate names (${count})`)
        await expect(disclosure).toHaveJSProperty('open', count <= 3)
        await expect(names).toHaveCount(count)
        if (count > 3) await expect(names.first()).toBeHidden()
        // Keyboard activation remains native and keeps focus on the control.
        await summary.focus()
        if (count <= 3) await summary.press('Enter')
        await expect(disclosure).toHaveJSProperty('open', false)
        await summary.press('Space')
        await expect(disclosure).toHaveJSProperty('open', true)
        await expect(names.first()).toBeVisible()
        await expect(summary).toBeFocused()
        await expect(summary).toBeInViewport({ ratio: 1 })
        await expect(names.first()).toBeInViewport({ ratio: 1 })
        await summary.click()
        await expect(disclosure).toHaveJSProperty('open', false)
      }

      // Mutation-driven query refreshes must preserve the user's collapsed
      // choice, including when the count crosses the three-name threshold.
      await identity.getByRole('textbox', { name: 'Manual title alias' }).fill('My release title')
      await identity.getByRole('button', { name: 'Add alias', exact: true }).click()
      await expect(identity.getByText('Alias added.', { exact: true })).toBeInViewport({ ratio: 1 })
      await expect(summary).toHaveText(`Alternate names (${count + 1})`)
      await expect(disclosure).toHaveJSProperty('open', count === 0)
      if (count > 0) await summary.click()
      const manual = names.filter({ hasText: 'My release title' })
      await expect(manual).toBeVisible()
      await manual.getByRole('button', { name: 'Remove', exact: true }).click()
      await expect(manual).toHaveCount(0)
      if (count > 0) {
        await expect(summary).toHaveText(`Alternate names (${count})`)
        await expect(disclosure).toHaveJSProperty('open', true)
        // A fresh page visit uses the size-based default again.
        await page.reload()
        await expect(disclosure).toHaveJSProperty('open', count <= 3)
      } else {
        await expect(disclosure).toHaveCount(0)
      }
    })
  }
}
