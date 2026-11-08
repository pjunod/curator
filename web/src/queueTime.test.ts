import { describe, expect, it } from 'vitest'
import { awaitingText, fmtDuration } from './queueTime'

describe('awaitingText', () => {
  const now = Date.parse('2026-11-07T12:00:00Z')
  it('shows how long an awaiting_import row has waited', () => {
    expect(awaitingText('2026-11-07T09:30:00Z', now)).toBe('Waiting for your approval for 2h 30m')
    expect(awaitingText('2026-11-04T12:00:00Z', now)).toBe('Waiting for your approval for 3d 0h')
  })
  it('says nothing it does not know', () => {
    expect(awaitingText(undefined, now)).toBe('Waiting for your approval')
    expect(awaitingText('not a date', now)).toBe('Waiting for your approval')
  })
})

describe('fmtDuration', () => {
  it('rounds to minutes and caps the units', () => {
    expect(fmtDuration(0)).toBe('now')
    expect(fmtDuration(59 * 60000)).toBe('59m')
    expect(fmtDuration(49 * 3600000)).toBe('2d 1h')
  })
})
