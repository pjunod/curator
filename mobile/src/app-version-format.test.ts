import { describe, expect, it } from 'vitest'
import { formatAppVersion } from './app-version-format'

describe('app version formatting', () => {
  it('includes the native build number when it is available', () => {
    expect(formatAppVersion('0.22.0', '11')).toBe('0.22.0 (11)')
    expect(formatAppVersion('0.22.0', 11)).toBe('0.22.0 (11)')
  })

  it('handles development and incomplete manifests', () => {
    expect(formatAppVersion('0.22.0', null)).toBe('0.22.0')
    expect(formatAppVersion(undefined, undefined)).toBe('Unknown')
  })
})
