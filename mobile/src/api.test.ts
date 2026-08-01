import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, MonarrClient } from './api'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('MonarrClient', () => {
  it('targets the native API and authenticates with the saved key', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ appName: 'Monarr' }), { status: 200 }))
    vi.stubGlobal('fetch', fetch)
    const client = new MonarrClient({ baseUrl: 'https://media.example.com/monarr', apiKey: 'secret' })

    await client.getStatus()

    expect(fetch).toHaveBeenCalledOnce()
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('https://media.example.com/monarr/api/v1/system/status')
    expect(new Headers(init.headers).get('X-Api-Key')).toBe('secret')
  })

  it('turns server messages into status-aware errors', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ message: 'no such item' }), { status: 404 })))
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: '' })

    await expect(client.getLibraryItem(99)).rejects.toEqual(new ApiError('no such item', 404))
  })

  it('gives authentication failures an actionable mobile message', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{}', { status: 401 })))
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: 'wrong' })

    await expect(client.getHealth()).rejects.toThrow('Settings → Security')
  })
})
