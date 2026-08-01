import type { MediaItemSummary, Rating } from './types'

export function posterUrl(path: string, size: 'w185' | 'w342' | 'w500' = 'w342'): string {
  if (!path) return ''
  return path.startsWith('http') ? path : `https://image.tmdb.org/t/p/${size}${path}`
}

export function formatBytes(bytes: number): string {
  if (bytes <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(value >= 100 || unit === 0 ? 0 : 1)} ${units[unit]}`
}

export function formatRating(rating: Rating): string {
  if (rating.scale === 100) return `${Math.round(rating.value)}%`
  if (rating.scale === 5) return `${rating.value.toFixed(1)}/5`
  return rating.value.toFixed(1)
}

export function formatUptime(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds))
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

export function formatRelative(iso: string, now = new Date()): string {
  const delta = Math.max(0, (now.getTime() - new Date(iso).getTime()) / 1000)
  if (delta < 60) return 'just now'
  if (delta < 3600) return `${Math.floor(delta / 60)}m ago`
  if (delta < 86400) return `${Math.floor(delta / 3600)}h ago`
  return `${Math.floor(delta / 86400)}d ago`
}

export function completeness(item: MediaItemSummary): { label: string; tone: 'ok' | 'warning' | 'neutral' } {
  if (item.kind === 'series') {
    if (item.episodeCount === 0) return { label: 'No aired episodes', tone: 'neutral' }
    if (item.episodeFileCount >= item.episodeCount) return { label: 'Complete', tone: 'ok' }
    return { label: `${item.episodeFileCount}/${item.episodeCount} episodes`, tone: 'warning' }
  }
  if (item.fileCount > 0) return { label: item.quality || 'On disk', tone: 'ok' }
  return { label: 'Missing', tone: 'warning' }
}

export function localDateKey(date: Date): string {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}
