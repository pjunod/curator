import { expect, test } from '@playwright/test'

// ADR 0031 §4-5 in the profile editor: every control that removes itself
// hands focus to what took its place (CLAUDE.md §5), and the device panel
// opens in view, closes on Escape, and returns focus to its trigger — also
// after "Use this device's order" turns the preference on.
test.use({ serviceWorkers: 'block' })

const device = {
  source: 'cinema', id: 'a1b2c3d4', device: 'Living room TV', client: 'plurx tvOS', user: 'paul',
  fel: false, dolbyVision: true, profile5: true, hdr10plus: false,
  hdrOrder: ['dv-p7-fel', 'dv-p7-mel', 'dv-p8', 'hdr10'],
  hdrTies: [['dv-p7-fel', 'dv-p7-mel', 'dv-p8']],
  hdrReject: [],
}

test('the HDR editor keeps focus on the control that replaced the one pressed', async ({ page }) => {
  await page.route('**/api/v1/hdr/devices', route => route.fulfill({ json: { devices: [device], errors: [] } }))
  await page.goto('/settings')
  const panel = page.locator('#profiles')
  await panel.getByRole('button', { name: 'New profile' }).click()
  const hdr = panel.getByRole('group', { name: 'HDR and Dolby Vision' })

  await hdr.getByRole('button', { name: 'Rank by HDR and Dolby Vision' }).click()
  await expect(hdr.getByRole('button', { name: 'Remove DV P7 FEL' })).toBeFocused()
  await hdr.getByRole('button', { name: 'Remove DV P7 FEL' }).click()
  await expect(hdr.getByRole('button', { name: 'Remove DV P7 MEL' })).toBeFocused()
  await hdr.getByRole('button', { name: 'Remove HDR preference' }).click()
  const enable = hdr.getByRole('button', { name: 'Rank by HDR and Dolby Vision' })
  await expect(enable).toBeFocused()

  const trigger = hdr.getByRole('button', { name: 'Derive order from device' })
  await trigger.click()
  const devices = hdr.getByTestId('hdr-devices')
  await expect(devices).toBeInViewport()
  await expect(devices).toBeFocused()
  await expect(devices).toContainText('Living room TV')
  await page.keyboard.press('Escape')
  await expect(devices).toHaveCount(0)
  await expect(trigger).toBeFocused()

  await trigger.click()
  await devices.getByRole('button', { name: "Use this device's order" }).click()
  await expect(devices).toHaveCount(0)
  await expect(trigger).toBeFocused()
  await expect(hdr.getByTestId('hdr-order')).toContainText('DV P7 FEL')
})
