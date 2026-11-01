import { describe, expect, it } from 'vitest'
import type { DeliveryLifecycleReceipt, RunnerLifecycleInterval } from './api'
import { lifecycleTimeline, monotonicWork } from './lifecycleTimeline'

const interval = (start: number, end: number, overrides: Partial<RunnerLifecycleInterval> = {}): RunnerLifecycleInterval => ({ stage: 'download', nodeId: 'worker', clockDomain: 'worker:boot-1', startUnixMs: 1700000000000 + start, endUnixMs: 1700000000000 + end, startMonotonicMs: start, endMonotonicMs: end, durationMs: end - start, state: 'completed', ...overrides })
const receipt = (overrides: Partial<DeliveryLifecycleReceipt> = {}) => ({ receiptId: 1, runnerInstanceId: 'runner', readyAt: '2026-11-01T10:00:00Z', acceptance: 'accepted', attemptHistory: [], ...overrides }) as DeliveryLifecycleReceipt

describe('joined lifecycle timing', () => {
  it('unions overlapping measured work only within one node boot clock', () => {
    const result = monotonicWork([interval(0, 100), interval(50, 150), interval(20, 30)])
    expect(result).toEqual({ knownWorkMs: 150, overlapMs: 60, unknownIntervals: 0 })
  })
  it('does not use wall-clock movement or missing durations to invent work', () => {
    const result = monotonicWork([
      interval(0, 100, { startUnixMs: 1000, endUnixMs: 10 }),
      interval(100, 200, { endMonotonicMs: undefined, durationMs: undefined, state: 'interrupted' }),
      interval(200, 300, { state: 'running' }),
      interval(300, 400, { durationMs: 99 }),
    ])
    expect(result).toEqual({ knownWorkMs: 100, overlapMs: 0, unknownIntervals: 3 })
  })
  it('leaves invalid or incomplete major row measurements unknown', () => {
    const lanes = lifecycleTimeline(receipt({ lifecycleTiming: { version: 1, intervals: [interval(0, 100), interval(100, 200, { endMonotonicMs: undefined }), interval(200, 300, { state: 'interrupted' }), interval(300, 400, { durationMs: 99 })] } }))
    expect(lanes[0].rows.map((row) => row.workMs)).toEqual([100, undefined, undefined, undefined])
  })
  it('keeps nodes and boot domains separate and excludes POST detail from major totals', () => {
    const lanes = lifecycleTimeline(receipt({ lifecycleTiming: { version: 1, intervals: [interval(0, 100), interval(50, 150, { stage: 'post_processing' }), interval(0, 40, { nodeId: 'other-node' }), interval(0, 30, { clockDomain: 'worker:boot-2' })] }, runnerStages: { nodeId: 'worker', clockDomain: 'worker:post', spans: [{ stage: 'unpack', started_at_unix: 1700000000, ms: 90 }] } }))
    expect(lanes.filter((lane) => lane.knownWorkMs !== undefined).map((lane) => lane.knownWorkMs)).toEqual([150, 40, 30])
    expect(lanes.find((lane) => lane.id === 'runner-post-details')?.knownWorkMs).toBeUndefined()
  })
  it('joins handoff and cleanup observations while retaining interrupted and legacy unknowns', () => {
    const lanes = lifecycleTimeline(receipt({ readyAt: '0001-01-01T00:00:00Z', attemptHistory: [{ id: 1, stage: 'import', attempt: 1, startedAt: '2026-11-01T10:00:00Z', finishedAt: '0001-01-01T00:00:00Z', result: 'running', error: '' }], sourceCleanup: { attemptHistory: [{ attempt: 1, startedAtMs: 1700000000000, workMs: null, waitMs: 30000, outcome: 'blocked' }] } }))
    expect(lanes.map((lane) => lane.id)).toEqual(['publication', 'curator', 'runner-cleanup'])
    expect(lanes[0].rows[0].startedAtMs).toBeUndefined()
    expect(lanes[1].rows[0].workMs).toBeUndefined()
    expect(lanes[1].rows[0].state).toContain('interrupted')
    expect(lanes[2].rows[0].workMs).toBeUndefined()
    expect(lanes.every((lane) => lane.knownWorkMs === undefined)).toBe(true)
  })
})
