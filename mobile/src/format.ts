import type { BookType, CalendarEntry, MediaItemSummary, Rating } from './types'

const EBOOK_SOURCES = new Set(['pdf', 'mobi', 'azw3', 'epub'])
const AUDIOBOOK_SOURCES = new Set(['mp3', 'wma', 'aac', 'ogg', 'opus', 'm4a', 'm4b', 'flac', 'wav'])

export function bookTypeForSource(source: string): BookType | undefined {
  if (EBOOK_SOURCES.has(source)) return 'ebook'
  if (AUDIOBOOK_SOURCES.has(source)) return 'audiobook'
  return undefined
}

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

/** parseDateKey reads YYYY-MM-DD as LOCAL midnight. `new Date('2020-01-01')`
 *  is UTC midnight and lands a day early anywhere west of Greenwich. */
export function parseDateKey(key: string): Date {
  const [year, month, day] = key.slice(0, 10).split('-').map(Number)
  return new Date(year ?? 1970, (month ?? 1) - 1, day ?? 1)
}

export function addDays(date: Date, days: number): Date {
  const out = new Date(date)
  out.setDate(out.getDate() + days)
  return out
}

/** Within one day: date-only entries first, then timed ones in time order —
 *  identical to the web agenda's rule, so the same library reads the same
 *  way on both. Ties break on title, then season and episode. */
export function compareCalendarEntries(a: CalendarEntry, b: CalendarEntry): number {
  const at = a.airDateUtc ? Date.parse(a.airDateUtc) : NaN
  const bt = b.airDateUtc ? Date.parse(b.airDateUtc) : NaN
  const aTimed = !Number.isNaN(at)
  const bTimed = !Number.isNaN(bt)
  if (aTimed !== bTimed) return aTimed ? 1 : -1
  if (aTimed && bTimed && at !== bt) return at - bt
  if (a.title !== b.title) return a.title < b.title ? -1 : 1
  return (a.seasonNumber ?? 0) - (b.seasonNumber ?? 0) || (a.episodeNumber ?? 0) - (b.episodeNumber ?? 0)
}

/** calendarDayLabel names a day the way a person would. The year appears
 *  only when it is not the current one. */
export function calendarDayLabel(key: string, now = new Date()): string {
  const date = parseDateKey(key)
  const long = new Intl.DateTimeFormat(undefined, {
    weekday: 'long',
    month: 'short',
    day: 'numeric',
    ...(date.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {}),
  }).format(date)
  if (key === localDateKey(now)) return `Today · ${long}`
  if (key === localDateKey(addDays(now, 1))) return `Tomorrow · ${long}`
  if (key === localDateKey(addDays(now, -1))) return `Yesterday · ${long}`
  return long
}

/** calendarTime renders an air instant in the reader's local zone, minutes
 *  dropped on the hour. Empty when there is no instant — the row then simply
 *  has no time, which is the honest rendering of an unknown schedule. */
export function calendarTime(airDateUtc?: string, timeZone?: string): string {
  if (!airDateUtc) return ''
  const at = new Date(airDateUtc)
  if (Number.isNaN(at.getTime())) return ''
  const parts = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit', timeZone }).formatToParts(at)
  const get = (type: string) => parts.find((part) => part.type === type)?.value ?? ''
  const minute = get('minute')
  const clock = minute === '00' ? get('hour') : `${get('hour')}:${minute}`
  const period = get('dayPeriod')
  return period ? `${clock} ${period}` : clock
}

/** The row's second line. Falls back to `detail` when the server is older
 *  than the structured fields. */
export function calendarSubtitle(entry: CalendarEntry): string {
  const bits: string[] = []
  if (entry.kind === 'episode') {
    const code =
      entry.seasonNumber !== undefined && entry.episodeNumber !== undefined
        ? `S${String(entry.seasonNumber).padStart(2, '0')}E${String(entry.episodeNumber).padStart(2, '0')}`
        : ''
    bits.push(entry.episodeTitle ? `${code} — ${entry.episodeTitle}` : code || entry.detail)
  } else {
    bits.push(entry.kind === 'book' ? 'Book' : 'Movie')
    if (entry.detail) bits.push(entry.detail)
  }
  if (entry.runtime) bits.push(`${entry.runtime} min`)
  return bits.filter(Boolean).join(' · ')
}

/** The status badge: "Missing" is a claim about something that has already
 *  happened, so an episode airing tonight reads Upcoming, not Missing. */
export function calendarStatus(entry: CalendarEntry, now = new Date()): { label: string; tone: 'ok' | 'warning' | 'neutral' } {
  if (entry.hasFile) return { label: 'On disk', tone: 'ok' }
  const aired = entry.airDateUtc
    ? new Date(entry.airDateUtc).getTime() <= now.getTime()
    : addDays(parseDateKey(entry.date), 1).getTime() <= now.getTime()
  return aired ? { label: 'Missing', tone: 'warning' } : { label: 'Upcoming', tone: 'neutral' }
}

/** groupCalendar buckets entries onto days, sorted, each day's rows ordered
 *  by compareCalendarEntries. */
export function groupCalendar(entries: CalendarEntry[]): { title: string; key: string; data: CalendarEntry[] }[] {
  const byDay = new Map<string, CalendarEntry[]>()
  for (const entry of entries) {
    const key = entry.date.slice(0, 10)
    const list = byDay.get(key)
    if (list) list.push(entry)
    else byDay.set(key, [entry])
  }
  return [...byDay.keys()].sort().map((key) => ({
    key,
    title: calendarDayLabel(key),
    data: (byDay.get(key) ?? []).slice().sort(compareCalendarEntries),
  }))
}
