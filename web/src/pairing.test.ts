import { describe, expect, it } from 'vitest'
import { buildPairingCode } from './pairing'

describe('buildPairingCode', () => {
  it('encodes the server and key without exposing them as URL structure', () => {
    const code = buildPairingCode(' http://[fd12:3456::7]:7676 ', ' key&value ')
    const parsed = new URL(code)

    expect(parsed.protocol).toBe('monarr:')
    expect(parsed.hostname).toBe('pair')
    expect(parsed.searchParams.get('v')).toBe('1')
    expect(parsed.searchParams.get('server')).toBe('http://[fd12:3456::7]:7676')
    expect(parsed.searchParams.get('key')).toBe('key&value')
  })
})
