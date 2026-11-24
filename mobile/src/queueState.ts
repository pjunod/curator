import type { QueueItem } from './types'

export type QueueTone = 'ok' | 'warning' | 'error' | 'neutral'

type BadgeInput = Pick<QueueItem, 'state' | 'stage' | 'error' | 'stale' | 'provisional'>

// queueBadge labels an Activity row. A Runner hold is waiting, not failing:
// its message (in error) is what Runner waits on, so it is never red. Held
// past Runner's own deadline is "stale" (Curator is reconciling), and a
// failure Curator concluded itself is marked provisional.
export function queueBadge(item: BadgeInput): { label: string; tone: QueueTone } {
  if (item.state === 'held') return { label: item.stale ? 'held (stale)' : 'held by Runner', tone: item.stale ? 'warning' : 'neutral' }
  const label = item.stage || (item.state === 'failed' && item.provisional ? 'failed (provisional)' : item.state)
  if (item.error || item.state === 'failed') return { label, tone: 'error' }
  if (item.state === 'imported' || item.state === 'notified') return { label, tone: 'ok' }
  return { label, tone: 'neutral' }
}

// queueSettled is true once a row is past downloading for good.
export function queueSettled(state: string): boolean {
  return state === 'imported' || state === 'notified' || state === 'failed'
}
