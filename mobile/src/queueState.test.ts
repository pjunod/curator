import { describe, expect, it } from 'vitest'
import { queueBadge, queueSettled } from './queueState'

describe('queueBadge', () => {
  it('never shows a Runner hold as an error', () => {
    expect(queueBadge({ state: 'held', error: 'waiting for disk' })).toEqual({ label: 'held by Runner', tone: 'neutral' })
    expect(queueBadge({ state: 'held', error: 'waiting for disk', stale: true })).toEqual({ label: 'held (stale)', tone: 'warning' })
  })
  it('marks a failure Curator concluded itself as provisional', () => {
    expect(queueBadge({ state: 'failed', provisional: true, error: 'lost' })).toEqual({ label: 'failed (provisional)', tone: 'error' })
    expect(queueBadge({ state: 'failed', error: 'fetch' })).toEqual({ label: 'failed', tone: 'error' })
  })
  it('treats notified like imported', () => {
    expect(queueBadge({ state: 'notified' })).toEqual({ label: 'notified', tone: 'ok' })
    expect(queueSettled('notified')).toBe(true)
    expect(queueSettled('submitting')).toBe(false)
  })
  it('prefers the live stage label', () => {
    expect(queueBadge({ state: 'downloading', stage: 'verifying' })).toEqual({ label: 'verifying', tone: 'neutral' })
  })
})
