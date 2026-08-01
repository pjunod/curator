import { describe, expect, it } from 'vitest'
import { normalizeServerUrl, sanitizeConnection } from './connection'

describe('normalizeServerUrl', () => {
  it('adds the homelab-friendly http scheme', () => {
    expect(normalizeServerUrl('192.168.1.20:7676')).toBe('http://192.168.1.20:7676')
  })

  it('adds the Monarr default port when none is provided', () => {
    expect(normalizeServerUrl('192.168.1.20')).toBe('http://192.168.1.20:7676')
    expect(normalizeServerUrl('http://monarr.local')).toBe('http://monarr.local:7676')
    expect(normalizeServerUrl('https://media.example.com/monarr')).toBe(
      'https://media.example.com/monarr',
    )
  })

  it('preserves explicitly selected ports, including protocol defaults', () => {
    expect(normalizeServerUrl('http://monarr.local:8080')).toBe('http://monarr.local:8080')
    expect(normalizeServerUrl('http://monarr.local:80')).toBe('http://monarr.local')
    expect(normalizeServerUrl('https://media.example.com:443/monarr')).toBe(
      'https://media.example.com/monarr',
    )
  })

  it('adds the default port to a bracketed IPv6 host', () => {
    expect(normalizeServerUrl('[2001:db8::1]')).toBe('http://[2001:db8::1]:7676')
    expect(normalizeServerUrl('http://[2001:db8::1]:8080')).toBe('http://[2001:db8::1]:8080')
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
