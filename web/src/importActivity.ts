import type { ImportActivityItem } from './api'

export function importNeedsReview({ download: d }: ImportActivityItem) {
  const c = d.importControl
  return !!(c?.pendingPublications || c?.retainedPublications || c?.heldStages)
}

export function canRetryImport(item: ImportActivityItem) {
  const d = item.download
  return d.id > 0 && item.category === 'attention' && !d.importState && !importNeedsReview(item) &&
    d.importControl?.outcome !== 'pending' && d.submissionPhase !== 'uncertain' &&
    !(d.control && (d.control.version !== 1 || d.control.lifecycle === 'held')) &&
    !!(d.importPath || d.savePath) &&
    !(d.publications ?? []).some(p => p.files.some(f => f.outcome === 'unresolved'))
}

export function importActivityLabel(item: ImportActivityItem) {
  const d = item.download
  if (d.importControl?.outcome === 'pending') return 'Cancelling'
  if (item.category === 'active') return d.stageDetail || 'Starting importer'
  if (item.category === 'queued') return d.importState === 'queued' ? 'Waiting for an import worker' : 'Waiting for import admission'
  if (importNeedsReview(item)) return 'Review required'
  if (item.category === 'imported') return 'Imported'
  if (d.control && (d.control.version !== 1 || d.control.lifecycle === 'held')) return d.control.message || 'Runner hold requires review'
  if (d.importControl?.outcome === 'cancelled') return item.category === 'history' ? 'Cancelled' : 'Cancelled · review required'
  if (d.state === 'importing' && !d.importState) return 'Interrupted · no importer is running'
  if (d.state === 'awaiting_import') return 'Awaiting import approval'
  if (d.state === 'failed') return 'Import failed'
  return 'Review file outcomes before retrying'
}

export function importProgress(item: ImportActivityItem) {
  const d = item.download
  // Download percentages and publication sizes are not copy measurements.
  if ((item.category !== 'active' && item.category !== 'progress') || d.stage !== 'importing' || !d.total || d.bytes === undefined) return null
  const bytes = Math.max(0, Math.min(d.bytes, d.total))
  const rate = d.bytesPerSecond && d.bytesPerSecond > 0 ? d.bytesPerSecond : null
  return { bytes, total: d.total, percent: Math.round(bytes / d.total * 100), rate,
    secondsLeft: rate ? Math.ceil((d.total - bytes) / rate) : null }
}

// The download keeps the original failure after publication cleanup. Show why
// separately from the short state pill, including after the files have left.
export function importActivityReason({ download: d }: ImportActivityItem) {
  const failure = d.error || (d.publications ?? []).find(p => p.error)?.error
  if (failure) return failure.replace(/^publication import_failed:\s*/, '')
  if (d.importControl?.heldStages) return 'An interrupted import left scratch files. Its retained-stage records are shown under Details.'
  return null
}
