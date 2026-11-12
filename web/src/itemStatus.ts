import type { ItemStatus, StatusAction, StatusDownload, StatusRematch, StatusStage, StatusStageKey, StatusState } from './api'

// Display helpers for the item page's status panel. The server decides every
// state; these only turn its numbers into words.

export const STAGE_NAMES: Record<StatusStageKey, string> = {
  searched: 'Searched',
  downloading: 'Downloading',
  processed: 'Processed',
  moved: 'Moved',
  cinema_notified: 'Cinema notified',
  in_cinema: 'In cinema',
}

export const STATE_ICON: Record<StatusState, string> = {
  done: '✓',
  active: '●',
  waiting: '◷',
  failed: '✕',
  todo: '○',
}

/** CSS class for a state, shared with the proposal's palette. */
export const stateClass = (state: StatusState) =>
  ({ done: 's-done', active: 's-active', waiting: 's-wait', failed: 's-fail', todo: 's-todo' })[state]

/** Polling cadence: 2 s while a download is moving, else 30 s. */
export const statusPollMs = (status: ItemStatus | undefined) => (status?.active ? 2_000 : 30_000)

export function fmtBytes(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(1)} GB`
  if (n >= 1e6) return `${Math.round(n / 1e6)} MB`
  if (n >= 1e3) return `${Math.round(n / 1e3)} KB`
  return `${n} B`
}

export function fmtRate(bps: number): string {
  return `${(bps / 1e6).toFixed(1)} MB/s`
}

export function fmtDuration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, '0')} min`
}

/** A stage time: the clock today, the date otherwise. */
export function fmtWhen(ms: number | undefined, now: Date = new Date()): string {
  if (!ms) return ''
  const d = new Date(ms)
  if (now.getTime() - d.getTime() > 20 * 3600_000) {
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
}

/** When the fetch should finish, from Runner's own ETA. */
export function doneAbout(dl: StatusDownload, now: Date = new Date()): string {
  if (!dl.etaSeconds) return ''
  return fmtWhen(now.getTime() + dl.etaSeconds * 1000, now)
}

export const ACTION_LABELS: Record<StatusAction, string> = {
  search: 'Search now',
  interactive_search: 'Interactive search',
  open_activity: 'Open in Activity',
  cancel_download: 'Cancel download',
  retry_import: 'Retry import',
  resend_cinema: 'Send to cinema again',
  confirm_cinema: 'Ask cinema to confirm',
}

/** Every action the stages offer, once each, in stage order. */
export function stageActions(stages: StatusStage[] | undefined): StatusAction[] {
  const out: StatusAction[] = []
  for (const s of stages ?? []) {
    for (const a of s.actions ?? []) if (!out.includes(a)) out.push(a)
  }
  return out
}

export function updatedAgo(at: number, now: number = Date.now()): string {
  const s = Math.max(0, Math.round((now - at) / 1000))
  return s < 60 ? `updated ${s} s ago` : `updated ${Math.round(s / 60)} min ago`
}

export const REMATCH_STATE: Record<StatusRematch['state'], string> = {
  planned: 'Starting',
  moving: 'Moving',
  moved: 'Files moved; committing',
  committed: 'Files in place; finishing',
  cancel_pending: 'Stopping; putting the moved files back',
  complete: 'Complete',
  cancelled: 'Stopped',
  failed: 'Failed',
}

/** "1 of 2 files" and, when bytes are known, "5 MB of 10 MB". */
export function rematchCounts(r: Pick<StatusRematch, 'filesDone' | 'filesTotal' | 'bytesDone' | 'bytesTotal'>): string[] {
  const out = [`${r.filesDone} of ${r.filesTotal} file${r.filesTotal === 1 ? '' : 's'}`]
  if (r.bytesTotal > 0) out.push(`${fmtBytes(r.bytesDone)} of ${fmtBytes(r.bytesTotal)}`)
  return out
}

/** A Stop/Dismiss answer belongs to the operation it was given for: once a different one opens, it goes. */
export const noticeBelongsTo = (noticeOp: string, currentOp: string | undefined) => !currentOp || !noticeOp || currentOp === noticeOp
