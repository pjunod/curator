import { describe, expect, it } from 'vitest'
import { displayAppName, PUBLIC_APP_NAME } from './branding'

describe('displayAppName', () => {
  it('presents the legacy server identity as the public product name', () => {
    expect(displayAppName('Monarr')).toBe(PUBLIC_APP_NAME)
  })

  it('leaves names from other servers unchanged', () => {
    expect(displayAppName('Living Room Curator')).toBe('Living Room Curator')
  })
})
