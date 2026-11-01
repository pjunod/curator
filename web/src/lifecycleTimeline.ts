import type { DeliveryLifecycleReceipt, RunnerLifecycleInterval } from './api'

export interface TimelineRow {
  id: string
  stage: string
  startedAtMs?: number
  workMs?: number
  state: string
  error?: string
  detailOnly?: boolean
  monotonic?: [number, number]
}
export interface TimelineLane {
  id: string
  label: string
  clockDomain: string
  rows: TimelineRow[]
  knownWorkMs?: number
  overlapMs?: number
  unknownIntervals: number
}
const finite = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
const timestamp = (value?: string) => value && !value.startsWith('0001') && Number.isFinite(Date.parse(value)) ? Date.parse(value) : undefined

// Only explicit monotonic endpoints in one boot clock can establish overlap.
// Wall timestamps are plotting anchors and never fill missing measurements.
export function monotonicWork(intervals: RunnerLifecycleInterval[]) {
  const complete = intervals.filter((interval) => interval.state === 'completed' && finite(interval.startMonotonicMs) && finite(interval.endMonotonicMs) && finite(interval.durationMs) && interval.endMonotonicMs >= interval.startMonotonicMs && interval.durationMs === interval.endMonotonicMs - interval.startMonotonicMs)
  const ordered = complete.map((interval) => [interval.startMonotonicMs!, interval.endMonotonicMs!] as const).sort((a, b) => a[0] - b[0])
  let total = 0
  let end: number | undefined
  for (const [start, finish] of ordered) {
    total += end === undefined ? finish - start : Math.max(0, finish - Math.max(start, end))
    end = Math.max(end ?? finish, finish)
  }
  const measured = complete.reduce((sum, interval) => sum + interval.durationMs!, 0)
  return { knownWorkMs: complete.length ? total : undefined, overlapMs: measured - total, unknownIntervals: intervals.length - complete.length }
}

export function lifecycleTimeline(receipt: DeliveryLifecycleReceipt): TimelineLane[] {
  const lanes: TimelineLane[] = []
  const clocks = new Map<string, RunnerLifecycleInterval[]>()
  for (const interval of (receipt.lifecycleTiming?.version === 1 ? receipt.lifecycleTiming.intervals : [])) {
    // Node identity participates even if a provider accidentally reuses a
    // clock label; work on different nodes must never share a subtotal.
    const key = JSON.stringify([interval.nodeId, interval.clockDomain])
    clocks.set(key, [...(clocks.get(key) ?? []), interval])
  }
  for (const [key, intervals] of clocks) {
    const first = intervals[0]
    lanes.push({ id: key, label: `Runner · ${first.nodeId}`, clockDomain: first.clockDomain, ...monotonicWork(intervals), rows: intervals.map((interval, index) => ({
      id: `${key}:${index}`, stage: interval.stage.replaceAll('_', ' '), startedAtMs: finite(interval.startUnixMs) && interval.startUnixMs > 0 ? interval.startUnixMs : undefined,
      workMs: finite(interval.durationMs) ? interval.durationMs : undefined, state: interval.state,
      monotonic: finite(interval.startMonotonicMs) && finite(interval.endMonotonicMs) ? [interval.startMonotonicMs, interval.endMonotonicMs] : undefined,
    })) })
  }
  if (receipt.runnerStages) {
    lanes.push({ id: 'runner-post-details', label: `Processing details · ${receipt.runnerStages.nodeId}`, clockDomain: receipt.runnerStages.clockDomain, unknownIntervals: 0,
      rows: receipt.runnerStages.spans.map((stage, index) => ({ id: `post:${index}`, stage: stage.stage.replaceAll('_', ' '), startedAtMs: stage.started_at_unix > 0 ? stage.started_at_unix * 1000 : undefined,
        workMs: finite(stage.ms) ? stage.ms : undefined, state: finite(stage.ms) ? 'completed' : 'interrupted; duration unknown', detailOnly: true })) })
  }
  lanes.push({ id: 'publication', label: 'Publication and handoff', clockDomain: `Runner publication · ${receipt.runnerInstanceId || 'clock not recorded'}`, unknownIntervals: 0,
    rows: [{ id: 'publication-ready', stage: 'READY publication', startedAtMs: timestamp(receipt.readyAt), state: receipt.acceptance === 'accepted' ? 'Curator acceptance recorded' : `acceptance ${receipt.acceptance}`, detailOnly: true }] })
  if (receipt.attemptHistory?.length) {
    lanes.push({ id: 'curator', label: 'Curator import and completed-file cleanup', clockDomain: 'Curator wall-clock observations', unknownIntervals: 0,
      rows: receipt.attemptHistory.map((attempt) => {
        const start = timestamp(attempt.startedAt), finish = timestamp(attempt.finishedAt)
        return { id: `curator:${attempt.id}`, stage: attempt.stage.replaceAll('_', ' '), startedAtMs: start,
          // These existing attempt spans are wall-clock observations; keep
          // their observed length separate from monotonic Runner work totals.
          workMs: start !== undefined && finish !== undefined && finish >= start ? finish - start : undefined,
          state: finish === undefined ? 'running or interrupted; duration unknown' : attempt.result, error: attempt.error, detailOnly: true }
      }) })
  }
  const cleanupAttempts = receipt.sourceCleanup?.attemptHistory ?? []
  if (cleanupAttempts.length) {
    lanes.push({ id: 'runner-cleanup', label: 'Runner processing-source cleanup', clockDomain: 'Runner cleanup clock · separate from worker snapshots', unknownIntervals: 0,
      rows: cleanupAttempts.map((attempt, index) => ({ id: `source:${index}`, stage: `source cleanup · attempt ${attempt.attempt}`, startedAtMs: finite(attempt.startedAtMs) ? attempt.startedAtMs : undefined,
        workMs: finite(attempt.workMs) ? attempt.workMs : undefined, state: attempt.outcome, error: attempt.error ?? undefined, detailOnly: true })) })
  }
  return lanes
}
