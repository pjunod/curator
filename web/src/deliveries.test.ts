import { describe, expect, it } from 'vitest'
import { retryDelivery, retryFailedDeliveries, testPassed } from './api'
import { statusLabel, summary } from './pages/NotifierDeliveries'
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

// A Cinema sub-delivery speaks Cinema's own words (pipeline contract v2 §4.2).
describe('sub-delivery summary', () => {
  const sub: Delivery = {
    ...base,
    status: 'pending',
    attempts: 3,
    attempt: 2,
    lastError: undefined,
    directory: '/media/tv/Show/Season 01',
    requestKey: 't-1-x/gen/0123456789abcdef/2',
    nextAt: Date.now() + 3_600_000,
  }
  it('shows visible with the item id once every expected file is placed', () => {
    const ok: Delivery = { ...sub, status: 'ok', visibleAt: 1, result: 'visible → plurx item 1201', nextAt: undefined }
    expect(summary(ok)).toBe('visible → plurx item 1201')
    expect(statusLabel(ok)).toEqual({ text: 'visible', pill: 'pill-ok' })
  })
  it("shows cinema's exact waiting_on while queued, and a warning after half an hour", () => {
    const queued: Delivery = { ...sub, remoteRequestId: 'sr-1', remoteState: 'queued', waitingOn: 'full_scan:3', result: 'queued as sr-1, waiting on full_scan:3' }
    expect(summary(queued)).toMatch(/^queued in cinema, waiting on full_scan:3, retrying in 1h$/)
    expect(statusLabel(queued).text).toBe('pending')
    const late: Delivery = { ...queued, warning: true }
    expect(summary(late)).toMatch(/^warning: queued in cinema, waiting on full_scan:3/)
    expect(statusLabel(late)).toEqual({ text: 'warning', pill: 'pill-warning' })
  })
  it("shows the file cinema did not place and cinema's reason", () => {
    const skipped: Delivery = { ...sub, result: 'warning: expected /media/tv/Show/Season 01/e02.mkv not placed (unsupported extension)' }
    expect(summary(skipped)).toMatch(/^warning: expected \/media\/tv\/Show\/Season 01\/e02\.mkv not placed \(unsupported extension\), retrying in 1h$/)
  })
  it('says an old cinema accepted the scan but reports nothing back', () => {
    const old: Delivery = { ...sub, status: 'ok', result: 'queued (cinema does not report results)' }
    expect(summary(old)).toBe('queued (cinema does not report results)')
    expect(statusLabel(old).text).toBe('ok')
  })
  it('marks a retry still failing after half an hour as a warning', () => {
    const down: Delivery = { ...sub, warning: true, lastError: 'cannot reach plurx: connection refused' }
    expect(summary(down)).toMatch(/^warning: attempt 3 failed: cannot reach plurx/)
  })
})

describe('notifier test result', () => {
  it('says delivered, or what the pass still has to say', () => {
    expect(testPassed(undefined)).toBe('✓ delivered')
    expect(testPassed({ message: 'connected; checking the library paths needs a newer cinema' })).toBe(
      '✓ connected; checking the library paths needs a newer cinema',
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
