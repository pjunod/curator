// Hand-written client for the Phase 0 API surface. The OpenAPI spec
// (internal/api/openapi.yaml) is the source of truth; client codegen from the
// spec is planned once the surface grows (blueprint §7).

export interface SystemStatus {
  appName: string
  version: string
  commit: string
  goVersion: string
  os: string
  arch: string
  startedAt: string
  uptimeSeconds: number
  dataDir: string
  dbSchemaVersion: number
}

export type HealthStatus = 'ok' | 'warning' | 'error'

export interface HealthCheck {
  name: string
  status: HealthStatus
  message?: string
  checkedAt: string
}

export interface HealthReport {
  overall: HealthStatus
  checks: HealthCheck[]
}

export interface TaskState {
  name: string
  intervalSeconds: number
  running: boolean
  lastRunAt?: string
  lastDurationMs?: number
  lastError?: string
  nextRunAt?: string
}

export interface BusEvent {
  type: string
  ts: string
  payload: unknown
}

// ---- Phase 1: library ----

export type MediaKind = 'movie' | 'series' | 'book'

export interface MediaItemSummary {
  id: number
  kind: MediaKind
  title: string
  year: number
  posterPath: string
  monitored: boolean
  path: string
}

export interface ExternalIds {
  tmdb: number
  imdb?: string
  tvdb?: number
}

export interface EpisodeInfo {
  id: number
  seasonNumber: number
  episodeNumber: number
  title: string
  airDate: string
  monitored: boolean
  hasFile: boolean
}

export interface SeasonInfo {
  number: number
  monitored: boolean
  episodes: EpisodeInfo[]
}

export interface MediaFileInfo {
  id: number
  path: string
  size: number
  episodeIds: number[]
}

export interface MediaItemDetail extends MediaItemSummary {
  backdropPath: string
  overview: string
  genres: string[]
  status: string
  releaseDate: string
  runtime: number
  rootFolderId: number
  ended: boolean
  ids: ExternalIds
  seasons: SeasonInfo[]
  files: MediaFileInfo[]
  addedAt: string
}

export interface SearchResult {
  kind: MediaKind
  tmdbId: number
  title: string
  year: number
  overview: string
  posterPath: string
  inLibrary: boolean
}

export interface AddMediaRequest {
  kind: MediaKind
  tmdbId: number
  rootFolderId?: number
  monitored?: boolean
}

export interface RootFolder {
  id: number
  path: string
  freeBytes: number
  accessible: boolean
}

export interface UnmatchedDir {
  rootFolderId: number
  path: string
  name: string
}

export interface ScanReport {
  scannedAt: string
  rootsScanned: number
  itemsScanned: number
  filesLinked: number
  filesRemoved: number
  unmatchedDirs: UnmatchedDir[]
  missingPaths: string[]
}

export interface Settings {
  tmdbApiKeyConfigured: boolean
  tmdbApiKeyHint: string
}

async function parseError(res: Response, fallback: string): Promise<never> {
  let msg = fallback
  try {
    const body = (await res.json()) as { message?: string }
    if (body.message) msg = body.message
  } catch {
    /* keep fallback */
  }
  throw new Error(msg)
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`/api/v1${path}`)
  if (!res.ok) await parseError(res, `GET ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
}

async function send<T = void>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) await parseError(res, `${method} ${path}: ${res.status} ${res.statusText}`)
  if (res.status === 204 || res.status === 202) return undefined as T
  return res.json() as Promise<T>
}

export const getLibrary = (kind?: MediaKind) =>
  get<MediaItemSummary[]>(`/library${kind ? `?kind=${kind}` : ''}`)
export const getLibraryItem = (id: number) => get<MediaItemDetail>(`/library/${id}`)
export const addLibraryItem = (req: AddMediaRequest) =>
  send<MediaItemDetail>('POST', '/library', req)
export const deleteLibraryItem = (id: number) => send('DELETE', `/library/${id}`)
export const searchMetadata = (kind: MediaKind, query: string) =>
  get<SearchResult[]>(`/metadata/search?kind=${kind}&query=${encodeURIComponent(query)}`)
export const getRootFolders = () => get<RootFolder[]>('/rootfolders')
export const addRootFolder = (path: string) => send<RootFolder>('POST', '/rootfolders', { path })
export const deleteRootFolder = (id: number) => send('DELETE', `/rootfolders/${id}`)
export const getSettings = () => get<Settings>('/settings')
export const updateSettings = (patch: { tmdbApiKey?: string }) => send('PUT', '/settings', patch)
export const triggerScan = () => send('POST', '/library/scan')
export const getScanReport = async (): Promise<ScanReport | null> => {
  const res = await fetch('/api/v1/library/scan/report')
  if (res.status === 404) return null
  if (!res.ok) await parseError(res, `scan report: ${res.status}`)
  return res.json() as Promise<ScanReport>
}

// ---- Phase 2: acquisition ----

export interface QualityProfile {
  id: number
  name: string
  cutoff: string
  upgradesAllowed: boolean
  qualities: string[]
}

export interface IndexerInput {
  name: string
  url: string
  apiKey?: string
  protocol: 'torrent' | 'usenet'
  categories?: number[]
  enabled?: boolean
}

export interface Indexer extends IndexerInput {
  id: number
}

export interface DownloadClientInput {
  type: 'qbittorrent' | 'sabnzbd'
  name: string
  url: string
  username?: string
  password?: string
  category?: string
  enabled?: boolean
}

export interface DownloadClientConfig extends DownloadClientInput {
  id: number
}

export interface Rejection {
  code: string
  reason: string
}

export interface ReleaseCandidate {
  title: string
  downloadUrl: string
  infoUrl?: string
  indexer: string
  protocol: string
  size: number
  seeders: number
  age: string
  quality: string
  accepted: boolean
  isUpgrade: boolean
  rejections: Rejection[]
}

export interface GrabRequest {
  mediaItemId: number
  season?: number
  episode?: number
  title: string
  downloadUrl: string
  indexer?: string
  protocol: string
  size?: number
}

export interface QueueItem {
  id: number
  mediaItemId: number
  title: string
  state: string
  progress: number
  protocol: string
  quality: string
  error?: string
  addedAt: string
}

export const getProfiles = () => get<QualityProfile[]>('/profiles')
export const getIndexers = () => get<Indexer[]>('/indexers')
export const addIndexer = (i: IndexerInput) => send<Indexer>('POST', '/indexers', i)
export const testIndexer = (i: IndexerInput) => send('POST', '/indexers/test', i)
export const deleteIndexer = (id: number) => send('DELETE', `/indexers/${id}`)
export const getDownloadClients = () => get<DownloadClientConfig[]>('/downloadclients')
export const addDownloadClient = (c: DownloadClientInput) =>
  send<DownloadClientConfig>('POST', '/downloadclients', c)
export const testDownloadClient = (c: DownloadClientInput) => send('POST', '/downloadclients/test', c)
export const deleteDownloadClient = (id: number) => send('DELETE', `/downloadclients/${id}`)
export const searchReleases = (itemId: number, season?: number, episode?: number) => {
  const p = new URLSearchParams()
  if (season !== undefined) p.set('season', String(season))
  if (episode !== undefined) p.set('episode', String(episode))
  const qs = p.toString()
  return get<ReleaseCandidate[]>(`/library/${itemId}/releases${qs ? `?${qs}` : ''}`)
}
export const grabRelease = (req: GrabRequest) => send<{ id: number }>('POST', '/grab', req)
export const getQueue = () => get<QueueItem[]>('/queue')
export const removeQueueItem = (id: number, fromClient: boolean) =>
  send('DELETE', `/queue/${id}?fromClient=${fromClient}`)

export function posterUrl(path: string, size: 'w185' | 'w342' | 'w500' = 'w342'): string {
  return path ? `https://image.tmdb.org/t/p/${size}${path}` : ''
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

export const getStatus = () => get<SystemStatus>('/system/status')
export const getHealth = () => get<HealthReport>('/health')
export const getTasks = () => get<TaskState[]>('/system/tasks')

export async function runTask(name: string): Promise<void> {
  const res = await fetch(`/api/v1/system/tasks/${encodeURIComponent(name)}/run`, {
    method: 'POST',
  })
  if (!res.ok) throw new Error(`run ${name}: ${res.status} ${res.statusText}`)
}

// ---- formatting helpers ----

export function fmtDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return `${d}d ${h}h ${m}m`
  if (h > 0) return `${h}h ${m}m ${sec}s`
  if (m > 0) return `${m}m ${sec}s`
  return `${sec}s`
}

export function fmtRelative(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  const diff = (t - now.getTime()) / 1000
  const abs = Math.abs(diff)
  const fmt = fmtDuration(abs)
  if (abs < 1) return 'now'
  return diff < 0 ? `${fmt} ago` : `in ${fmt}`
}

export function fmtInterval(seconds: number): string {
  if (seconds % 3600 === 0) {
    const h = seconds / 3600
    return h === 1 ? 'every hour' : `every ${h}h`
  }
  if (seconds % 60 === 0) {
    const m = seconds / 60
    return m === 1 ? 'every minute' : `every ${m}m`
  }
  return `every ${seconds}s`
}
