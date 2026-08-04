import { afterEach, describe, expect, it, vi } from 'vitest'
import { parsePairingCode, probeMonarrServer, serviceBaseUrls } from './pairing'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('parsePairingCode', () => {
  it('accepts and normalizes a versioned Monarr pairing URL', () => {
    const code = 'monarr://pair?v=1&server=http%3A%2F%2F10.2.0.8%3A7676&key=secret'
    expect(parsePairingCode(code)).toEqual({
      baseUrl: 'http://10.2.0.8:7676',
      apiKey: 'secret',
    })
  })

  it('supports a bracketed IPv6 server address', () => {
    const server = encodeURIComponent('http://[fd12:3456:789a::8]:7676')
    expect(parsePairingCode(`monarr://pair?v=1&server=${server}&key=k`)).toEqual({
      baseUrl: 'http://[fd12:3456:789a::8]:7676',
      apiKey: 'k',
    })
  })

  it('rejects unrelated and unsupported codes', () => {
    expect(() => parsePairingCode('https://example.com')).toThrow('not a Curator pairing')
    expect(() => parsePairingCode('monarr://pair?v=2&server=monarr.local')).toThrow('unsupported')
  })
})

describe('serviceBaseUrls', () => {
  it('uses DNS-SD hostnames plus both IPv4 and IPv6 addresses', () => {
    expect(serviceBaseUrls({
      name: 'Monarr',
      host: 'media.local.',
      port: 8787,
      addresses: ['10.42.16.7', 'fd12:3456:789a::7'],
      txt: { host: 'server.local', path: '/' },
    })).toEqual([
      'http://server.local:8787',
      'http://media.local:8787',
      'http://10.42.16.7:8787',
      'http://[fd12:3456:789a::7]:8787',
    ])
  })

  it('uses the service hostname for scoped link-local IPv6 addresses', () => {
    expect(serviceBaseUrls({
      name: 'Monarr',
      host: 'Monarr on media',
      port: 7676,
      addresses: ['fe80::1234%en0'],
      txt: { host: 'media.local' },
    })).toEqual(['http://media.local:7676'])
  })
})

describe('probeMonarrServer', () => {
  it('accepts a Monarr status response and sends the entered key', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ appName: 'Monarr', version: '1.2.3' }), { status: 200 }))
    vi.stubGlobal('fetch', fetch)

    await expect(probeMonarrServer('Living room', 'http://[fd00::7]:7676', 'secret')).resolves.toMatchObject({
      name: 'Living room',
      baseUrl: 'http://[fd00::7]:7676',
      requiresApiKey: false,
    })
    const [, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(new Headers(init.headers).get('X-Api-Key')).toBe('secret')
  })

  it('recognizes Monarr authentication without accepting an unrelated 401', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ message: 'authentication required' }),
      { status: 401 },
    )))
    await expect(probeMonarrServer('Server', 'http://media.local:7676', '')).resolves.toMatchObject({
      requiresApiKey: true,
    })
  })
})
