import { ACTIVE_DOWNLOAD_STATES, type MediaItemSummary, type QueueItem, type Stage } from './api'
import { availabilityLabel } from './availability'

type CardStatus = { label: string; tone: string; icon: string }

function cardStatus(item: MediaItemSummary): CardStatus {
  const value = item.kind === 'book' ? undefined : item.availability
  if (!value) {
    return item.offloaded && item.kind !== 'series'
      ? { label: 'Offloaded', tone: 'offloaded', icon: '↗' }
      : { label: 'Unverified', tone: 'unknown', icon: '◷' }
  }
  if (value.catalog === 'ambiguous') return { label: 'Identity conflict', tone: 'missing', icon: '⊖' }
  if (value.catalog === 'unknown') return { label: 'Unverified', tone: 'unknown', icon: '◷' }
  if (value.catalog === 'absent') return { label: 'Not in library', tone: 'unknown', icon: '⊖' }
  switch (value.presence) {
    case 'present':
      return { label: item.kind === 'series' && value.expected > 0 ? `${value.present}/${value.expected} episodes available` : 'Available', tone: 'available', icon: '✓' }
    case 'partial':
      return { label: `${value.present}/${value.expected} episodes available`, tone: 'missing', icon: '⊖' }
    case 'missing':
      return { label: 'Absent at last check', tone: 'missing', icon: '⊖' }
    case 'offloaded':
      return { label: 'Offloaded', tone: 'offloaded', icon: '↗' }
    default:
      return { label: 'Unverified', tone: 'unknown', icon: '◷' }
  }
}

export function LibraryCardFooter({ item }: { item: MediaItemSummary }) {
  const status = cardStatus(item)
  const count = item.kind === 'series' ? item.episodeFileCount : item.fileCount
  const records = item.kind === 'series'
    ? `${count} ${count === 1 ? 'ep.' : 'eps'} recorded`
    : `${count} ${count === 1 ? 'record' : 'records'}`
  const metadata = item.kind === 'book'
    ? [item.author, ...(item.bookTypes ?? [item.bookType ?? 'ebook']).map(
        (edition) => edition === 'audiobook' ? 'Audiobook' : 'Ebook',
      )].filter(Boolean).join(' · ')
    : String(item.year || '—')
  const detail = item.kind === 'book' ? status.label : availabilityLabel(item.availability)
  const checked = item.availability?.checkedAt

  return (
    <div className={`poster-meta library-card-meta${item.kind === 'series' ? ' library-card-meta-series' : ''}`}>
      <div className="poster-title" title={item.title}>{item.title}</div>
      <div className="library-card-secondary">
        <span className="library-card-metadata" title={metadata}>{metadata}</span>
        <span className="library-card-records" title={item.kind === 'series' ? 'Monitored aired episodes with a recorded file; availability is checked separately' : 'Recorded files; availability is checked separately'}>
          <span aria-hidden="true"> · </span>{records}
        </span>
      </div>
      <div className={`library-card-status status-${status.tone}`} title={checked ? `${detail} · Last checked ${checked}` : detail}>
        <span className="library-card-status-icon" aria-hidden="true">{status.icon}</span>
        <span>{status.label}</span>
      </div>
    </div>
  )
}

const CARD_STAGE_LABELS: Record<Stage, string> = {
  downloading: 'Downloading', par_rename: 'Renaming', par_verify: 'Verifying',
  par_repair: 'Repairing', rar_rename: 'Naming archives', unpack: 'Extracting',
  cleanup: 'Cleaning up', move: 'Moving', post_unpack_rename: 'Renaming',
  publication_verify: 'Verifying publication', script: 'Running scripts',
  importing: 'Copying to library', notifying: 'Notifying', publication_cleanup: 'Cleaning up publication',
}

export function LibraryCardActivity({ downloads = [], active }: { downloads?: QueueItem[]; active: boolean }) {
  const running = downloads.filter((job) => ACTIVE_DOWNLOAD_STATES.includes(job.state)
    && job.importControl?.outcome !== 'cancelled' && job.importControl?.outcome !== 'completed')
  if (!running.length && (downloads.length || !active)) return null
  const download = running.length === 1 ? running[0] : undefined
  const cancelling = download?.importControl?.outcome === 'pending'
  const held = download?.control && (download.control.lifecycle === 'held' || download.control.version !== 1)
  const label = running.length > 1 ? `${running.length} active jobs`
    : cancelling ? 'Cancelling import'
    : held ? 'Held'
    : download?.importState === 'queued' ? 'Waiting to import'
    : download?.importState === 'running' && !download.stage ? 'Starting importer'
    : download?.stage ? CARD_STAGE_LABELS[download.stage]
    : download ? ({
    grabbed: 'Download queued',
    downloading: 'Downloading',
    downloaded: 'Downloaded',
    awaiting_import: 'Waiting to import',
    importing: 'Waiting to retry import',
  } as Record<string, string>)[download.state] ?? 'Work in progress' : 'Work in progress'
  const transfer = download?.state === 'downloading' && (!download.stage || download.stage === 'downloading')
    && !cancelling && !held && !download.importState
  const fraction = transfer && download
    ? typeof download.total === 'number' && download.total > 0 && typeof download.bytes === 'number'
      ? download.bytes / download.total
      : download.progress
    : undefined
  const percent = fraction !== undefined && Number.isFinite(fraction)
    ? Math.round(Math.min(1, Math.max(0, fraction)) * 100)
    : undefined

  return (
    <div className="library-card-activity">
      <span><span aria-hidden="true">↓ </span>{label}</span>
      {percent !== undefined && <>
        <span className="library-card-percent">{percent}%</span>
        <span className="library-card-progress" role="progressbar" aria-label="Download progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}>
          <span style={{ width: `${percent}%` }} />
        </span>
      </>}
    </div>
  )
}
