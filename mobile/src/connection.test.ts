import { describe, expect, it } from 'vitest'
import { normalizeServerUrl, sanitizeConnection } from './connection'

describe('normalizeServerUrl', () => {
  it('adds the homelab-friendly http scheme', () => {
    expect(normalizeServerUrl('192.168.1.20:7676')).toBe('http://192.168.1.20:7676')
  })

  it('accepts a reverse proxy path and strips a pasted API suffix', () => {
    expect(normalizeServerUrl('https://media.example.com/monarr/api/v1/')).toBe(
      'https://media.example.com/monarr',
    )
  })

  it('rejects URLs that could hide credentials or change the target', () => {
    expect(() => normalizeServerUrl('https://user:pass@example.com')).toThrow('Remove credentials')
    expect(() => normalizeServerUrl('https://example.com?next=elsewhere')).toThrow('Remove credentials')
    expect(() => normalizeServerUrl('ftp://example.com')).toThrow('http:// or https://')
  })
})

describe('sanitizeConnection', () => {
  it('trims secrets without changing them', () => {
    expect(sanitizeConnection({ baseUrl: ' monarr.local:7676/ ', apiKey: ' key-123 ' })).toEqual({
      baseUrl: 'http://monarr.local:7676',
      apiKey: 'key-123',
    })
  })
})
