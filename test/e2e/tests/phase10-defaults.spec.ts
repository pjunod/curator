import { expect, test } from '@playwright/test'

// Two things a settings screen owes you: it must be possible to change the
// default profile at all, and the add form must say which one it is about to
// use. Both were missing — the default was a constant in the server and the
// add form's profile picker said only "(default)".
test.describe.configure({ mode: 'serial' })

test('the default profile for each kind is visible and changeable', async ({ page, request }) => {
  await page.goto('/settings')
  const panel = page.locator('#profiles')
  await expect(panel).toBeVisible()

  // Out of the box: films and television start on 1080p, with independent
  // Ebook and Audiobook defaults for the two book families.
  const defaults = panel.getByTestId('profile-defaults')
  await expect(defaults).toBeVisible()
  const movies = defaults.getByLabel('Default profile for Movies')
  const series = defaults.getByLabel('Default profile for Series')
  const books = defaults.getByLabel('Default profile for Ebooks')
  const audiobooks = defaults.getByLabel('Default profile for Audiobooks')
  await expect(movies).toHaveValue('1')
  await expect(series).toHaveValue('1')
  await expect(books).toHaveValue('4')
  await expect(audiobooks).toHaveValue('5')

  // The table says which kinds start on each profile, so the answer is visible
  // without scrolling to the picker.
  const row1080 = panel.locator('tr', { hasText: '1080p' }).first()
  await expect(row1080).toContainText('Movies')

  // Change films to 4K. It sticks on the server, not just in the select.
  await movies.selectOption({ label: '4K' })
  await expect.poll(async () => {
    const s = await (await request.get('/api/v1/settings')).json()
    return s.defaultProfiles?.movie
  }).toBe(3)

  // And television is untouched: the whole reason this is per kind.
  await expect(series).toHaveValue('1')

  // A book cannot be offered a video profile at all — format families never
  // compete, so the option must not exist rather than fail on save.
  await expect(books.locator('option')).toHaveCount(1)
  await expect(audiobooks.locator('option')).toHaveCount(1)
  await expect(books.locator('option', { hasText: '4K' })).toHaveCount(0)

  // Put it back so later specs see the library they expect.
  await movies.selectOption({ label: '1080p' })
  await expect.poll(async () => {
    const s = await (await request.get('/api/v1/settings')).json()
    return s.defaultProfiles?.movie
  }).toBe(1)
})

test('a profile that is a default cannot be deleted out from under new items', async ({ page }) => {
  await page.goto('/settings')
  const panel = page.locator('#profiles')
  const row = panel.locator('tr', { hasText: '1080p' }).first()
  const del = row.getByRole('button', { name: 'Delete' })
  await expect(del).toBeDisabled()
})

// The add form must name the default rather than gesture at one. "(default)"
// alone is a promise with no content — you cannot tell what you are agreeing
// to until after the item exists.
test('the add form names the default profile it will use', async ({ page }) => {
  await page.goto('/add')
  // The option text, not its visibility: a closed <select> renders no visible
  // options, and opening one to read a label is theatre.
  await expect(page.getByLabel('Quality profile')).toContainText('Default — 1080p')
})

// The "adding keeps you on the add screen" behaviour itself is asserted in
// library.spec.ts, where the seeded provider actually has something to add.
// Adding a second movie here purely to test the flow would leave a title in
// the shared library that every later spec has to account for.
