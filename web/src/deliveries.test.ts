import { describe, expect, it } from 'vitest'
import { retryDelivery, retryFailedDeliveries } from './api'
import { summary } from './pages/NotifierDeliveries'
import type { Delivery } from './api'

// The delivery log is where "imported, but not in plurx" gets answered, and
// after the 2026-09-28 wedge (plurx refusing every scan for a day) it is also
// where the answer gets acted on: a failed row is retried from the row.

const base: Delivery = {
  id: 7,
  notifierId: 1,
  event: 'import',
  attempts: 4,
  status: 'failed',
  lastError: 'plurx returned 503: the durable queue refused this request',
  createdAt: 0,
  updatedAt: 0,
}

describe('delivery summary', () => {
  it('reads the failure off a failed row', () => {
    expect(summary(base)).toBe('plurx returned 503: the durable queue refused this request')
  })
  it('tells a requeued row apart from a row mid-schedule', () => {
    const requeued: Delivery = { ...base, status: 'pending', attempts: 0, nextAt: Date.now() }
    expect(summary(requeued)).toMatch(/^retry queued, retrying in \d+s — last failure: plurx returned 503/)
    const midway: Delivery = { ...base, status: 'pending', attempts: 2, nextAt: Date.now() + 30_000 }
    expect(summary(midway)).toMatch(/^attempt 2 failed: plurx returned 503.*, retrying in 30s$/)
  })
  it('says queued for a fresh row and what the far side said for a delivered one', () => {
    expect(summary({ ...base, status: 'pending', attempts: 0, lastError: undefined })).toBe('queued')
    expect(summary({ ...base, status: 'ok', result: 'scanned → plurx item 1201' })).toBe(
      'scanned → plurx item 1201',
    )
  })
})

describe('retry endpoints', () => {
  it('post to the notifier-scoped routes and return what the server said', async () => {
    const orig = globalThis.fetch
    const seen: { url: string; method?: string }[] = []
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      seen.push({ url, method: init?.method })
      const body = url.endsWith('/deliveries/retry')
        ? { requeued: 3 }
        : { ...base, status: 'pending', attempts: 0, nextAt: 1 }
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }) as unknown as typeof fetch
    try {
      const one = await retryDelivery(1, 7)
      expect(one.status).toBe('pending')
      expect(one.attempts).toBe(0)
      const all = await retryFailedDeliveries(1)
      expect(all.requeued).toBe(3)
      expect(seen).toEqual([
        { url: '/api/v1/notifiers/1/deliveries/7/retry', method: 'POST' },
        { url: '/api/v1/notifiers/1/deliveries/retry', method: 'POST' },
      ])
    } finally {
      globalThis.fetch = orig
    }
  })
})
