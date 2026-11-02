import { afterEach, describe, expect, it, vi } from 'vitest'
import { acceptCompletedStorageChange, previewCompletedStorageChange } from './api'

afterEach(() => vi.unstubAllGlobals())

describe('storage change review authority', () => {
  it('preserves the exact expected/observed review token across preview and acceptance', async () => {
    const comparison = { root: '/completed/a', expectedIdentity: 'nfs4\u0000nas:/old', observedIdentity: 'nfs4\u0000nas:/new' }
    const review = { ...comparison, reviewToken: 'exact-custody-snapshot', expectedDescription: 'old', observedDescription: 'new', existingPayloads: 2, existingRequests: 3, inFlightRequests: 1, activeImports: 0 }
    const fetch = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify(review), { status: 200, headers: { 'content-type': 'application/json' } })).mockResolvedValueOnce(new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetch)
    const result = await previewCompletedStorageChange(comparison)
    expect(result).toEqual(review)
    await acceptCompletedStorageChange(result)
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/downloads/inventory/storage-change/preview')
    expect(fetch.mock.calls[1][0]).toBe('/api/v1/downloads/inventory/storage-change/accept')
    expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual(review)
  })
})
