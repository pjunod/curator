import { describe, expect, it } from 'vitest'
import { fmtDuration, fmtInterval, fmtRelative } from './api'

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
