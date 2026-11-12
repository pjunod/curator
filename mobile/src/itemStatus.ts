import type { ItemStatus, StatusAction, StatusDownload, StatusStage, StatusStageKey, StatusState } from './types'

// Display helpers for the item status panel; the server decides every state.

export const STAGE_NAMES: Record<StatusStageKey, string> = {
  searched: 'Searched',
  downloading: 'Downloading',
  processed: 'Processed',
  moved: 'Moved',
  cinema_notified: 'Cinema notified',
  in_cinema: 'In cinema',
}

export const STATE_ICON: Record<StatusState, string> = { done: '✓', active: '●', waiting: '◷', failed: '✕', todo: '○' }

export const ACTION_LABELS: Record<StatusAction, string> = {
  search: 'Search now',
  interactive_search: 'Interactive search',
  open_activity: 'Open in Activity',
  cancel_download: 'Cancel download',
  retry_import: 'Retry import',
  resend_cinema: 'Send to cinema again',
  confirm_cinema: 'Ask cinema to confirm',
}

/** 2 s while a download is moving, else 30 s. */
export const statusPollMs = (status: ItemStatus | null | undefined) => (status?.active ? 2_000 : 30_000)

export function stageActions(stages: StatusStage[] | undefined): StatusAction[] {
  const out: StatusAction[] = []
  for (const s of stages ?? []) for (const a of s.actions ?? []) if (!out.includes(a)) out.push(a)
  return out
}

export function fmtDuration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, '0')} min`
}

export function fmtWhen(ms: number | undefined, now = Date.now()): string {
  if (!ms) return ''
  const d = new Date(ms)
  if (now - ms > 20 * 3600_000) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
}

/** "6.3 of 18.4 GB · 48.2 MB/s · 3 min 48 s left", from Runner's numbers. */
export function downloadLine(dl: StatusDownload): string {
  const gb = (n: number) => (n / 1e9).toFixed(1)
  const parts = [dl.total > 0 ? `${gb(dl.bytes)} of ${gb(dl.total)} GB` : '']
  if (dl.rateBps) parts.push(`${(dl.rateBps / 1e6).toFixed(1)} MB/s`)
  if (dl.etaSeconds) parts.push(`${fmtDuration(dl.etaSeconds)} left`)
  if (dl.stage && dl.stage !== 'downloading') parts.push(`runner: ${dl.stage.replaceAll('_', ' ')}`)
  return parts.filter(Boolean).join(' · ')
}
