// Durations Activity shows on a row: how long a hold has left, and how long
// an awaiting_import row has waited for a person (the one state whose exit is
// a person, pipeline contract section 3.1, so it has no deadline).

export function fmtDuration(ms: number): string {
  if (ms <= 0) return 'now'
  const m = Math.round(ms / 60000)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  return h < 48 ? `${h}h ${m % 60}m` : `${Math.floor(h / 24)}d ${h % 24}h`
}

export function awaitingText(stateEnteredAt: string | undefined, now = Date.now()): string {
  const since = stateEnteredAt ? new Date(stateEnteredAt).getTime() : 0
  return since > 0 && Number.isFinite(since) ? `Waiting for your approval for ${fmtDuration(now - since)}` : 'Waiting for your approval'
}
