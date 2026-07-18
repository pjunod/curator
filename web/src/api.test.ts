import { describe, expect, it } from 'vitest'
import { addIndexer, composeHostPort, fmtDuration, fmtInterval, fmtRelative, testIndexer } from './api'

describe('fmtDuration', () => {
  it('formats seconds', () => {
    expect(fmtDuration(0)).toBe('0s')
    expect(fmtDuration(59)).toBe('59s')
  })
  it('formats minutes and seconds', () => {
    expect(fmtDuration(61)).toBe('1m 1s')
    expect(fmtDuration(600)).toBe('10m 0s')
  })
  it('formats hours', () => {
    expect(fmtDuration(3661)).toBe('1h 1m 1s')
  })
  it('formats days without seconds', () => {
    expect(fmtDuration(90061)).toBe('1d 1h 1m')
  })
  it('clamps negatives and fractions', () => {
    expect(fmtDuration(-5)).toBe('0s')
    expect(fmtDuration(1.9)).toBe('1s')
  })
})

describe('fmtRelative', () => {
  const now = new Date('2026-07-17T12:00:00Z')
  it('returns a dash for missing timestamps', () => {
    expect(fmtRelative(undefined, now)).toBe('—')
  })
  it('formats past timestamps as ago', () => {
    expect(fmtRelative('2026-07-17T11:59:18Z', now)).toBe('42s ago')
    expect(fmtRelative('2026-07-17T10:59:00Z', now)).toBe('1h 1m 0s ago')
  })
  it('formats future timestamps as in', () => {
    expect(fmtRelative('2026-07-17T12:00:22Z', now)).toBe('in 22s')
  })
  it('treats sub-second differences as now', () => {
    expect(fmtRelative('2026-07-17T12:00:00.4Z', now)).toBe('now')
  })
})

describe('fmtInterval', () => {
  it('humanizes minutes', () => {
    expect(fmtInterval(60)).toBe('every minute')
    expect(fmtInterval(900)).toBe('every 15m')
  })
  it('humanizes hours', () => {
    expect(fmtInterval(3600)).toBe('every hour')
    expect(fmtInterval(7200)).toBe('every 2h')
  })
  it('falls back to seconds', () => {
    expect(fmtInterval(90)).toBe('every 90s')
  })
})

describe('send tolerates empty success bodies', () => {
  it('resolves on 200 with no body (the /test endpoints)', async () => {
    const orig = globalThis.fetch
    globalThis.fetch = (async () => new Response('', { status: 200 })) as typeof fetch
    try {
      await expect(
        testIndexer({ name: 'x', url: 'http://idx', protocol: 'usenet' }),
      ).resolves.toBeUndefined()
    } finally {
      globalThis.fetch = orig
    }
  })

  it('still surfaces server error messages from JSON bodies', async () => {
    const orig = globalThis.fetch
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ message: 'indexer unreachable' }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      })) as typeof fetch
    try {
      await expect(
        testIndexer({ name: 'x', url: 'http://idx', protocol: 'usenet' }),
      ).rejects.toThrow('indexer unreachable')
    } finally {
      globalThis.fetch = orig
    }
  })

  it('still parses real JSON success bodies', async () => {
    const orig = globalThis.fetch
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ id: 7, name: 'x', url: 'http://idx', protocol: 'usenet' }), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      })) as typeof fetch
    try {
      const created = await addIndexer({ name: 'x', url: 'http://idx', protocol: 'usenet' })
      expect(created.id).toBe(7)
    } finally {
      globalThis.fetch = orig
    }
  })
})

describe('composeHostPort', () => {
  it('appends the port to a bare host', () => {
    expect(composeHostPort('192.168.4.7', '6789')).toBe('192.168.4.7:6789')
  })
  it('respects an existing port', () => {
    expect(composeHostPort('192.168.4.7:9999', '6789')).toBe('192.168.4.7:9999')
    expect(composeHostPort('http://x.local:8080', '6789')).toBe('http://x.local:8080')
  })
  it('keeps schemes and paths intact', () => {
    expect(composeHostPort('http://nzbget.local', '6789')).toBe('http://nzbget.local:6789')
    expect(composeHostPort('https://box.example/sab', '8080')).toBe('https://box.example:8080/sab')
  })
  it('passes through when host or port is empty', () => {
    expect(composeHostPort('', '6789')).toBe('')
    expect(composeHostPort('192.168.4.7', '')).toBe('192.168.4.7')
  })
})
