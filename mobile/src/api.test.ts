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

  it('sends item edits to the detail endpoint', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }))
    vi.stubGlobal('fetch', fetch)
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: '' })

    await client.updateLibraryItem(42, {
      monitored: false,
      qualityProfileId: 7,
      downloadPriority: 100,
      rootFolderId: 3,
      path: '/media/movies/Arrival (2016)',
    })

    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://monarr.local:7676/api/v1/library/42')
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(String(init.body))).toEqual({
      monitored: false,
      qualityProfileId: 7,
      downloadPriority: 100,
      rootFolderId: 3,
      path: '/media/movies/Arrival (2016)',
    })
  })

  it('updates monitoring for a series season', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }))
    vi.stubGlobal('fetch', fetch)
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: '' })

    await client.setSeasonMonitored(42, 3, false)

    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://monarr.local:7676/api/v1/library/42/seasons/3')
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(String(init.body))).toEqual({ monitored: false })
  })

  it('clears every failed Activity row through the bulk endpoint', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ cleared: 3 }), { status: 200 }))
    vi.stubGlobal('fetch', fetch)
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: '' })

    await expect(client.clearFailedQueue()).resolves.toEqual({ cleared: 3 })

    expect(fetch).toHaveBeenCalledWith(
      'http://monarr.local:7676/api/v1/queue/failed',
      expect.objectContaining({ method: 'DELETE', body: undefined }),
    )
  })

  it('adds, edits, and removes additional quality copies', async () => {
    const fetch = vi.fn().mockImplementation(() => Promise.resolve(new Response('{}', { status: 200 })))
    vi.stubGlobal('fetch', fetch)
    const client = new MonarrClient({ baseUrl: 'http://monarr.local:7676', apiKey: '' })

    await client.addMediaCopy(42, { qualityProfileId: 5, name: 'Travel', monitored: true })
    await client.updateMediaCopy(42, 9, { qualityProfileId: 4, name: 'Tablet', monitored: false })
    await client.deleteMediaCopy(42, 9)

    expect(fetch).toHaveBeenNthCalledWith(
      1,
      'http://monarr.local:7676/api/v1/library/42/copies',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ qualityProfileId: 5, name: 'Travel', monitored: true }),
      }),
    )
    expect(fetch).toHaveBeenNthCalledWith(
      2,
      'http://monarr.local:7676/api/v1/library/42/copies/9',
      expect.objectContaining({
        method: 'PATCH',
        body: JSON.stringify({ qualityProfileId: 4, name: 'Tablet', monitored: false }),
      }),
    )
    expect(fetch).toHaveBeenNthCalledWith(
      3,
      'http://monarr.local:7676/api/v1/library/42/copies/9',
      expect.objectContaining({ method: 'DELETE', body: undefined }),
    )
  })
})
